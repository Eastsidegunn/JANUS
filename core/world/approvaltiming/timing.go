// Package approvaltiming defines the bounded approval-hook timing contract.
// It is shared by the adapter, hook helper, production relay, and local
// world broker so those layers cannot accidentally reintroduce a timeout race.
package approvaltiming

import "time"

const (
	// HookTimeoutSeconds is the Claude command-hook timeout in seconds. Keep
	// this as the one value rendered into --settings.
	HookTimeoutSeconds = 600

	// HookTimeout is the outer Claude hook budget. HxapproveDeadline leaves a
	// full minute for Claude's hook process teardown before this expires.
	HookTimeout        = HookTimeoutSeconds * time.Second
	HookTeardownMargin = 60 * time.Second

	// HxapproveDeadline is deliberately below HookTimeout: an unavailable or
	// silent relay therefore exits 2 (blocks) before Claude treats a timeout as
	// a non-blocking hook failure.
	HxapproveDeadline = HookTimeout - HookTeardownMargin

	// ApprovalWaitMax is below the hxapprove deadline by another full minute.
	// Host-side deciders must settle (or durable-deny) before the hook's own
	// fail-closed deadline can win a race.
	ApprovalWaitMax = HxapproveDeadline - HookTeardownMargin
)
