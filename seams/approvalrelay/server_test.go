package approvalrelay

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func roundTrip(t *testing.T, s *Server, m Message) Result {
	t.Helper()
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { s.serve(a); close(done) }()
	if err := json.NewEncoder(b).Encode(m); err != nil {
		t.Fatal(err)
	}
	var r Result
	if err := json.NewDecoder(bufio.NewReader(b)).Decode(&r); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	<-done
	return r
}

func TestExpiredRecordKeepsFinalStateAndSeq(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan Result, 1)
	go func() { ch <- s.Wait(ctx, "t", "s", "r") }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-ch
	s.RecordApprovalResult(policy.ApprovalRequest{RequestID: "r", SpanID: "s", Args: []byte(`{}`)}, policy.ApprovalDecision{Reason: "EXPIRED"}, 9)
	r := roundTrip(t, s, Message{Op: "query", TraceID: "t", SpanID: "s", RequestID: "r"})
	if r.Status != "expired" || r.Decision != "deny" || r.ResponseSeq != 9 {
		t.Fatalf("%+v", r)
	}
}

func TestExpiredRuntimeEqualsRebuild(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan Result, 1)
	go func() { ch <- s.Wait(ctx, "t", "s", "r") }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-ch
	s.RecordApprovalResult(policy.ApprovalRequest{RequestID: "r", SpanID: "s", Args: []byte(`{}`)}, policy.ApprovalDecision{Reason: "EXPIRED"}, 9)
	runtime := roundTrip(t, s, Message{Op: "query", TraceID: "t", SpanID: "s", RequestID: "r"})
	p, _ := json.Marshal(gen.PolicyDecisionPayload{Decision: gen.PolicyDecisionPayloadDecisionDeny, ProfileID: "p", RequestID: ptr("r"), Reason: ptr("EXPIRED"), DecisionSource: sourcePtr(gen.PolicyDecisionPayloadDecisionSourceForced)})
	derived := Rebuild([]gen.EventRecord{{Seq: 9, TraceID: "t", SpanID: "s", Kind: gen.KindPolicyDecision, Payload: p}})[requestKey{"t", "s", "r"}]
	if runtime.Status != derived.Status || runtime.Decision != derived.Decision || runtime.ResponseSeq != derived.ResponseSeq {
		t.Fatalf("runtime=%+v rebuild=%+v", runtime, derived)
	}
}

func TestServerApprovalRelayWaitsForSubmit(t *testing.T) {
	s := testServer(t)
	r, err := NewServerApprovalRelay(s, RelayConfig{Endpoint: filepath.Join(t.TempDir(), "unused.sock"), TraceID: "t", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan policy.ApprovalDecision, 1)
	go func() {
		d, _ := r.Decide(context.Background(), policy.ApprovalRequest{RequestID: "r", SpanID: "s", Args: []byte(`{}`)})
		done <- d
	}()
	time.Sleep(10 * time.Millisecond)
	got := roundTrip(t, s, Message{Op: "submit", TraceID: "t", SpanID: "s", RequestID: "r", ResponseID: "resp", Decision: "allow"})
	if got.Status != "decided" {
		t.Fatalf("submit=%+v", got)
	}
	d := <-done
	if !d.Allow || d.DecisionSource != "relay" || d.ActorRef != "unverified-local-operator" || d.ResponseID != "resp" {
		t.Fatalf("decision=%+v", d)
	}
}

func testServer(t *testing.T) *Server {
	s, err := NewServer(filepath.Join(t.TempDir(), "approval.sock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestServerSubmitQueryAndResponseID(t *testing.T) {
	s := testServer(t)
	key := Message{TraceID: "t", SpanID: "s", RequestID: "r"}
	r := roundTrip(t, s, Message{Op: "submit", TraceID: key.TraceID, SpanID: key.SpanID, RequestID: key.RequestID, ResponseID: "resp-1", Decision: "allow"})
	if r.Status != "decided" || r.ResponseID != "resp-1" {
		t.Fatalf("submit=%+v", r)
	}
	q := roundTrip(t, s, Message{Op: "query", TraceID: key.TraceID, SpanID: key.SpanID, RequestID: key.RequestID})
	if q.Status != "decided" || q.ResponseID != "resp-1" {
		t.Fatalf("query=%+v", q)
	}
}

func TestServerResponseIDIdempotenceAndConflict(t *testing.T) {
	s := testServer(t)
	base := Message{TraceID: "t", SpanID: "s", RequestID: "r", ResponseID: "x", Decision: "deny", Reason: "no"}
	_ = roundTrip(t, s, Message{Op: "submit", TraceID: base.TraceID, SpanID: base.SpanID, RequestID: base.RequestID, ResponseID: base.ResponseID, Decision: base.Decision, Reason: base.Reason})
	if r := roundTrip(t, s, Message{Op: "submit", TraceID: "t", SpanID: "s", RequestID: "r", ResponseID: "x", Decision: "deny", Reason: "no"}); r.Status != "decided" {
		t.Fatalf("idempotent=%+v", r)
	}
	for _, m := range []Message{{Op: "submit", TraceID: "t", SpanID: "s", RequestID: "r", ResponseID: "x", Decision: "allow"}, {Op: "submit", TraceID: "t", SpanID: "s", RequestID: "r", ResponseID: "y", Decision: "deny", Reason: "no"}} {
		if r := roundTrip(t, s, m); r.Reason != "RESPONSE_CONFLICT" {
			t.Fatalf("conflict=%+v", r)
		}
	}
}

func TestServerDenyReasonAndExpiryAreFinal(t *testing.T) {
	s := testServer(t)
	if r := roundTrip(t, s, Message{Op: "submit", TraceID: "t", SpanID: "s", RequestID: "r", ResponseID: "x", Decision: "deny"}); r.Reason != "REQUEST_MISMATCH" {
		t.Fatalf("missing reason=%+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan Result, 1)
	go func() { ch <- s.Wait(ctx, "t", "s", "late") }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	expired := <-ch
	if expired.Status != "expired" || expired.Decision != "deny" {
		t.Fatalf("expired=%+v", expired)
	}
	if r := roundTrip(t, s, Message{Op: "submit", TraceID: "t", SpanID: "s", RequestID: "late", ResponseID: "late-allow", Decision: "allow"}); r.Status != "expired" || r.Decision != "deny" {
		t.Fatalf("late allow=%+v", r)
	}
	if r := roundTrip(t, s, Message{Op: "query", TraceID: "t", SpanID: "s", RequestID: "late"}); r.Status != "expired" || r.Decision != "deny" {
		t.Fatalf("expired query=%+v", r)
	}
}

func TestServerUnknownAndTupleIsolation(t *testing.T) {
	s := testServer(t)
	if r := roundTrip(t, s, Message{Op: "query", TraceID: "u", SpanID: "s", RequestID: "missing"}); r.Status != "unknown" {
		t.Fatalf("unknown=%+v", r)
	}
	_ = roundTrip(t, s, Message{Op: "submit", TraceID: "a", SpanID: "s", RequestID: "same", ResponseID: "a", Decision: "allow"})
	if r := roundTrip(t, s, Message{Op: "query", TraceID: "b", SpanID: "s", RequestID: "same"}); r.Status != "unknown" {
		t.Fatalf("tuple leak=%+v", r)
	}
}
