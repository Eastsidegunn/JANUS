package logd

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// FakeStore는 테스트 전용 인메모리 store다 (실제 구현은 seams/store/sqlite).
type FakeStore struct {
	mu      sync.Mutex
	events  []gen.EventRecord
	lastSeq int64
	// gate가 non-nil이면 Append는 gate 수신까지 블록된다(백프레셔 테스트용).
	gate chan struct{}
}

func (s *FakeStore) LastSeq(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq, nil
}

func (s *FakeStore) Append(ctx context.Context, rec gen.EventRecord) error {
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, rec)
	s.lastSeq = rec.Seq
	return nil
}

func (s *FakeStore) ReadFrom(ctx context.Context, fromSeq int64) ([]gen.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []gen.EventRecord
	for _, e := range s.events {
		if e.Seq >= fromSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *FakeStore) Close() error { return nil }

func (s *FakeStore) snapshot() []gen.EventRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gen.EventRecord{}, s.events...)
}

func sampleEvent(payload string) gen.EventRecord {
	return gen.EventRecord{
		Ts: 1, TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("b", 16),
		Kind: gen.KindUserMessage, Actor: "parent",
		Payload: []byte(payload),
	}
}

// FR-LOG-02: 동시 제출에도 seq는 빈틈·중복 없는 전순서이고,
// store에는 seq 오름차순으로만 도달한다.
func TestWriterSeqTotalOrder(t *testing.T) {
	store := &FakeStore{}
	w, err := NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	seqs := make(chan int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq, err := w.Submit(context.Background(), sampleEvent(`{"text":"x"}`))
			if err != nil {
				t.Error(err)
				return
			}
			seqs <- seq
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	close(seqs)
	seen := map[int64]bool{}
	for s := range seqs {
		if s < 1 || s > n || seen[s] {
			t.Fatalf("seq 전순서 위반: %d (중복=%v)", s, seen[s])
		}
		seen[s] = true
	}
	if len(seen) != n {
		t.Fatalf("ack된 seq %d개 (%d개 기대)", len(seen), n)
	}
	events := store.snapshot()
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("store 도달 순서 위반: index %d에 seq %d", i, e.Seq)
		}
	}
}

// writer는 store의 마지막 seq에 이어서 발급한다 (재기동 시나리오).
func TestWriterResumesFromLastSeq(t *testing.T) {
	store := &FakeStore{lastSeq: 41}
	w, err := NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	seq, err := w.Submit(context.Background(), sampleEvent(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 42 {
		t.Fatalf("seq = %d, 42 기대", seq)
	}
}

// FR-LOG-09: 큐 포화 시 Submit이 블록되고(입력 중단), 공간이 생기면 재개되며,
// 수락된 이벤트는 순서대로 전량 저장된다 — 유실 0.
func TestWriterBackpressure(t *testing.T) {
	gate := make(chan struct{})
	store := &FakeStore{gate: gate}
	w, err := NewWriter(context.Background(), store, WithQueueCap(2))
	if err != nil {
		t.Fatal(err)
	}

	acked := make(chan int64, 8)
	submit := func(i int) {
		seq, err := w.Submit(context.Background(), sampleEvent(`{"i":`+string(rune('0'+i))+`}`))
		if err != nil {
			t.Error(err)
			return
		}
		acked <- seq
	}
	// 1번은 루프가 집어가 Append에서 블록, 2·3번이 큐(cap 2)를 채운다.
	go submit(1)
	go submit(2)
	go submit(3)
	// 큐가 실제로 포화될 때까지 대기
	deadline := time.Now().Add(2 * time.Second)
	for len(w.queue) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("큐가 포화되지 않음 — 테스트 전제 실패")
		}
		time.Sleep(time.Millisecond)
	}

	// 4번 제출은 백프레셔로 블록되어야 한다
	blocked := make(chan struct{})
	go func() {
		submit(4)
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("큐 포화 상태에서 Submit이 블록되지 않음 — 백프레셔 부재")
	case <-time.After(100 * time.Millisecond):
	}

	// 공간 확보 → 재개
	close(gate)
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("공간 확보 후에도 Submit이 재개되지 않음")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 수락된 4건 전량 저장, seq 순서, 유실 0
	events := store.snapshot()
	if len(events) != 4 {
		t.Fatalf("저장 %d건 (4건 기대) — 유실 발생", len(events))
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("순서 위반: index %d에 seq %d", i, e.Seq)
		}
	}
}

// Close는 이미 수락된 제출을 전부 커밋한 뒤 반환하고, 이후 Submit은 ErrClosed.
func TestWriterCloseDrains(t *testing.T) {
	store := &FakeStore{}
	w, err := NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Submit(context.Background(), sampleEvent(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Submit(context.Background(), sampleEvent(`{}`)); err != ErrClosed {
		t.Fatalf("닫힌 writer의 Submit = %v, ErrClosed 기대", err)
	}
	if len(store.snapshot()) != 1 {
		t.Fatal("Close 전 수락분이 커밋되지 않음")
	}
}

// FR-LOG-08: 기본 자격증명 패턴이 payload와 raw(base64 내부)에서 마스킹된다.
func TestWriterRedaction(t *testing.T) {
	store := &FakeStore{}
	w, err := NewWriter(context.Background(), store, WithRedactionPatterns(`내부비밀-[0-9]{4}`))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	secrets := []string{
		"AKIAIOSFODNN7EXAMPLE",
		"sk-ant-api03-abcdefghijklmnop",
		"ghp_0123456789abcdefghij0123456789",
		"xoxb-1234567890-abcdefghijk",
		"내부비밀-7777", // 설정으로 확장된 패턴 (SHOULD)
	}
	payload := `{"text":"키는 ` + strings.Join(secrets, " 그리고 ") + ` 입니다"}`
	rawPlain := "creds: " + strings.Join(secrets, ",")
	raw := base64.StdEncoding.EncodeToString([]byte(rawPlain))
	ev := sampleEvent(payload)
	ev.Raw = &raw

	if _, err := w.Submit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	got := store.snapshot()[0]
	for _, s := range secrets {
		if strings.Contains(string(got.Payload), s) {
			t.Errorf("payload에 자격증명 잔존: %s", s)
		}
	}
	if !strings.Contains(string(got.Payload), Redacted) {
		t.Error("payload에 마스킹 흔적 없음")
	}
	decoded, err := base64.StdEncoding.DecodeString(*got.Raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(string(decoded), s) {
			t.Errorf("raw에 자격증명 잔존: %s", s)
		}
	}
}
