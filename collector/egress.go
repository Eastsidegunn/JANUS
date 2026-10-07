package collector

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// EgressAttempt is the collector-owned copy of a world effect. Keeping this
// type in collector preserves the layer boundary: the surface copies fields
// from core/world and the collector never imports that package.
type EgressAttempt struct {
	Domain    string
	Method    string
	SizeBytes int64
	AtMs      int64
	Decision  string
	Reason    string
}

// EgressPayload converts a host proxy observation into the schema-owned
// payload. It rejects malformed or sensitive denial reasons before a caller
// can submit a record to the writer.
func EgressPayload(attempt EgressAttempt) (gen.EgressPayload, error) {
	if attempt.Domain == "" || attempt.Method == "" {
		return gen.EgressPayload{}, errors.New("collector: egress domain and method are required")
	}
	if attempt.SizeBytes < 0 || attempt.AtMs < 0 {
		return gen.EgressPayload{}, errors.New("collector: egress size/time must be non-negative")
	}
	p := gen.EgressPayload{Domain: attempt.Domain, Method: attempt.Method, SizeBytes: attempt.SizeBytes, AtMs: attempt.AtMs}
	switch attempt.Decision {
	case string(gen.EgressPayloadDecisionAllow):
		if attempt.Reason != "" {
			return gen.EgressPayload{}, errors.New("collector: allow egress cannot carry a reason")
		}
		p.Decision = gen.EgressPayloadDecisionAllow
	case string(gen.EgressPayloadDecisionDeny):
		if attempt.Reason == "" {
			return gen.EgressPayload{}, errors.New("collector: deny egress requires a reason")
		}
		if len([]rune(attempt.Reason)) > 512 {
			return gen.EgressPayload{}, errors.New("collector: deny reason exceeds 512 characters")
		}
		if sensitiveReason(attempt.Reason) {
			return gen.EgressPayload{}, errors.New("collector: deny reason contains prohibited sensitive data")
		}
		p.Decision = gen.EgressPayloadDecisionDeny
		reason := attempt.Reason
		p.Reason = &reason
	default:
		return gen.EgressPayload{}, fmt.Errorf("collector: unknown egress decision %q", attempt.Decision)
	}
	return p, nil
}

func sensitiveReason(reason string) bool {
	lower := strings.ToLower(reason)
	for _, token := range []string{"header", "body", "credential", "resolved ip", "resolved_ip"} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

// NewFsChangedRecord wraps a scanner payload as a synthetic collector event.
// raw is explicitly present as an empty string because there is no native
// upstream line for a host-side observation.
func NewFsChangedRecord(traceID, spanID string, ts int64, payload gen.FsChangedPayload) (gen.EventRecord, error) {
	return newCollectorRecord(traceID, spanID, gen.KindCollectorFsChanged, ts, payload)
}

// NewEgressRecord validates and wraps one proxy observation as a synthetic
// collector event.
func NewEgressRecord(traceID, spanID string, attempt EgressAttempt) (gen.EventRecord, error) {
	payload, err := EgressPayload(attempt)
	if err != nil {
		return gen.EventRecord{}, err
	}
	return newCollectorRecord(traceID, spanID, gen.KindCollectorEgress, attempt.AtMs, payload)
}

func newCollectorRecord(traceID, spanID string, kind gen.Kind, ts int64, payload any) (gen.EventRecord, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return gen.EventRecord{}, fmt.Errorf("collector: marshal %s payload: %w", kind, err)
	}
	raw := ""
	return gen.EventRecord{TraceID: traceID, SpanID: spanID, Ts: ts, Kind: kind, Actor: "collector", Payload: b, Raw: &raw}, nil
}
