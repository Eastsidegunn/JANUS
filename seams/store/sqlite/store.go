// Package sqlite는 core/logd.Store의 SQLite(WAL) 구현이다 (NFR-03).
// modernc.org/sqlite(순수 Go)를 쓰며, 이 모듈의 import는 boundarylint가
// 이 패키지로만 한정한다 (docs/t3-sqlite-driver-proposal.md 승인 조건).
//
// mutation 객체(store)는 패키지 밖으로 노출되지 않는다 — 외부에 보이는
// 표면은 Log(유일한 쓰기 경로인 logd.Writer + 읽기 전용 Reader)뿐이다.
// 임의 seq의 직접 Append나 redaction·검증 우회는 타입 수준에서 불가능하다.
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
	"net/url"
	"strings"
	"time"

	sqlite3 "modernc.org/sqlite"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
)

// 컴파일 타임 계약 확인.
var _ logd.Store = (*store)(nil)

// Log는 세션 로그의 공개 표면이다. 쓰기는 Writer(단일 writer, redaction,
// contracts 검증 경유)로만, 읽기는 Reader로만 가능하다.
type Log struct {
	Writer *logd.Writer
	Reader logd.Reader
	st     *store
}

// Open은 세션 로그 파일을 열고(없으면 생성) 조립된 Log를 반환한다.
func Open(ctx context.Context, path string, opts ...logd.Option) (*Log, error) {
	st, err := openStore(ctx, path)
	if err != nil {
		return nil, err
	}
	w, err := logd.NewWriter(ctx, st, opts...)
	if err != nil {
		st.Close()
		return nil, err
	}
	return &Log{Writer: w, Reader: st, st: st}, nil
}

// Close는 writer를 드레인한 뒤 store를 닫는다.
func (l *Log) Close() error {
	werr := l.Writer.Close()
	serr := l.st.Close()
	if werr != nil {
		return werr
	}
	return serr
}

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

// store는 세션 하나의 append-only 이벤트 로그 파일이다. 패키지 비공개 —
// 이 타입이 밖으로 나가면 단일 writer·redaction·검증이 전부 우회된다.
type store struct {
	write *sql.DB // 단일 커넥션 — writer 전용 (FR-LOG-02)
	read  *sql.DB // 읽기 전용
}

// fileDSN은 경로를 SQLite URI로 안전하게 인코딩한다. 합법적인 유닉스
// 파일명에 들어갈 수 있는 URI 특수문자(%, ?, #)와 공백을 percent-encode
// 하지 않으면 경로 일부가 DSN 옵션으로 해석된다(리뷰 실증:
// "session?mode=memory"가 in-memory 모드로 열림).
func fileDSN(path, query string) string {
	esc := strings.NewReplacer(
		"%", "%25", // 반드시 먼저
		"?", "%3F",
		"#", "%23",
		" ", "%20",
	).Replace(path)
	u := url.URL{Scheme: "file", Opaque: esc, RawQuery: query}
	return u.String()
}

// openStore는 스키마·트리거를 설치하고 쓰기·읽기 핸들을 연다.
// sql.Open은 lazy이므로 두 핸들 모두 Ping까지 성공해야 반환한다.
func openStore(ctx context.Context, path string) (*store, error) {
	// 초기화: 전용 커넥션 하나에서 순차 1회 (실증된 261 경합 회피)
	init, err := sql.Open("sqlite", fileDSN(path, ""))
	if err != nil {
		return nil, err
	}
	init.SetMaxOpenConns(1)
	var mode string
	if err := init.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		init.Close()
		return nil, fmt.Errorf("sqlite: WAL 설정: %w", err)
	}
	if mode != "wal" {
		init.Close()
		return nil, fmt.Errorf("sqlite: journal_mode=%s (wal 기대)", mode)
	}
	for _, ddl := range schemaDDL {
		if _, err := init.ExecContext(ctx, ddl); err != nil {
			init.Close()
			return nil, fmt.Errorf("sqlite: 스키마 설치: %w", err)
		}
	}
	if err := init.Close(); err != nil {
		return nil, err
	}

	// 검증형 DSN 파라미터 우선 (제안서 §5). busy_timeout=0 — 재시도는 Go 계층.
	write, err := sql.Open("sqlite", fileDSN(path, "_synchronous=FULL&_busy_timeout=0&_foreign_keys=1"))
	if err != nil {
		return nil, err
	}
	write.SetMaxOpenConns(1)
	if err := write.PingContext(ctx); err != nil {
		write.Close()
		return nil, fmt.Errorf("sqlite: 쓰기 핸들 ping: %w", err)
	}
	read, err := sql.Open("sqlite", fileDSN(path, "mode=ro&_busy_timeout=0"))
	if err != nil {
		write.Close()
		return nil, err
	}
	if err := read.PingContext(ctx); err != nil {
		write.Close()
		read.Close()
		return nil, fmt.Errorf("sqlite: 읽기 핸들 ping: %w", err)
	}
	return &store{write: write, read: read}, nil
}

func (s *store) Close() error {
	rerr := s.read.Close()
	werr := s.write.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

// LastSeq는 마지막으로 커밋된 seq를 반환한다. 빈 로그면 0.
func (s *store) LastSeq(ctx context.Context) (int64, error) {
	var last int64
	err := retryBusy(ctx, func() error {
		return s.read.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&last)
	})
	return last, err
}

// Append는 rec을 내구 커밋한다 — synchronous=FULL이므로 반환 시점에
// WAL이 fsync되어 있다(NFR-02). raw는 base64를 디코드해 BLOB으로 보존한다
// (FR-LOG-07 — 정규화 이벤트와 원본 페이로드 동시 보존).
func (s *store) Append(ctx context.Context, rec gen.EventRecord) error {
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
func (s *store) ReadFrom(ctx context.Context, fromSeq int64) ([]gen.EventRecord, error) {
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
