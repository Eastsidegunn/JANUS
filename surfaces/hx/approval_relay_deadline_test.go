package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"

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

// Contract v1.2 ②-b: coordinator timeout deny is durable and final.
func TestApprovalRelayDeadlineThroughCoordinator(t *testing.T) {
	h := startRelayHarness(t, 100*time.Millisecond)
	ev, req := h.awaitApprovalRequest(t)
	if _, err := h.sub.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := h.events(t)
	var seq int64
	for _, e := range events {
		if e.Kind == gen.KindPolicyDecision {
			var p gen.PolicyDecisionPayload
			if json.Unmarshal(e.Payload, &p) == nil && p.RequestID != nil && *p.RequestID == req.RequestID {
				seq = e.Seq
				if p.DecisionSource == nil || string(*p.DecisionSource) != "forced" || p.ActorRef != nil || p.Reason == nil || *p.Reason != "EXPIRED" {
					t.Fatalf("payload=%+v", p)
				}
			}
		}
	}
	if seq == 0 {
		t.Fatal("durable forced deny missing")
	}
	q := h.message(t, approvalrelay.Message{Op: "query", TraceID: ev.TraceID, SpanID: ev.SpanID, RequestID: req.RequestID})
	if q.Status != "expired" || q.Decision != "deny" || q.ResponseSeq != seq {
		t.Fatalf("query=%+v seq=%d", q, seq)
	}
	before := len(events)
	late := h.message(t, approvalrelay.Message{Op: "submit", TraceID: ev.TraceID, SpanID: ev.SpanID, RequestID: req.RequestID, ResponseID: "late", Decision: "allow"})
	if late.Status != "expired" || late.Decision != "deny" {
		t.Fatalf("late=%+v", late)
	}
	if after := len(h.events(t)); after != before {
		t.Fatalf("late submit changed log: %d -> %d", before, after)
	}
}
