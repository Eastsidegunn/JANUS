package approvalrelay

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
)

// memStore is a test-only logd.Store; the Writer is still the only seq issuer.
type memStore struct {
	mu     sync.Mutex
	events []gen.EventRecord
}

func (s *memStore) LastSeq(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return 0, nil
	}
	return s.events[len(s.events)-1].Seq, nil
}
func (s *memStore) Append(_ context.Context, rec gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, rec)
	return nil
}
func (s *memStore) AppendBatch(_ context.Context, recs []gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, recs...)
	return nil
}
func (s *memStore) ReadFrom(_ context.Context, from int64) ([]gen.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []gen.EventRecord
	for _, e := range s.events {
		if e.Seq >= from {
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *memStore) Close() error { return nil }

func sessionServer() *Server {
	s := stopServer()
	s.Timeout = 2 * time.Second
	return s
}

func call[T any](t *testing.T, s *Server, m Message) T {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if err := a.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.serve(b); close(done) }()
	go func() { _ = json.NewEncoder(a).Encode(m) }()
	var r T
	if err := json.NewDecoder(a).Decode(&r); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return after its single response")
	}
	return r
}

const testTrace = "11111111111111111111111111111111"

// writeSession builds a realistic session log through the single Writer.
func writeSession(t *testing.T, mode gen.SubagentSpawnPayloadSessionMode, done bool) (*memStore, *logd.Writer) {
	t.Helper()
	store := &memStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	root, child := "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	submit := func(ts int64, span string, kind gen.Kind, actor, payload string, in, out *int64) {
		t.Helper()
		rec := gen.EventRecord{Ts: ts, TraceID: testTrace, SpanID: span, Kind: kind, Actor: actor, Payload: json.RawMessage(payload), UsageIn: in, UsageOut: out}
		if span == child {
			p := root
			rec.ParentSpanID = &p
		}
		if _, err := w.Submit(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	i64 := func(v int64) *int64 { return &v }
	spawn := `{"adapter":"claudecode","instruction":"x","depth":0,"budget":{"tokens":1,"time_ms":1,"max_depth":1},"world_backend":"none","control_mode":"tool_approval"`
	if mode != "" {
		spawn += `,"session_mode":"` + string(mode) + `"`
	}
	spawn += `}`
	submit(1000, root, gen.KindSessionStart, "parent", `{}`, nil, nil)
	submit(1001, child, gen.KindSubagentSpawn, "parent", spawn, nil, nil)
	submit(1002, child, gen.KindSubagentReady, "subagent:claudecode:1", `{"grade":"observable"}`, nil, nil)
	submit(1003, child, gen.KindSubagentMessage, "subagent:claudecode:1", `{"text":"t1"}`, nil, nil)
	submit(1004, child, gen.KindSubagentUsage, "subagent:claudecode:1", `{"input_tokens":10,"output_tokens":4}`, i64(10), i64(4))
	submit(1010, child, gen.KindUserMessage, "parent", `{"text":"둘째"}`, nil, nil)
	submit(1011, child, gen.KindSubagentToolCall, "subagent:claudecode:1", `{"call_id":"c","name":"Bash","args":{}}`, nil, nil)
	submit(1012, child, gen.KindSubagentUsage, "subagent:claudecode:1", `{"input_tokens":20,"output_tokens":6}`, i64(20), i64(6))
	if done {
		submit(1020, child, gen.KindSubagentDone, "subagent:claudecode:1", `{"status":"stopped","result":"x"}`, nil, nil)
		submit(1021, root, gen.KindSessionEnd, "parent", `{}`, nil, nil)
	}
	return store, w
}

// T25 (d): events_tail is the read-only projection of the session log. Its
// answer equals a fresh recomputation from the log (ProjectSession, and the
// usage totals equal logd.Replay's independent aggregation), envelopes carry
// every identity field, seq is contiguous, and serving it never writes.
func TestEventsTailEqualsLogRecomputation(t *testing.T) {
	for _, done := range []bool{false, true} {
		store, _ := writeSession(t, gen.SubagentSpawnPayloadSessionModeMultiturn, done)
		s := sessionServer()
		s.SetSession(SessionControl{SessionID: testTrace, Multiturn: true, Events: store.ReadFrom})
		before, _ := store.LastSeq(context.Background())
		got := call[TailResult](t, s, Message{Op: "events_tail", SessionID: testTrace, FromSeq: 0})
		after, _ := store.LastSeq(context.Background())
		if before != after {
			t.Fatalf("events_tail wrote to the log: %d → %d", before, after)
		}
		events, _ := store.ReadFrom(context.Background(), 1)
		want, err := ProjectSession(events, testTrace, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("socket projection != recomputation\ngot:  %+v\nwant: %+v", got, want)
		}
		replayed, err := logd.Replay(events)
		if err != nil {
			t.Fatal(err)
		}
		st := got.Session
		if st.UsageInTotal != replayed.UsageIn || st.UsageOutTotal != replayed.UsageOut || st.UsageInTotal != 30 || st.UsageOutTotal != 10 {
			t.Fatalf("usage totals=%d/%d replay=%d/%d", st.UsageInTotal, st.UsageOutTotal, replayed.UsageIn, replayed.UsageOut)
		}
		if st.SessionMode != "multiturn" || st.LastSeq != int64(len(events)) || st.LastActivityTs != events[len(events)-1].Ts {
			t.Fatalf("status=%+v", st)
		}
		if done && (st.State != "exited" || st.DoneStatus != "stopped" || st.TerminalSeq != 9) {
			t.Fatalf("exited status=%+v", st)
		}
		if !done && (st.State != "running" || st.DoneStatus != "" || st.TerminalSeq != 0) {
			t.Fatalf("running status=%+v", st)
		}
		for i, e := range got.Events {
			if e.Seq != int64(i+1) || e.Ts == 0 || e.Kind == "" || e.Actor == "" || e.SpanID == "" || e.TraceID != testTrace {
				t.Fatalf("envelope %d=%+v", i, e)
			}
		}
		// Required envelope fields are serialized even when a consumer would
		// otherwise see them omitted.
		raw, _ := json.Marshal(got.Events[0])
		for _, key := range []string{`"seq":`, `"ts":`, `"kind":`, `"actor":`, `"span_id":`, `"trace_id":`} {
			if !strings.Contains(string(raw), key) {
				t.Fatalf("envelope json lacks %s: %s", key, raw)
			}
		}
		if again := call[TailResult](t, s, Message{Op: "events_tail", SessionID: testTrace}); !reflect.DeepEqual(again, got) {
			t.Fatal("projection is not deterministic")
		}
	}
}

// T25 (d): paging by next_from_seq over any cursor/limit reconstructs exactly
// the seq-ordered tail: no gap, no duplicate, no synthesized entry.
func TestProjectSessionPagingIsGaplessProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(25))
	for iter := 0; iter < 500; iter++ {
		n := rng.Intn(40)
		var events []gen.EventRecord
		kinds := []gen.Kind{gen.KindSubagentMessage, gen.KindSubagentToolCall, gen.KindUserMessage, gen.KindSubagentUsage, gen.KindPolicyDecision}
		for i := 0; i < n; i++ {
			events = append(events, gen.EventRecord{Seq: int64(i + 1), Ts: int64(1000 + i), TraceID: testTrace, SpanID: "bbbbbbbbbbbbbbbb", Kind: kinds[rng.Intn(len(kinds))], Actor: "parent", Payload: json.RawMessage(`{}`)})
		}
		from := int64(rng.Intn(n + 2))
		limit := 1 + rng.Intn(7)
		var pages []Envelope
		cursor := from
		for guard := 0; ; guard++ {
			if guard > n+2 {
				t.Fatalf("paging did not terminate: n=%d from=%d limit=%d", n, from, limit)
			}
			r, err := ProjectSession(events, testTrace, cursor, limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Events) > limit {
				t.Fatalf("page over limit")
			}
			pages = append(pages, r.Events...)
			cursor = r.NextFromSeq
			if !r.More {
				break
			}
		}
		var want []Envelope
		for _, e := range events {
			if e.Seq > from {
				want = append(want, Envelope{Seq: e.Seq, Ts: e.Ts, Kind: string(e.Kind), Actor: e.Actor, SpanID: e.SpanID, TraceID: e.TraceID})
			}
		}
		if len(want) == 0 && len(pages) == 0 {
			continue
		}
		if !reflect.DeepEqual(pages, want) {
			t.Fatalf("iter %d: pages=%v want=%v", iter, pages, want)
		}
	}
}

// F2: a log missing an envelope field, with a seq gap, or with a foreign trace
// is refused rather than zero-filled or gap-skipped.
func TestProjectSessionRejectsInvalidEnvelopes(t *testing.T) {
	good := func() []gen.EventRecord {
		return []gen.EventRecord{
			{Seq: 1, Ts: 1, TraceID: testTrace, SpanID: "a", Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`)},
			{Seq: 2, Ts: 2, TraceID: testTrace, SpanID: "a", Kind: gen.KindSubagentMessage, Actor: "parent", Payload: json.RawMessage(`{}`)},
		}
	}
	if _, err := ProjectSession(good(), testTrace, 0, 0); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]gen.EventRecord){
		"seq gap":       func(e []gen.EventRecord) { e[1].Seq = 3 },
		"seq not from1": func(e []gen.EventRecord) { e[0].Seq, e[1].Seq = 2, 3 },
		"ts missing":    func(e []gen.EventRecord) { e[1].Ts = 0 },
		"kind missing":  func(e []gen.EventRecord) { e[1].Kind = "" },
		"actor missing": func(e []gen.EventRecord) { e[1].Actor = "" },
		"span missing":  func(e []gen.EventRecord) { e[1].SpanID = "" },
		"trace missing": func(e []gen.EventRecord) { e[1].TraceID = "" },
		"foreign trace": func(e []gen.EventRecord) { e[1].TraceID = "22222222222222222222222222222222" },
	} {
		events := good()
		mutate(events)
		if _, err := ProjectSession(events, testTrace, 0, 0); err == nil {
			t.Fatalf("%s accepted", name)
		}
		s := sessionServer()
		s.SetSession(SessionControl{SessionID: testTrace, Events: func(context.Context, int64) ([]gen.EventRecord, error) { return events, nil }})
		if r := call[TailResult](t, s, Message{Op: "events_tail", SessionID: testTrace}); r.Status != "error" || r.Reason != ReasonLogInvalid || len(r.Events) != 0 {
			t.Fatalf("%s via socket: %+v", name, r)
		}
	}
}

func TestEventsTailRejections(t *testing.T) {
	store, _ := writeSession(t, "", false)
	s := sessionServer()
	if r := call[TailResult](t, s, Message{Op: "events_tail", SessionID: testTrace}); r.Reason != ReasonUnknownSession {
		t.Fatalf("unbound session: %+v", r)
	}
	s.SetSession(SessionControl{SessionID: testTrace, Events: store.ReadFrom})
	for name, m := range map[string]Message{
		"missing session": {Op: "events_tail"},
		"negative cursor": {Op: "events_tail", SessionID: testTrace, FromSeq: -1},
	} {
		if r := call[TailResult](t, s, m); r.Reason != ReasonRequestMismatch {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	if r := call[TailResult](t, s, Message{Op: "events_tail", SessionID: "ffffffffffffffffffffffffffffffff"}); r.Reason != ReasonUnknownSession {
		t.Fatalf("foreign session: %+v", r)
	}
	s.SetSession(SessionControl{SessionID: testTrace, Events: func(context.Context, int64) ([]gen.EventRecord, error) { return nil, errors.New("io") }})
	if r := call[TailResult](t, s, Message{Op: "events_tail", SessionID: testTrace}); r.Reason != ReasonLogUnavailable {
		t.Fatalf("read failure: %+v", r)
	}
	// Oneshot sessions are still observable read-only.
	s.SetSession(SessionControl{SessionID: testTrace, Events: store.ReadFrom})
	if r := call[TailResult](t, s, Message{Op: "events_tail", SessionID: testTrace, FromSeq: 6}); r.Status != "ok" || len(r.Events) != 2 || r.Events[0].Seq != 7 || r.Session.SessionMode != "oneshot" {
		t.Fatalf("oneshot tail: %+v", r)
	}
}

// F1: one request, one response, then the connection ends. A consumer that
// disconnects without reading leaves no serving goroutine behind.
func TestEventsTailConsumerDepartureLeavesNoGoroutine(t *testing.T) {
	store, _ := writeSession(t, gen.SubagentSpawnPayloadSessionModeMultiturn, true)
	s := sessionServer()
	s.SetSession(SessionControl{SessionID: testTrace, Multiturn: true, Events: store.ReadFrom})
	runtime.GC()
	baseline := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		a, b := net.Pipe()
		done := make(chan struct{})
		go func() { s.serve(b); _ = b.Close(); close(done) }()
		if err := json.NewEncoder(a).Encode(Message{Op: "events_tail", SessionID: testTrace}); err != nil {
			t.Fatal(err)
		}
		_ = a.Close() // departs before reading the response
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("serve blocked after consumer departure")
		}
	}
	for i := 0; i < 50; i++ {
		_ = call[TailResult](t, s, Message{Op: "events_tail", SessionID: testTrace})
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Fatalf("goroutines leaked: baseline=%d now=%d", baseline, n)
	}
}

// T25 (e) + S3: send_message rejects deterministically with structured reasons
// and routes accepted text only through the session-owned handler.
func TestSendMessageRoutingAndRejections(t *testing.T) {
	s := sessionServer()
	var got []string
	handler := func(_ context.Context, text string) (int64, error) { got = append(got, text); return 42, nil }
	m := Message{Op: "send_message", SessionID: testTrace, Text: "둘째 턴"}
	if r := call[Result](t, s, m); r.Reason != ReasonUnknownSession {
		t.Fatalf("unbound: %+v", r)
	}
	s.SetSession(SessionControl{SessionID: testTrace, Multiturn: false})
	s.SetMessageHandler(handler)
	if r := call[Result](t, s, m); r.Status != "error" || r.Reason != ReasonNotMultiturn {
		t.Fatalf("oneshot: %+v", r)
	}
	s.SetSession(SessionControl{SessionID: testTrace, Multiturn: true})
	s.SetMessageHandler(nil)
	if r := call[Result](t, s, m); r.Reason != ReasonSessionNotReady {
		t.Fatalf("not ready: %+v", r)
	}
	s.SetMessageHandler(handler)
	for name, bad := range map[string]Message{
		"missing session": {Op: "send_message", Text: "x"},
		"empty text":      {Op: "send_message", SessionID: testTrace, Text: " \n"},
	} {
		if r := call[Result](t, s, bad); r.Reason != ReasonRequestMismatch {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	if r := call[Result](t, s, Message{Op: "send_message", SessionID: "ffffffffffffffffffffffffffffffff", Text: "x"}); r.Reason != ReasonUnknownSession {
		t.Fatalf("foreign session: %+v", r)
	}
	if r := call[Result](t, s, m); r.Status != "message_accepted" || r.MessageSeq != 42 {
		t.Fatalf("accepted: %+v", r)
	}
	if !reflect.DeepEqual(got, []string{"둘째 턴"}) {
		t.Fatalf("handler saw %q", got)
	}
	for err, reason := range map[error]string{ErrNotMultiturn: ReasonNotMultiturn, ErrSessionTerminal: ReasonSessionTerminal, errors.New("io"): ReasonLogUnavailable} {
		e := err
		s.SetMessageHandler(func(context.Context, string) (int64, error) { return 0, e })
		if r := call[Result](t, s, m); r.Reason != reason {
			t.Fatalf("%v → %+v", err, r)
		}
	}
	s.SetMessageHandler(func(context.Context, string) (int64, error) { return 7, errors.New("pipe") })
	if r := call[Result](t, s, m); r.Reason != ReasonDeliveryFailed || r.MessageSeq != 7 {
		t.Fatalf("delivery failure: %+v", r)
	}
	calls := 0
	s.SetMessageHandler(func(context.Context, string) (int64, error) { calls++; return 1, nil })
	s.MarkTerminal(99)
	if r := call[Result](t, s, m); r.Reason != ReasonSessionTerminal || r.TerminalRef != 99 || calls != 0 {
		t.Fatalf("terminal: %+v calls=%d", r, calls)
	}
}

// S3 size bound: send_message obeys the T18 relay line limit; an oversize
// request never reaches the handler.
func TestSendMessageOversizeNeverReachesHandler(t *testing.T) {
	s := sessionServer()
	calls := 0
	s.SetSession(SessionControl{SessionID: testTrace, Multiturn: true})
	s.SetMessageHandler(func(context.Context, string) (int64, error) { calls++; return 1, nil })
	a, b := net.Pipe()
	defer a.Close()
	_ = a.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan struct{})
	go func() { s.serve(b); _ = b.Close(); close(done) }()
	go func() {
		_ = json.NewEncoder(a).Encode(Message{Op: "send_message", SessionID: testTrace, Text: strings.Repeat("x", maxRelayMessage+1)})
	}()
	var r Result
	if err := json.NewDecoder(a).Decode(&r); err != nil {
		t.Fatal(err)
	}
	if r.Reason != ReasonRequestMismatch || calls != 0 {
		t.Fatalf("oversize: %+v calls=%d", r, calls)
	}
	_ = a.Close()
	<-done
}

// T19 stop semantics are untouched by the multiplexed session ops: stop on a
// multiturn-bound relay is still idempotent and already_terminal after done.
func TestStopSemanticsUnchangedOnMultiturnRelay(t *testing.T) {
	s := sessionServer()
	s.SetSession(SessionControl{SessionID: testTrace, Multiturn: true})
	s.SetMessageHandler(func(context.Context, string) (int64, error) { return 1, nil })
	n := 0
	s.stopCallback = func(Message) { n++ }
	stop := Message{Op: "stop", TraceID: testTrace, StopID: "s1", Reason: "user"}
	if r := stopCall(t, s, stop); r.Status != "stop_accepted" {
		t.Fatal(r)
	}
	if r := stopCall(t, s, stop); r.Status != "stop_accepted" || n != 1 {
		t.Fatalf("idempotence: %+v n=%d", r, n)
	}
	s.MarkTerminal(12)
	if r := stopCall(t, s, Message{Op: "stop", TraceID: testTrace, StopID: "s2", Reason: "user"}); r.Status != "already_terminal" || r.TerminalRef != 12 {
		t.Fatal(r)
	}
	if r := stopCall(t, s, Message{Op: "send_message", SessionID: testTrace, Text: "x"}); r.Reason != ReasonSessionTerminal {
		t.Fatalf("send after terminal: %+v", r)
	}
}
