package logd

import (
	"context"
	"encoding/base64"
	"errors"
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
	// started가 non-nil이면 Append 진입 시 신호를 보낸다(순서 결정용).
	started chan struct{}
	// failOn과 seq가 일치하면 Append가 failErr를 반환한다(terminal 테스트용).
	failOn      int64
	failErr     error
	appendCalls int
}

func (s *FakeStore) LastSeq(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq, nil
}

func (s *FakeStore) Append(ctx context.Context, rec gen.EventRecord) error {
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendCalls++
	if s.failOn != 0 && rec.Seq == s.failOn {
		return s.failErr
	}
	s.events = append(s.events, rec)
	s.lastSeq = rec.Seq
	return nil
}

// AppendBatch는 all-or-nothing 계약을 흉내낸다: failOn에 해당하는 seq가
// 배치에 있으면 아무것도 저장하지 않고 실패한다.
func (s *FakeStore) AppendBatch(ctx context.Context, recs []gen.EventRecord) error {
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendCalls++
	for _, rec := range recs {
		if s.failOn != 0 && rec.Seq == s.failOn {
			return s.failErr
		}
	}
	for _, rec := range recs {
		s.events = append(s.events, rec)
		s.lastSeq = rec.Seq
	}
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
	// 핵심 회귀 검사: 4번은 큐에 admission되지 않았어야 한다. Submit은 ack까지
	// 기다리므로 "블록됨"만으로는 cap 회귀를 못 잡는다 — 큐 길이가 cap을
	// 넘지 않았음을 직접 고정한다. (cap이 사라지면 여기서 3이 관측된다.)
	if n := len(w.queue); n != 2 {
		t.Fatalf("큐 길이 %d (cap 2 기대) — bounded queue 회귀", n)
	}
	// gate를 잠근 채로는 저장도 admission분(1 in-flight + 큐 2)뿐이다
	if got := len(store.snapshot()); got != 0 {
		t.Fatalf("gate 잠금 중 저장 %d건 (0 기대)", got)
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

// T3 재리뷰 차단 2의 회귀: contracts 위반 이벤트는 저장 직전 검증에서
// 거부되고, seq를 소비하지 않으며, store에 도달하지 않는다.
func TestWriterRejectsContractViolations(t *testing.T) {
	store := &FakeStore{}
	w, err := NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	bad := []struct {
		name string
		mut  func(*gen.EventRecord)
	}{
		{"미지의 kind", func(e *gen.EventRecord) { e.Kind = gen.Kind("session/pause") }},
		{"깨진 payload JSON", func(e *gen.EventRecord) { e.Payload = []byte(`{"broken":`) }},
		{"잘못된 actor", func(e *gen.EventRecord) { e.Actor = "observer" }},
		{"all-zero trace_id", func(e *gen.EventRecord) { e.TraceID = strings.Repeat("0", 32) }},
		{"짧은 span_id", func(e *gen.EventRecord) { e.SpanID = "abc" }},
		{"비base64 raw", func(e *gen.EventRecord) { r := "@@@"; e.Raw = &r }},
	}
	for _, c := range bad {
		ev := sampleEvent(`{"ok":true}`)
		c.mut(&ev)
		if _, err := w.Submit(context.Background(), ev); err == nil {
			t.Errorf("%s: 위반 이벤트가 ack됨", c.name)
		}
	}
	if calls := storeAppendCalls(store); calls != 0 {
		t.Fatalf("위반 이벤트가 store에 %d회 도달", calls)
	}
	// 거부는 seq를 소비하지 않는다 — 다음 유효 제출이 1번을 받는다
	seq, err := w.Submit(context.Background(), sampleEvent(`{"ok":true}`))
	if err != nil || seq != 1 {
		t.Fatalf("유효 제출 seq=%d err=%v (1 기대)", seq, err)
	}
}

func storeAppendCalls(s *FakeStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendCalls
}

// T3 재리뷰 차단 3의 회귀 (1): ctx는 admission까지만 적용된다 — 큐에
// 수락된 제출은 호출자 ctx가 취소돼도 커밋 결과(seq)를 확정적으로 받는다.
func TestSubmitAckDespiteCtxCancelAfterAdmission(t *testing.T) {
	gate := make(chan struct{})
	store := &FakeStore{gate: gate, started: make(chan struct{}, 8)}
	w, err := NewWriter(context.Background(), store, WithQueueCap(2))
	if err != nil {
		t.Fatal(err)
	}

	// 1번이 Append에 진입해 블록된 것을 확인한 뒤에야 2번을 제출한다 —
	// 이후 큐에 보이는 1건은 반드시 2번이다(순서 결정).
	go w.Submit(context.Background(), sampleEvent(`{"i":1}`))
	select {
	case <-store.started:
	case <-time.After(2 * time.Second):
		t.Fatal("1번이 Append에 진입하지 않음")
	}
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		seq int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		seq, err := w.Submit(ctx, sampleEvent(`{"i":2}`))
		done <- result{seq, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(w.queue) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("2번이 admission되지 않음")
		}
		time.Sleep(time.Millisecond)
	}
	cancel() // admission 이후 취소
	select {
	case r := <-done:
		t.Fatalf("취소 직후 모호한 반환: %+v — admission 후에는 ack를 기다려야 함", r)
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	select {
	case r := <-done:
		// 두 제출의 admission 순서는 비결정적이므로 seq 값(1 또는 2)이 아니라
		// 속성만 단정한다: ctx 취소에도 커밋 결과(성공 ack)가 반환됐는가.
		if r.err != nil || r.seq < 1 {
			t.Fatalf("커밋 결과 대신 %+v — 커밋 여부가 모호해짐", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ack가 반환되지 않음")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if len(store.snapshot()) != 2 {
		t.Fatal("수락분 유실")
	}
}

// T3 재리뷰 차단 3의 회귀 (2): store 커밋 실패는 terminal — 실패 이후
// 어떤 이벤트도 커밋되지 않고, Submit과 Close 모두 원인을 반환한다.
func TestWriterTerminalOnStoreFailure(t *testing.T) {
	store := &FakeStore{failOn: 2, failErr: errors.New("디스크 사망")}
	w, err := NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Submit(context.Background(), sampleEvent(`{"i":1}`)); err != nil {
		t.Fatal(err)
	}
	// seq 2에서 store 실패 → 제출자에게 terminal 오류.
	// errors.Is 체인은 ErrTerminal과 원인(store 오류) 양쪽으로 유지되어야 한다.
	if _, err := w.Submit(context.Background(), sampleEvent(`{"i":2}`)); !errors.Is(err, ErrTerminal) {
		t.Fatalf("실패 커밋의 오류 = %v (ErrTerminal 기대)", err)
	} else if !errors.Is(err, store.failErr) {
		t.Fatalf("terminal 오류에서 원인 체인이 끊김: %v", err)
	}
	callsAtFailure := storeAppendCalls(store)
	// 이후 제출은 store에 닿지 않고 terminal로 거부
	if _, err := w.Submit(context.Background(), sampleEvent(`{"i":3}`)); !errors.Is(err, ErrTerminal) {
		t.Fatalf("terminal 이후 Submit = %v (ErrTerminal 기대)", err)
	}
	if storeAppendCalls(store) != callsAtFailure {
		t.Fatal("terminal 이후에도 store 커밋을 시도함")
	}
	// Close도 원인을 전달한다
	if err := w.Close(); !errors.Is(err, ErrTerminal) {
		t.Fatalf("Close = %v (ErrTerminal 기대)", err)
	}
	if len(store.snapshot()) != 1 {
		t.Fatalf("저장 %d건 (1건 기대)", len(store.snapshot()))
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
