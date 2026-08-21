// Package world owns the backend-neutral execution-world contract (FR-SBX-05).
// A Lease is the single lifecycle boundary for the sandbox filesystem, agent
// process, network/effect stream, and cleanup. Implementations live below the
// core layer; consumers must not split these resources into independent owners.
package world

import (
	"context"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

// Backend opens one isolated execution world. A future remote-microVM backend
// implements the same contract; callers do not branch on the implementation.
type Backend interface {
	Open(context.Context, SpawnSpec) (Lease, error)
}

// Lease owns the complete lifecycle of one execution world.
//
// Close must preserve this order even on errors:
//
//  1. stop the agent process;
//  2. drain and acknowledge the effect stream;
//  3. clean up the mount and network.
//
// AdapterEndpoint is a host-side process-broker capability. It is for the host
// adapter only and must never be exposed inside the agent container
// (FR-ADP-10). UpperDir is also host-only: it must not be copied into an agent
// command, adapter command, endpoint, or event payload.
type Lease interface {
	AdapterEndpoint() Endpoint
	Metadata() SpawnMetadata
	UpperDir() string
	Effects() <-chan EffectAttempt
	Close(context.Context) error
}

// EffectivePolicy is the immutable result of policy evaluation. It deliberately
// exposes no merge or mutation operation: a world backend receives the already
// narrowed values and may only bake those values into its sandbox.
type EffectivePolicy struct {
	profileID string
	workspace string
	fsScope   []string
	egress    []string
	budget    gen.Budget
	approval  policy.ApprovalMode
}

// NewEffectivePolicy snapshots an already evaluated SandboxConfig. Slice
// fields are copied so later caller mutation cannot widen an opened world.
func NewEffectivePolicy(cfg policy.SandboxConfig) EffectivePolicy {
	return EffectivePolicy{
		profileID: cfg.ProfileID,
		workspace: cfg.Workspace,
		fsScope:   append([]string(nil), cfg.FSScope...),
		egress:    append([]string(nil), cfg.Egress...),
		budget:    cfg.Budget,
		approval:  cfg.Approval,
	}
}

func (p EffectivePolicy) ProfileID() string             { return p.profileID }
func (p EffectivePolicy) Workspace() string             { return p.workspace }
func (p EffectivePolicy) Budget() gen.Budget            { return p.budget }
func (p EffectivePolicy) Approval() policy.ApprovalMode { return p.approval }
func (p EffectivePolicy) Egress() []string              { return append([]string(nil), p.egress...) }
func (p EffectivePolicy) FSScope() []string             { return append([]string(nil), p.fsScope...) }

// AgentIdentity is the numeric UID/GID declared by the agent image. The local
// backend maps the invoking rootless user to this identity with keep-id.
type AgentIdentity struct {
	UID uint32
	GID uint32
}

// CredentialHandle names a scoped, expiring credential held by the world
// broker. It contains no credential value (FR-SBX-04).
type CredentialHandle struct {
	ID              string
	Scope           string
	ExpiresAtUnixMs int64
}

// SpawnSpec is an immutable request to open an execution world. The policy is
// already merged and narrowed before construction; world has no policy merge
// API. AgentArgv and Credentials are defensively copied.
type SpawnSpec struct {
	policy      EffectivePolicy
	imageDigest string
	agentArgv   []string
	depth       int64
	traceID     string
	spanID      string
	identity    AgentIdentity
	credentials []CredentialHandle
}

func NewSpawnSpec(
	policy EffectivePolicy,
	imageDigest string,
	agentArgv []string,
	depth int64,
	traceID, spanID string,
	identity AgentIdentity,
	credentials []CredentialHandle,
) SpawnSpec {
	return SpawnSpec{
		policy: policy, imageDigest: imageDigest,
		agentArgv: append([]string(nil), agentArgv...), depth: depth,
		traceID: traceID, spanID: spanID,
		identity:    identity,
		credentials: append([]CredentialHandle(nil), credentials...),
	}
}

func (s SpawnSpec) Policy() EffectivePolicy      { return s.policy }
func (s SpawnSpec) ImageDigest() string          { return s.imageDigest }
func (s SpawnSpec) AgentArgv() []string          { return append([]string(nil), s.agentArgv...) }
func (s SpawnSpec) Depth() int64                 { return s.depth }
func (s SpawnSpec) TraceID() string              { return s.traceID }
func (s SpawnSpec) SpanID() string               { return s.spanID }
func (s SpawnSpec) AgentIdentity() AgentIdentity { return s.identity }
func (s SpawnSpec) Credentials() []CredentialHandle {
	return append([]CredentialHandle(nil), s.credentials...)
}

// Endpoint is an opaque host-side broker capability descriptor. It contains no
// workspace upper path. Network and Address locate the broker; Capability is a
// per-lease secret and must not be logged or passed to the agent.
type Endpoint struct {
	network    string
	address    string
	capability string
}

func NewEndpoint(network, address, capability string) Endpoint {
	return Endpoint{network: network, address: address, capability: capability}
}

func (e Endpoint) Network() string    { return e.network }
func (e Endpoint) Address() string    { return e.address }
func (e Endpoint) Capability() string { return e.capability }

// SpawnMetadata is the durable, non-secret environment description used by the
// subagent/spawn event (FR-SBX-06). UpperDir is intentionally absent; UpperRef
// is the stable, host-state-relative identifier from the contracts schema.
type SpawnMetadata struct {
	Backend     gen.SubagentSpawnPayloadWorldBackend
	ProfileID   string
	ImageDigest string
	Mounts      []gen.SubagentSpawnMount
}

// Clone returns metadata whose mount slice does not alias the source.
func (m SpawnMetadata) Clone() SpawnMetadata {
	m.Mounts = append([]gen.SubagentSpawnMount(nil), m.Mounts...)
	return m
}

// EffectAttempt is one observed filesystem, process, or network attempt. T10
// owns the stream and its drain/ack lifecycle; T11 maps attempts to collector
// events without taking ownership of the Lease.
type EffectAttempt struct {
	ID           string
	SpanID       string
	Kind         string
	Target       string
	Method       string
	RequestBytes int64
	AtUnixMs     int64
	Decision     EffectDecision
	Reason       string
}

// EffectDecision records whether the sandbox allowed or denied an observed
// effect. Both decisions are part of the audit stream; an empty value is only
// retained for non-egress Fake/test records created before T10-4.
type EffectDecision string

const (
	EffectDecisionAllow EffectDecision = "allow"
	EffectDecisionDeny  EffectDecision = "deny"
)
