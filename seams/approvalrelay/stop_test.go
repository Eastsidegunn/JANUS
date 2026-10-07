package approvalrelay

import (
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"
)

func stopServer() *Server {
	return &Server{OwnerUID: os.Getuid(), peerCheck: func(net.Conn) (int, error) { return os.Getuid(), nil }, stops: map[stopKey]Result{}}
}

func stopCall(t *testing.T, s *Server, m Message) Result {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if err := a.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.serve(b); close(done) }()
	go func() { _ = json.NewEncoder(a).Encode(m) }()
	var r Result
	if err := json.NewDecoder(a).Decode(&r); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	<-done
	return r
}

func TestStopLifecycleAndIdempotence(t *testing.T) {
	s := stopServer()
	n := 0
	s.stopCallback = func(Message) { n++ }
	m := Message{Op: "stop", TraceID: "t", StopID: "s", Reason: "user"}
	if r := stopCall(t, s, m); r.Status != "stop_accepted" {
		t.Fatal(r)
	}
	if r := stopCall(t, s, m); r.Status != "stop_accepted" {
		t.Fatal(r)
	}
	if n != 1 {
		t.Fatalf("callback=%d", n)
	}
	if r := stopCall(t, s, Message{Op: "stop", TraceID: "t", StopID: "s", Reason: "policy"}); r.Reason != "STOP_CONFLICT" {
		t.Fatal(r)
	}
	s.MarkTerminal(9)
	if r := stopCall(t, s, Message{Op: "stop", TraceID: "t2", StopID: "s2", Reason: "user"}); r.Status != "already_terminal" || r.TerminalRef != 9 {
		t.Fatal(r)
	}
}

func TestStopReasonAuthorization(t *testing.T) {
	s := stopServer()
	s.budgetExceeded = func() bool { return false }
	s.evidenceValid = func(int64) bool { return false }
	for _, m := range []Message{{Op: "stop", TraceID: "t", StopID: "b", Reason: "budget_exceeded"}, {Op: "stop", TraceID: "t", StopID: "p", Reason: "policy", EvidenceSeq: 1}, {Op: "stop", TraceID: "t", StopID: "d", Reason: "parent_done"}} {
		if r := stopCall(t, s, m); r.Reason != "UNAUTHORIZED" {
			t.Fatal(r)
		}
	}
	s.budgetExceeded = func() bool { return true }
	if r := stopCall(t, s, Message{Op: "stop", TraceID: "t", StopID: "b2", Reason: "budget_exceeded"}); r.Status != "stop_accepted" {
		t.Fatal(r)
	}
}

func TestStopQueryUnknown(t *testing.T) {
	if r := stopCall(t, stopServer(), Message{Op: "stop_query", TraceID: "t", StopID: "missing"}); r.Status != "unknown" {
		t.Fatal(r)
	}
}
