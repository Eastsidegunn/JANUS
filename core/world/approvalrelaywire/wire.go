// Package approvalrelaywire owns the request-only wire exposed inside an
// execution world. It deliberately contains no adapter operation, capability,
// or process command: an agent can request a decision for an exact native hook
// and acknowledge delivery, but cannot inject an intent or parent response.
package approvalrelaywire

import "encoding/json"

const MaxLineBytes = 4 << 20

type Request struct {
	Raw []byte `json:"raw"`
}

type Decision struct {
	Decision string  `json:"decision"`
	Reason   *string `json:"reason,omitempty"`
}

type Ack struct {
	Delivered bool `json:"delivered"`
}

// NativeInput is the closed subset of a PreToolUse input used for host-side
// correlation. Raw Request bytes remain the audit source of truth.
type NativeInput struct {
	HookEventName string          `json:"hook_event_name"`
	ToolUseID     string          `json:"tool_use_id"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}
