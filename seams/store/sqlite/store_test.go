package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// openTestStore는 화이트박스 테스트용 내부 store다. 공개 표면(Log)은
// mutation 객체를 노출하지 않으므로, 저장 계층 자체의 성질(트리거, BUSY,
// 내구성)은 패키지 내부에서만 검증할 수 있다.
func openTestStore(t *testing.T) (*store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.db")
	s, err := openStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func rec(seq int64, payload string) gen.EventRecord {
	return gen.EventRecord{
		Seq: seq, Ts: 1, TraceID: strings.Repeat("a", 32),
		SpanID: strings.Repeat("b", 16), Kind: gen.KindUserMessage,
		Actor: "parent", Payload: []byte(payload),
	}
}

func ptr[T any](v T) *T { return &v }

// FR-LOG-01: UPDATE/DELETE는 저장소 수준(트리거)에서 물리적으로 차단된다.
// writer 경유가 아닌 직접 연결로 시도해도 막혀야 "물리적" 차단이다.
func TestAppendOnlyTriggers(t *testing.T) {
	s, path := openTestStore(t)
	ctx := context.Background()
	if err := s.Append(ctx, rec(1, `{"a":1}`)); err != nil {
		t.Fatal(err)
	}

	direct, err := sql.Open("sqlite", fileDSN(path, "_busy_timeout=0"))
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	if _, err := direct.Exec(`UPDATE events SET payload='변조' WHERE seq=1`); err == nil {
		t.Fatal("직접 연결의 UPDATE가 차단되지 않음")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE 차단 사유가 트리거가 아님: %v", err)
	}
	if _, err := direct.Exec(`DELETE FROM events`); err == nil {
		t.Fatal("직접 연결의 DELETE가 차단되지 않음")
	}
	got, err := s.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0].Payload) != `{"a":1}` {
		t.Fatalf("원본 훼손: %+v", got)
	}
}

// NFR-02/03: WAL + synchronous=FULL이 쓰기 풀 커넥션에 실제 적용된다.
func TestDurabilityPragmas(t *testing.T) {
	s, _ := openTestStore(t)
	var jm string
	var syn, bt int
	if err := s.write.QueryRow("PRAGMA journal_mode").Scan(&jm); err != nil {
		t.Fatal(err)
	}
	s.write.QueryRow("PRAGMA synchronous").Scan(&syn)
	s.write.QueryRow("PRAGMA busy_timeout").Scan(&bt)
	if jm != "wal" || syn != 2 || bt != 0 {
		t.Fatalf("journal_mode=%s synchronous=%d busy_timeout=%d (wal/2/0 기대)", jm, syn, bt)
	}
}

// raw(FR-LOG-07)와 옵셔널 필드의 왕복 보존.
func TestRoundTrip(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	e := rec(1, `{"x":"y"}`)
	e.ParentSpanID = ptr(strings.Repeat("c", 16))
	e.Raw = ptr("aGVsbG8=")
	e.UsageIn = ptr(int64(10))
	e.UsageOut = ptr(int64(20))
	if err := s.Append(ctx, e); err != nil {
		t.Fatal(err)
	}
	synthetic := rec(2, `{}`)
	synthetic.Raw = ptr("") // 합성 이벤트의 빈 base64
	if err := s.Append(ctx, synthetic); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadFrom(ctx, 1)
	if err != nil || len(got) != 2 {
		t.Fatalf("ReadFrom: %v, %d건", err, len(got))
	}
	g := got[0]
	if g.Raw == nil || *g.Raw != "aGVsbG8=" || g.ParentSpanID == nil || *g.UsageIn != 10 || *g.UsageOut != 20 {
		t.Fatalf("왕복 훼손: %+v", g)
	}
	if got[1].Raw == nil || *got[1].Raw != "" {
		t.Fatalf("빈 raw 왕복 훼손: %+v", got[1].Raw)
	}
	if got[0].Kind != gen.KindUserMessage {
		t.Fatalf("kind 훼손: %s", got[0].Kind)
	}
}

// 리뷰 차단 5의 회귀: URI 특수문자(?, #, %, 공백)가 든 합법적 파일명이
// DSN 옵션으로 해석되지 않고 정확히 그 이름의 파일로 열린다.
func TestWeirdFilenames(t *testing.T) {
	names := []string{
		"session?mode=memory", // 리뷰 실증 — 인코딩 없으면 in-memory로 열림
		"a b#c%d.db",
		"100%?done#1 .db",
	}
	ctx := context.Background()
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			l, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := l.Writer.Submit(ctx, rec(0, `{"n":1}`)); err != nil {
				t.Fatal(err)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("정확한 이름의 파일이 없음: %v", err)
			}
			// 재오픈 후 내용 확인
			l2, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer l2.Close()
			last, err := l2.Reader.LastSeq(ctx)
			if err != nil || last != 1 {
				t.Fatalf("재오픈 LastSeq=%d err=%v", last, err)
			}
		})
	}
}

// FR-LOG-02: 공개 표면(Log) 경유 동시 쓰기 — seq 전순서와 유실 0.
// mutation 객체는 노출되지 않으므로 임의 seq의 직접 Append는 컴파일
// 수준에서 불가능하다. 저장소 측 최후 방어(중복 seq PRIMARY KEY 거부)는
// 직접 파일 연결로 확인한다.
func TestSingleWriterOverSQLite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.db")
	ctx := context.Background()
	l, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Writer.Submit(ctx, rec(0, `{"c":1}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := l.Reader.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("%d건 저장 (%d건 기대)", len(got), n)
	}
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq 구멍/역전: index %d에 %d", i, e.Seq)
		}
	}
	// 저장소 최후 방어: 파일 직접 연결로 기존 seq에 INSERT → PK 거부
	direct, err := sql.Open("sqlite", fileDSN(path, "_busy_timeout=0"))
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	if _, err := direct.Exec(`INSERT INTO events (seq, ts, trace_id, span_id, kind, actor, payload)
		VALUES (1, 1, 'x', 'y', 'user/message', 'parent', '{}')`); err == nil {
		t.Fatal("중복 seq 직접 INSERT가 성공함")
	}
}

// 공개 표면 경유 시 redaction·contracts 검증이 우회 불가능함을 고정한다
// (리뷰 차단 1·2의 공개 API 측 회귀).
func TestLogPathEnforcesRedactionAndContract(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	l, err := Open(ctx, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// redaction: 토큰이 저장 전에 마스킹된다
	ev := rec(0, `{"token":"sk-abcdefghijklmnopqrstuvwxyz"}`)
	if _, err := l.Writer.Submit(ctx, ev); err != nil {
		t.Fatal(err)
	}
	got, err := l.Reader.ReadFrom(ctx, 1)
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	if strings.Contains(string(got[0].Payload), "sk-abcdefghijklmnop") {
		t.Fatal("미마스킹 토큰이 저장됨 — redaction 우회")
	}

	// contracts 위반: 미지의 kind는 거부되고 저장되지 않는다
	bad := rec(0, `{}`)
	bad.Kind = gen.Kind("session/pause")
	if _, err := l.Writer.Submit(ctx, bad); err == nil {
		t.Fatal("계약 위반 이벤트가 ack됨")
	}
	if last, _ := l.Reader.LastSeq(ctx); last != 1 {
		t.Fatalf("위반 이벤트가 저장됨 (LastSeq=%d)", last)
	}
}

// AppendBatch는 저장소 수준 all-or-nothing이다: 배치 내부의 실패(중복 seq의
// PK 위반)가 트랜잭션 전체를 rollback시켜 아무것도 남지 않는다.
func TestAppendBatchAtomic(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// 2번째 insert에서 PK 위반이 나는 배치 — 1번째도 남으면 안 된다
	bad := []gen.EventRecord{rec(1, `{"a":1}`), rec(1, `{"a":2}`)}
	if err := s.AppendBatch(ctx, bad); err == nil {
		t.Fatal("중복 seq 배치가 성공함")
	}
	got, err := s.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("실패 배치 후 %d건 잔존 — rollback 미동작", len(got))
	}

	// 정상 배치는 전건 커밋
	good := []gen.EventRecord{rec(1, `{"a":1}`), rec(2, `{"a":2}`), rec(3, `{"a":3}`)}
	if err := s.AppendBatch(ctx, good); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ReadFrom(ctx, 1)
	if len(got) != 3 {
		t.Fatalf("정상 배치 %d건 저장 (3건 기대)", len(got))
	}
}

// 제안서 §5 실증 2번의 회귀 고정: 다른 연결이 write lock을 잡은 상태에서
// 50ms deadline의 Append는 SQLite busy handler의 5초 블록 없이 짧게 반환된다.
func TestBusyReturnsPromptlyOnShortDeadline(t *testing.T) {
	s, path := openTestStore(t)

	locker, err := sql.Open("sqlite", fileDSN(path, "_busy_timeout=0"))
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	tx, err := locker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO events (seq, ts, trace_id, span_id, kind, actor, payload)
		VALUES (900, 1, '` + strings.Repeat("a", 32) + `', '` + strings.Repeat("b", 16) + `', 'session/start', 'parent', '{}')`); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = s.Append(ctx, rec(1, `{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("잠금 상태의 Append가 성공함")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("deadline 50ms인데 %v 소요 — busy handler 블록 회귀", elapsed)
	}
	if ctx.Err() == nil {
		t.Fatalf("ctx 만료 전에 실패: %v", err)
	}

	// 잠금 해제 후엔 재시도 경로로 성공
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(context.Background(), rec(1, `{}`)); err != nil {
		t.Fatalf("잠금 해제 후 Append 실패: %v", err)
	}
}

// 짧은 잠금은 재시도로 흡수된다 — BUSY는 오류로 새지 않는다.
func TestBusyRetrySucceedsAfterUnlock(t *testing.T) {
	s, path := openTestStore(t)
	locker, err := sql.Open("sqlite", fileDSN(path, "_busy_timeout=0"))
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	tx, err := locker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO events (seq, ts, trace_id, span_id, kind, actor, payload)
		VALUES (901, 1, '` + strings.Repeat("a", 32) + `', '` + strings.Repeat("b", 16) + `', 'session/start', 'parent', '{}')`); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		tx.Rollback()
	}()
	if err := s.Append(context.Background(), rec(1, `{}`)); err != nil {
		t.Fatalf("잠금 해제 후에도 실패: %v", err)
	}
}

func TestLastSeqAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	ctx := context.Background()
	s, err := openStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		if err := s.Append(ctx, rec(i, `{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	last, err := s2.LastSeq(ctx)
	if err != nil || last != 3 {
		t.Fatalf("LastSeq = %d, %v (3 기대)", last, err)
	}
}
