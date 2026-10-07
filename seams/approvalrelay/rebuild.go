package approvalrelay

import (
	"encoding/json"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// Rebuild derives relay decisions from the session event log. The log remains
// the source of truth; the returned map is only an in-memory acceleration index.
func Rebuild(events []gen.EventRecord) map[requestKey]Result {
	state := make(map[requestKey]Result)
	for _, event := range events {
		if event.Kind != gen.KindPolicyDecision {
			continue
		}
		var p gen.PolicyDecisionPayload
		if json.Unmarshal(event.Payload, &p) != nil || p.RequestID == nil {
			continue
		}
		key := requestKey{TraceID: event.TraceID, SpanID: event.SpanID, RequestID: *p.RequestID}
		r := Result{Status: "decided", Decision: string(p.Decision), ResponseSeq: event.Seq}
		if p.ResponseID != nil {
			r.ResponseID = *p.ResponseID
		}
		if p.Reason != nil {
			r.Reason = *p.Reason
		}
		if r.Reason == "EXPIRED" || r.Reason == "LEASE_ENDED" {
			r.Status = "expired"
			r.Decision = "deny"
		}
		state[key] = r
	}
	return state
}

// Restore merges decisions derived from a fresh session snapshot into the
// in-memory acceleration index; the session log remains the source of truth.
func (s *Server) Restore(events []gen.EventRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, r := range Rebuild(events) {
		s.decided[k] = r
	}
}
