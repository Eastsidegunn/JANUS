package approvalrelay

import (
	"bufio"
	"context"
	"encoding/json"
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
