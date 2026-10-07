package approvalrelay

import (
	"encoding/json"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

func TestRebuildDerivesDecisionAndSequence(t *testing.T) {
	p, _ := json.Marshal(gen.PolicyDecisionPayload{Decision: gen.PolicyDecisionPayloadDecisionDeny, ProfileID: "p", RequestID: ptr("r"), ResponseID: ptr("resp"), Reason: ptr("EXPIRED"), DecisionSource: sourcePtr(gen.PolicyDecisionPayloadDecisionSourceForced)})
	state := Rebuild([]gen.EventRecord{{Seq: 7, TraceID: "trace", SpanID: "span", Kind: gen.KindPolicyDecision, Payload: p}})
	r, ok := state[requestKey{"trace", "span", "r"}]
	if !ok || r.Status != "expired" || r.Decision != "deny" || r.ResponseSeq != 7 || r.ResponseID != "resp" {
		t.Fatalf("state=%+v", state)
	}
}

func TestRebuildKeepsOrdinaryDecisionsDecided(t *testing.T) {
	p, _ := json.Marshal(gen.PolicyDecisionPayload{Decision: gen.PolicyDecisionPayloadDecisionAllow, ProfileID: "p", RequestID: ptr("r"), ResponseID: ptr("resp")})
	state := Rebuild([]gen.EventRecord{{Seq: 3, TraceID: "t", SpanID: "s", Kind: gen.KindPolicyDecision, Payload: p}})
	if got := state[requestKey{"t", "s", "r"}]; got.Status != "decided" || got.Decision != "allow" || got.ResponseSeq != 3 || got.ResponseID != "resp" {
		t.Fatalf("state=%+v", got)
	}
}

func TestRebuildDoesNotClassifyHumanExpiredText(t *testing.T) {
	reason := "credential expired while checking"
	p, _ := json.Marshal(gen.PolicyDecisionPayload{Decision: gen.PolicyDecisionPayloadDecisionDeny, ProfileID: "p", RequestID: ptr("r"), Reason: &reason})
	state := Rebuild([]gen.EventRecord{{Seq: 4, TraceID: "t", SpanID: "s", Kind: gen.KindPolicyDecision, Payload: p}})
	if got := state[requestKey{"t", "s", "r"}]; got.Status != "decided" || got.Decision != "deny" {
		t.Fatalf("state=%+v", got)
	}
}

func TestRebuildSkipsLegacyDecisionWithoutRequestID(t *testing.T) {
	p, _ := json.Marshal(gen.PolicyDecisionPayload{Decision: gen.PolicyDecisionPayloadDecisionDeny, ProfileID: "p"})
	if got := Rebuild([]gen.EventRecord{{Seq: 5, TraceID: "t", SpanID: "s", Kind: gen.KindPolicyDecision, Payload: p}}); len(got) != 0 {
		t.Fatalf("legacy event must be skipped: %+v", got)
	}
}
func ptr(s string) *string { return &s }
func sourcePtr(s gen.PolicyDecisionPayloadDecisionSource) *gen.PolicyDecisionPayloadDecisionSource {
	return &s
}
