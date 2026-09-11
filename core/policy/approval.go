package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// CanonicalArgs defines hx-args-digest-v1 input; changes require a new version.
func CanonicalArgs(args json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, err
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("JSON object required")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ApprovalRequest is the policy-owned input for one adapter tool approval
// request (FR-POL-05). SpanID identifies the requesting child span.
type ApprovalRequest struct {
	RequestID string
	CallID    string
	ToolName  string
	Args      json.RawMessage
	SpanID    string
}

// ApprovalDecision is fail-closed: a denial must carry a non-empty reason.
type ApprovalDecision struct {
	Allow  bool
	Reason string
	// Audit metadata is optional at the policy boundary and becomes required
	// when a decision is emitted by an operational relay.
	DecisionSource string
	ActorRef       string
	ResponseID     string
	OperationID    string
	HumanIntentID  string
	CorrelationID  string
}

// ApprovalDecider supplies a parent-side policy decision for a manual profile.
type ApprovalDecider interface {
	Decide(context.Context, ApprovalRequest) (ApprovalDecision, error)
}

// ApprovalResultRecorder optionally receives the durable event sequence after
// the coordinator has committed a policy decision.
type ApprovalResultRecorder interface {
	RecordApprovalResult(ApprovalRequest, ApprovalDecision, int64)
}

// DenyAll is the non-interactive default assembled by the hx surface.
type DenyAll struct{}

func (DenyAll) Decide(context.Context, ApprovalRequest) (ApprovalDecision, error) {
	return ApprovalDecision{Reason: "기본 거부 정책"}, nil
}
