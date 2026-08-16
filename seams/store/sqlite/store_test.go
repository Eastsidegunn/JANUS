package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.db")
	s, err := Open(path)
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
	s, path := openStore(t)
	ctx := context.Background()
	if err := s.Append(ctx, rec(1, `{"a":1}`)); err != nil {
		t.Fatal(err)
	}

	direct, err := sql.Open("sqlite", "file:"+path+"?_busy_timeout=0")
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
	s, _ := openStore(t)
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
	s, _ := openStore(t)
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

// FR-LOG-02: 실제 SQLite store 위에서 logd.Writer 경유 동시 쓰기 —
// seq 전순서와 유실 0. writer를 경유하지 않은 동일 seq 삽입은
// PRIMARY KEY 충돌로 거부된다(단일 writer 강제의 저장소 측 방어).
func TestSingleWriterOverSQLite(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	w, err := logd.NewWriter(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.Submit(ctx, rec(0, `{"c":1}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadFrom(ctx, 1)
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
	// writer 우회 시도: 이미 발급된 seq로의 직접 Append는 거부
	if err := s.Append(ctx, rec(int64(n), `{"우회":1}`)); err == nil {
		t.Fatal("중복 seq Append가 성공함 — 단일 writer 방어 실패")
	}
}

// 제안서 §5 실증 2번의 회귀 고정: 다른 연결이 write lock을 잡은 상태에서
// 50ms deadline의 Append는 SQLite busy handler의 5초 블록 없이 짧게 반환된다.
func TestBusyReturnsPromptlyOnShortDeadline(t *testing.T) {
	s, path := openStore(t)

	locker, err := sql.Open("sqlite", "file:"+path+"?_busy_timeout=0")
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
	s, path := openStore(t)
	locker, err := sql.Open("sqlite", "file:"+path+"?_busy_timeout=0")
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
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := int64(1); i <= 3; i++ {
		if err := s.Append(ctx, rec(i, `{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	last, err := s2.LastSeq(ctx)
	if err != nil || last != 3 {
		t.Fatalf("LastSeq = %d, %v (3 기대)", last, err)
	}
}
