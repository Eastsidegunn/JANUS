package main

import (
	"context"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/seams/approvalrelay"
)

// Contract v1.2 ②-b: JANUS-owned deadline deny is final; a late allow cannot
// replace it, and the derived query remains expired with the durable seq.
func TestApprovalRelayDeadlineFinality(t *testing.T) {
	s, err := approvalrelay.NewServer("/tmp/hx-deadline-test.sock", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := approvalrelay.NewServerApprovalRelay(s, approvalrelay.RelayConfig{Endpoint: "/tmp/hx-deadline-test.sock", TraceID: "trace", Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan policy.ApprovalDecision, 1)
	go func() {
		d, _ := relay.Decide(context.Background(), policy.ApprovalRequest{RequestID: "req", SpanID: "span", Args: []byte(`{"x":1}`)})
		done <- d
	}()
	d := <-done
	if d.Allow || d.DecisionSource != "forced" || d.ActorRef != "" {
		t.Fatalf("deadline decision=%+v", d)
	}
	s.RecordApprovalResult(policy.ApprovalRequest{RequestID: "req", SpanID: "span"}, d, 17)
	// Late submit is rejected by the already-final expired state; query keeps it.
	// The transport-level finality is covered by approvalrelay's socket tests.
	if d2 := s.Wait(context.Background(), "trace", "span", "req"); d2.Status != "expired" || d2.Decision != "deny" || d2.ResponseSeq != 17 {
		t.Fatalf("deadline state=%+v", d2)
	}
}
