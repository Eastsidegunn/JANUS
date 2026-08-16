// Package sqlite는 core/logd.Store의 SQLite(WAL) 구현이다 (NFR-03).
// modernc.org/sqlite(순수 Go)를 쓰며, 이 모듈의 import는 boundarylint가
// 이 패키지로만 한정한다 (docs/t3-sqlite-driver-proposal.md 승인 조건).
//
// 설계 근거는 전부 T3 제안서의 실증에서 온다:
//   - journal_mode=WAL은 초기화 커넥션 하나에서 순차 1회 설정
//     (동시 open 시 SQLITE_BUSY_RECOVERY(261) 경합 실증).
//   - busy_timeout=0 + Go 계층 context-aware 재시도
//     (busy_timeout=5000이 50ms deadline을 5초 블록하는 충돌 실증).
//   - synchronous=FULL — 커밋마다 WAL fsync (NFR-02).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlite3 "modernc.org/sqlite"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
)

// 컴파일 타임 계약 확인.
var _ logd.Store = (*Store)(nil)

// schemaDDL은 §5.1 events 테이블과 FR-LOG-01의 물리 차단 트리거다.
// UPDATE/DELETE는 스키마 마이그레이션 외에 금지되며 트리거가 차단한다.
var schemaDDL = []string{
	`CREATE TABLE IF NOT EXISTS events (
		seq            INTEGER PRIMARY KEY,
		ts             INTEGER NOT NULL,
		trace_id       TEXT NOT NULL,
		span_id        TEXT NOT NULL,
		parent_span_id TEXT,
		kind           TEXT NOT NULL,
		actor          TEXT NOT NULL,
		payload        TEXT NOT NULL,
		raw            BLOB,
		usage_in       INTEGER,
		usage_out      INTEGER
	)`,
	`CREATE TRIGGER IF NOT EXISTS events_no_update BEFORE UPDATE ON events
		BEGIN SELECT RAISE(ABORT, 'append-only: UPDATE 금지 (FR-LOG-01)'); END`,
	`CREATE TRIGGER IF NOT EXISTS events_no_delete BEFORE DELETE ON events
		BEGIN SELECT RAISE(ABORT, 'append-only: DELETE 금지 (FR-LOG-01)'); END`,
}

// Store는 세션 하나의 append-only 이벤트 로그 파일이다.
type Store struct {
	write *sql.DB // 단일 커넥션 — writer 전용 (FR-LOG-02)
	read  *sql.DB // 읽기 전용
}

// Open은 세션 로그 파일을 열고(없으면 생성) 스키마·트리거를 설치한다.
func Open(path string) (*Store, error) {
	// 초기화: 전용 커넥션 하나에서 순차 1회 (실증된 261 경합 회피)
	init, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, err
	}
	init.SetMaxOpenConns(1)
	var mode string
	if err := init.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		init.Close()
		return nil, fmt.Errorf("sqlite: WAL 설정: %w", err)
	}
	if mode != "wal" {
		init.Close()
		return nil, fmt.Errorf("sqlite: journal_mode=%s (wal 기대)", mode)
	}
	for _, ddl := range schemaDDL {
		if _, err := init.Exec(ddl); err != nil {
			init.Close()
			return nil, fmt.Errorf("sqlite: 스키마 설치: %w", err)
		}
	}
	if err := init.Close(); err != nil {
		return nil, err
	}

	// 검증형 DSN 파라미터 우선 (제안서 §5). busy_timeout=0 — 재시도는 Go 계층.
	write, err := sql.Open("sqlite", "file:"+path+"?_synchronous=FULL&_busy_timeout=0&_foreign_keys=1")
	if err != nil {
		return nil, err
	}
	write.SetMaxOpenConns(1)
	read, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_busy_timeout=0")
	if err != nil {
		write.Close()
		return nil, err
	}
	return &Store{write: write, read: read}, nil
}

func (s *Store) Close() error {
	rerr := s.read.Close()
	werr := s.write.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

// LastSeq는 마지막으로 커밋된 seq를 반환한다. 빈 로그면 0.
func (s *Store) LastSeq(ctx context.Context) (int64, error) {
	var last int64
	err := retryBusy(ctx, func() error {
		return s.read.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&last)
	})
	return last, err
}

// Append는 rec을 내구 커밋한다 — synchronous=FULL이므로 반환 시점에
// WAL이 fsync되어 있다(NFR-02). raw는 base64를 디코드해 BLOB으로 보존한다
// (FR-LOG-07 — 정규화 이벤트와 원본 페이로드 동시 보존).
func (s *Store) Append(ctx context.Context, rec gen.EventRecord) error {
	raw, err := decodeRaw(rec.Raw)
	if err != nil {
		return fmt.Errorf("sqlite: raw 디코드: %w", err)
	}
	return retryBusy(ctx, func() error {
		_, err := s.write.ExecContext(ctx,
			`INSERT INTO events (seq, ts, trace_id, span_id, parent_span_id, kind, actor, payload, raw, usage_in, usage_out)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.Seq, rec.Ts, rec.TraceID, rec.SpanID, rec.ParentSpanID,
			string(rec.Kind), rec.Actor, string(rec.Payload), raw,
			rec.UsageIn, rec.UsageOut)
		return err
	})
}

// ReadFrom은 fromSeq 이상(포함)의 이벤트를 seq 오름차순으로 반환한다.
func (s *Store) ReadFrom(ctx context.Context, fromSeq int64) ([]gen.EventRecord, error) {
	var out []gen.EventRecord
	err := retryBusy(ctx, func() error {
		rows, err := s.read.QueryContext(ctx,
			`SELECT seq, ts, trace_id, span_id, parent_span_id, kind, actor, payload, raw, usage_in, usage_out
			 FROM events WHERE seq >= ? ORDER BY seq`, fromSeq)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			var rec gen.EventRecord
			var kind, payload string
			var raw sql.Null[[]byte]
			if err := rows.Scan(&rec.Seq, &rec.Ts, &rec.TraceID, &rec.SpanID,
				&rec.ParentSpanID, &kind, &rec.Actor, &payload, &raw,
				&rec.UsageIn, &rec.UsageOut); err != nil {
				return err
			}
			rec.Kind = gen.Kind(kind)
			rec.Payload = []byte(payload)
			rec.Raw = encodeRaw(raw)
			out = append(out, rec)
		}
		return rows.Err()
	})
	return out, err
}

// IsBusy는 저장소 잠금 경합(SQLITE_BUSY/LOCKED 계열) 여부다.
// 이것은 재시도 대상이며 FR-LOG-09 백프레셔(writer 큐 포화)와 별개 상태다.
func IsBusy(err error) bool {
	var se *sqlite3.Error
	if !errors.As(err, &se) {
		return false
	}
	primary := se.Code() & 0xff
	return primary == 5 || primary == 6 // SQLITE_BUSY, SQLITE_LOCKED (확장 코드 포함)
}

// retryBusy는 BUSY 계열 오류를 context-aware 백오프로 재시도한다.
// busy_timeout=0이므로 잠금 경합은 즉시 표면화되고, deadline이 짧으면
// 짧게 반환된다(제안서 §5 실증 2번 — SQLite busy handler의 5초 블록 회피).
func retryBusy(ctx context.Context, fn func() error) error {
	backoff := time.Millisecond
	for {
		err := fn()
		if err == nil || !IsBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 32*time.Millisecond {
			backoff *= 2
		}
	}
}
