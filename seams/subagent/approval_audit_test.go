package subagent

import (
	"github.com/Eastsidegunn/JANUS/core/policy"
	"testing"
)

func TestDecisionAuditPayloadSourcesAndActors(t *testing.T) {
	req := policy.ApprovalRequest{RequestID: "req-1"}
	tests := []struct {
		name, forced, source string
		actor                bool
		want                 string
	}{
		{"local", "", "", false, "local"},
		{"forced", "deadline", "relay", false, "forced"},
		{"relay", "", "relay", true, "relay"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := policy.ApprovalDecision{DecisionSource: tt.source, ActorRef: "unverified-local-operator", ResponseID: "resp-1"}
			if !tt.actor {
				d.ActorRef = ""
			}
			p := decisionAuditPayload(req, d, tt.forced, "profile")
			if p.DecisionSource == nil || string(*p.DecisionSource) != tt.want || p.RequestID == nil || *p.RequestID != req.RequestID {
				t.Fatalf("payload=%+v", p)
			}
			if (p.ActorRef != nil) != tt.actor {
				t.Fatalf("actor_ref=%v want=%v", p.ActorRef, tt.actor)
			}
			if tt.actor && (p.ResponseID == nil || *p.ResponseID != "resp-1") {
				t.Fatalf("relay metadata 누락: %+v", p)
			}
		})
	}
}
