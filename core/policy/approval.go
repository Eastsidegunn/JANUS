package policy

import (
	"context"
	"encoding/json"
)

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
}

// ApprovalDecider supplies a parent-side policy decision for a manual profile.
type ApprovalDecider interface {
	Decide(context.Context, ApprovalRequest) (ApprovalDecision, error)
}

// DenyAll is the non-interactive default assembled by the hx surface.
type DenyAll struct{}

func (DenyAll) Decide(context.Context, ApprovalRequest) (ApprovalDecision, error) {
	return ApprovalDecision{Reason: "기본 거부 정책"}, nil
}
