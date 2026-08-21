// Package brokerwire owns the backend-neutral, host-only world broker wire.
// Both the adapter seam and a world backend depend inward on these exact
// message types, so no horizontal seam import or duplicated JSON contract is
// needed. This is not the public §5.2 adapter protocol and is never exposed to
// an agent container.
package brokerwire

import "encoding/json"

type Operation string

const (
	OperationIntent Operation = "intent"
	OperationNext   Operation = "next"
)

type Request struct {
	Operation  Operation       `json:"op"`
	Capability string          `json:"capability"`
	SpanID     string          `json:"span_id"`
	CallID     string          `json:"call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
}

type Response struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Hook      *Hook  `json:"hook,omitempty"`
	Delivered bool   `json:"delivered,omitempty"`
}

type Hook struct {
	RequestID string  `json:"request_id"`
	Raw       []byte  `json:"raw"`
	Reason    *string `json:"reason,omitempty"`
}

type Decision struct {
	RequestID string  `json:"request_id"`
	Decision  string  `json:"decision"`
	Reason    *string `json:"reason,omitempty"`
}
