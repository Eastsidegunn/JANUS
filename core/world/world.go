// Package world owns the backend-neutral execution-world contract (FR-SBX-05).
// A Lease is the single lifecycle boundary for the sandbox filesystem, agent
// process, network/effect stream, and cleanup. Implementations live below the
// core layer; consumers must not split these resources into independent owners.
package world

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

// Backend prepares one isolated execution world without creating or starting
// its agent process. A future remote-microVM backend implements the same
// contract; callers do not branch on the implementation.
type Backend interface {
	Prepare(context.Context, SpawnSpec) (PreparedLease, error)
}

// PreparedLease owns preflight and filesystem state only. Activate requires a
// SpawnReceipt minted after the exact spawn metadata was durably committed.
// Once Activate accepts that receipt, it consumes the prepared lease even if
// runtime activation fails; the backend then owns partial-runtime cleanup.
// Abort is idempotent and cleans only a lease that has not entered Activate.
type PreparedLease interface {
	ID() PreparedID
	Metadata() SpawnMetadata
	UpperDir() string
	Activate(context.Context, SpawnReceipt) (ActiveLease, error)
	Abort(context.Context) error
}

// ActiveLease owns the complete lifecycle of one execution world.
//
// Close must preserve this order even on errors:
//
//  1. stop the agent process;
//  2. drain and acknowledge the effect stream;
//  3. clean up the mount and network.
//
// ProcessEndpoint and ApprovalEndpoint are host-only, nominally distinct
// capabilities. Neither may be exposed inside the agent container
// (FR-ADP-10). UpperDir is also host-only.
type ActiveLease interface {
	ProcessEndpoint() ProcessEndpoint
	ApprovalEndpoint() ApprovalEndpoint
	UpperDir() string
	Effects() <-chan EffectAttempt
	Close(context.Context) error
	AcknowledgeCollection(CollectionReceipt) error
}

// PreparedID is an opaque, per-prepare identity. Only NewPreparedID can mint a
// non-zero value; callers can compare IDs but cannot inspect their token.
type PreparedID struct {
	token  [32]byte
	spanID string
}

func NewPreparedID(spanID string) (PreparedID, error) {
	if spanID == "" {
		return PreparedID{}, fmt.Errorf("world: prepared span ID가 비어 있음")
	}
	id := PreparedID{spanID: spanID}
	if _, err := rand.Read(id.token[:]); err != nil {
		return PreparedID{}, fmt.Errorf("world: prepared ID 발급: %w", err)
	}
	return id, nil
}

func (id PreparedID) IsZero() bool { return id == PreparedID{} }

// SpawnReceipt is proof that CommitSpawn received a durable writer ACK for
// this exact prepared lease and metadata. Its fields and constructor are not
// exported, so a caller cannot manufacture an Activate permission.
type SpawnReceipt struct {
	preparedID PreparedID
	metadata   [32]byte
	seq        int64
}

// SpawnRecord is the strongly named input to CommitSpawn. It remains a full
// EventRecord because the writer, not world, owns envelope sequencing.
type SpawnRecord gen.EventRecord

// CollectionReceipt is proof that a collector/fs_changed record was durably
// committed for one active lease. Its fields are intentionally private so a
// caller cannot manufacture permission to remove an upper directory.
type CollectionReceipt struct {
	leaseID string
	spanID  string
	payload [32]byte
	seq     int64
}

// CommitCollection validates and durably records a collector fs snapshot,
// then mints a lease-bound receipt. The backend may accept that receipt only
// after the writer ACK; this keeps upper cleanup after durable evidence.
func CommitCollection(ctx context.Context, writer *logd.Writer, lease ActiveLease, record gen.EventRecord) (CollectionReceipt, error) {
	if writer == nil || lease == nil {
		return CollectionReceipt{}, fmt.Errorf("world: collection commit 입력이 비어 있음")
	}
	if record.Kind != gen.KindCollectorFsChanged || record.Actor != "collector" ||
		record.Raw == nil || *record.Raw != "" {
		return CollectionReceipt{}, fmt.Errorf("world: collector/fs_changed envelope 위반")
	}
	endpoint := lease.ProcessEndpoint()
	if endpoint.LeaseID() == "" || record.SpanID == "" || record.TraceID == "" {
		return CollectionReceipt{}, fmt.Errorf("world: collection lease/span 식별자가 비어 있음")
	}
	seq, err := writer.Submit(ctx, record)
	if err != nil {
		return CollectionReceipt{}, fmt.Errorf("world: collection durable commit: %w", err)
	}
	hash := sha256.Sum256(record.Payload)
	return CollectionReceipt{leaseID: endpoint.LeaseID(), spanID: record.SpanID, payload: hash, seq: seq}, nil
}

// ValidateCollectionReceipt is for backend implementations. It verifies the
// opaque receipt against the active lease and finalizer span; it does not
// consume the receipt.
func ValidateCollectionReceipt(receipt CollectionReceipt, leaseID, spanID string) error {
	if receipt.seq < 1 || receipt.leaseID == "" || receipt.leaseID != leaseID || receipt.spanID != spanID {
		return fmt.Errorf("world: collection receipt가 lease와 일치하지 않음")
	}
	var zero [32]byte
	if subtle.ConstantTimeCompare(receipt.payload[:], zero[:]) == 1 {
		return fmt.Errorf("world: collection receipt payload 증명이 비어 있음")
	}
	return nil
}

// CommitSpawn validates and durably records an exact local-podman spawn before
// minting its one-use activation receipt.
func CommitSpawn(ctx context.Context, writer *logd.Writer, prepared PreparedLease, record SpawnRecord) (SpawnReceipt, error) {
	if writer == nil || prepared == nil || prepared.ID().IsZero() {
		return SpawnReceipt{}, fmt.Errorf("world: spawn commit 입력이 비어 있음")
	}
	rec := gen.EventRecord(record)
	if rec.Kind != gen.KindSubagentSpawn || rec.SpanID != prepared.ID().spanID || rec.ParentSpanID == nil ||
		*rec.ParentSpanID == "" || *rec.ParentSpanID == rec.SpanID {
		return SpawnReceipt{}, fmt.Errorf("world: spawn record는 parent가 있는 subagent/spawn이어야 함")
	}
	metadata := prepared.Metadata()
	if metadata.Backend != gen.SubagentSpawnPayloadWorldBackendLocalPodman {
		return SpawnReceipt{}, fmt.Errorf("world: production spawn은 local-podman metadata여야 함")
	}
	var payload gen.SubagentSpawnPayload
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return SpawnReceipt{}, fmt.Errorf("world: spawn payload decode: %w", err)
	}
	if err := metadataMatchesPayload(metadata, payload); err != nil {
		return SpawnReceipt{}, err
	}
	seq, err := writer.Submit(ctx, rec)
	if err != nil {
		return SpawnReceipt{}, fmt.Errorf("world: spawn durable commit: %w", err)
	}
	hash, err := hashMetadata(metadata)
	if err != nil {
		return SpawnReceipt{}, err
	}
	return SpawnReceipt{preparedID: prepared.ID(), metadata: hash, seq: seq}, nil
}

// ValidateSpawnReceipt is for backend implementations. It validates the
// opaque receipt against their prepared identity and immutable metadata; it
// does not consume it. PreparedLease.Activate owns one-shot consumption.
func ValidateSpawnReceipt(receipt SpawnReceipt, id PreparedID, metadata SpawnMetadata) error {
	if receipt.seq < 1 || id.IsZero() || receipt.preparedID != id {
		return fmt.Errorf("world: spawn receipt가 prepared lease와 일치하지 않음")
	}
	hash, err := hashMetadata(metadata)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(receipt.metadata[:], hash[:]) != 1 {
		return fmt.Errorf("world: spawn receipt metadata 불일치")
	}
	return nil
}

func hashMetadata(metadata SpawnMetadata) ([32]byte, error) {
	encoded, err := json.Marshal(metadata.Clone())
	if err != nil {
		return [32]byte{}, fmt.Errorf("world: spawn metadata canonical encode: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

func metadataMatchesPayload(metadata SpawnMetadata, payload gen.SubagentSpawnPayload) error {
	if payload.WorldBackend != metadata.Backend || payload.ProfileID == nil ||
		payload.ImageDigest == nil ||
		*payload.ProfileID != metadata.ProfileID ||
		*payload.ImageDigest != metadata.ImageDigest {
		return fmt.Errorf("world: spawn payload와 prepared metadata 불일치")
	}
	left, err := json.Marshal(payload.Mounts)
	if err != nil {
		return err
	}
	right, err := json.Marshal(metadata.Mounts)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(left, right) != 1 {
		return fmt.Errorf("world: spawn mount metadata 불일치")
	}
	left, err = json.Marshal(payload.Extensions)
	if err != nil {
		return err
	}
	right, err = json.Marshal(metadata.Extensions)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(left, right) != 1 {
		return fmt.Errorf("world: spawn extension metadata 불일치")
	}
	return nil
}

// EffectivePolicy is the immutable result of policy evaluation. It deliberately
// exposes no merge or mutation operation: a world backend receives the already
// narrowed values and may only bake those values into its sandbox.
type EffectivePolicy struct {
	profileID  string
	workspace  string
	fsScope    []string
	egress     []string
	budget     gen.Budget
	approval   policy.ApprovalMode
	extensions []gen.Extension
}

// NewEffectivePolicy snapshots an already evaluated SandboxConfig. Slice
// fields are copied so later caller mutation cannot widen an opened world.
func NewEffectivePolicy(cfg policy.SandboxConfig) EffectivePolicy {
	return EffectivePolicy{
		profileID:  cfg.ProfileID,
		workspace:  cfg.Workspace,
		fsScope:    append([]string(nil), cfg.FSScope...),
		egress:     append([]string(nil), cfg.Egress...),
		budget:     cfg.Budget,
		approval:   cfg.Approval,
		extensions: cloneWorldExtensions(cfg.Extensions),
	}
}

func (p EffectivePolicy) ProfileID() string             { return p.profileID }
func (p EffectivePolicy) Workspace() string             { return p.workspace }
func (p EffectivePolicy) Budget() gen.Budget            { return p.budget }
func (p EffectivePolicy) Approval() policy.ApprovalMode { return p.approval }
func (p EffectivePolicy) Egress() []string              { return append([]string(nil), p.egress...) }
func (p EffectivePolicy) FSScope() []string             { return append([]string(nil), p.fsScope...) }
func (p EffectivePolicy) Extensions() []gen.Extension   { return cloneWorldExtensions(p.extensions) }

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
	image       ImageReference
	agentArgv   []string
	depth       int64
	traceID     string
	spanID      string
	identity    AgentIdentity
	credentials []CredentialHandle
	bundle      ExtensionBundle
}

func NewSpawnSpec(
	policy EffectivePolicy,
	image ImageReference,
	agentArgv []string,
	depth int64,
	traceID, spanID string,
	identity AgentIdentity,
	credentials []CredentialHandle,
) SpawnSpec {
	return SpawnSpec{
		policy: policy, image: image,
		agentArgv: append([]string(nil), agentArgv...), depth: depth,
		traceID: traceID, spanID: spanID,
		identity:    identity,
		credentials: append([]CredentialHandle(nil), credentials...),
	}
}

func (s SpawnSpec) Policy() EffectivePolicy      { return s.policy }
func (s SpawnSpec) Image() ImageReference        { return s.image }
func (s SpawnSpec) AgentArgv() []string          { return append([]string(nil), s.agentArgv...) }
func (s SpawnSpec) Depth() int64                 { return s.depth }
func (s SpawnSpec) TraceID() string              { return s.traceID }
func (s SpawnSpec) SpanID() string               { return s.spanID }
func (s SpawnSpec) AgentIdentity() AgentIdentity { return s.identity }
func (s SpawnSpec) Credentials() []CredentialHandle {
	return append([]CredentialHandle(nil), s.credentials...)
}

// ExtensionBundle is a host-only, immutable handle to a verified provisioning
// result. It is intentionally opaque: callers can pass it to a world backend
// but cannot place its host path in wire payloads or mutate its contents.
type ExtensionBundle struct {
	path       string
	digest     string
	extensions []gen.SubagentSpawnExtension
}

// NewExtensionBundle is used by the provisioning seam after it has sealed and
// hashed a bundle. The path is never serialized into an agent command payload.
func NewExtensionBundle(path, digest string, extensions []gen.SubagentSpawnExtension) ExtensionBundle {
	return ExtensionBundle{path: path, digest: digest, extensions: append([]gen.SubagentSpawnExtension(nil), extensions...)}
}

func (b ExtensionBundle) Path() string   { return b.path }
func (b ExtensionBundle) Digest() string { return b.digest }
func (b ExtensionBundle) Extensions() []gen.SubagentSpawnExtension {
	return append([]gen.SubagentSpawnExtension(nil), b.extensions...)
}
func (b ExtensionBundle) IsZero() bool {
	return b.path == "" && b.digest == "" && len(b.extensions) == 0
}

// WithExtensionBundle returns a defensive copy with the already verified
// bundle attached. It does not perform policy evaluation or provisioning.
func (s SpawnSpec) WithExtensionBundle(bundle ExtensionBundle) SpawnSpec {
	s.bundle = ExtensionBundle{path: bundle.path, digest: bundle.digest, extensions: bundle.Extensions()}
	return s
}
func (s SpawnSpec) ExtensionBundle() ExtensionBundle {
	return ExtensionBundle{path: s.bundle.path, digest: s.bundle.digest, extensions: s.bundle.Extensions()}
}

func cloneWorldExtensions(in []gen.Extension) []gen.Extension {
	out := make([]gen.Extension, len(in))
	for i, ext := range in {
		out[i] = ext
		out[i].Egress = append([]string(nil), ext.Egress...)
	}
	return out
}

// ImageReference separates the trusted repository lookup name from the
// immutable digest. Runtime backends execute Repository@Digest; only Digest is
// copied to SpawnMetadata and the durable log.
type ImageReference struct {
	repository string
	digest     string
}

func NewImageReference(repository, digest string) ImageReference {
	return ImageReference{repository: repository, digest: digest}
}

func (r ImageReference) Repository() string { return r.repository }
func (r ImageReference) Digest() string     { return r.digest }
func (r ImageReference) String() string     { return r.repository + "@" + r.digest }

// ProcessEndpoint is the host-only process broker capability. It cannot be
// assigned to ApprovalEndpoint and contains no workspace upper path.
type ProcessEndpoint struct {
	network           string
	address           string
	leaseID           string
	controlCapability string
	outputCapability  string
}

func NewProcessEndpoint(network, address, leaseID, controlCapability, outputCapability string) ProcessEndpoint {
	return ProcessEndpoint{network: network, address: address, leaseID: leaseID, controlCapability: controlCapability, outputCapability: outputCapability}
}

func (e ProcessEndpoint) Network() string           { return e.network }
func (e ProcessEndpoint) Address() string           { return e.address }
func (e ProcessEndpoint) LeaseID() string           { return e.leaseID }
func (e ProcessEndpoint) ControlCapability() string { return e.controlCapability }
func (e ProcessEndpoint) OutputCapability() string  { return e.outputCapability }

// String returns a non-secret correlation identifier. It must not be used as
// an activation capability; SpawnReceipt remains the only activation proof.
func (id PreparedID) String() string { return hex.EncodeToString(id.token[:]) }

// ApprovalEndpoint is the separate host-only approval capability.
type ApprovalEndpoint struct {
	network    string
	address    string
	capability string
}

func NewApprovalEndpoint(network, address, capability string) ApprovalEndpoint {
	return ApprovalEndpoint{network: network, address: address, capability: capability}
}

func (e ApprovalEndpoint) Network() string    { return e.network }
func (e ApprovalEndpoint) Address() string    { return e.address }
func (e ApprovalEndpoint) Capability() string { return e.capability }

// AgentDescriptor is the only endpoint bundle a host adapter receives. It has
// no UpperDir and no serialization tags or exported fields.
type AgentDescriptor struct {
	process  ProcessEndpoint
	approval ApprovalEndpoint
	spanID   string
}

func NewAgentDescriptor(process ProcessEndpoint, approval ApprovalEndpoint, spanID string) AgentDescriptor {
	return AgentDescriptor{process: process, approval: approval, spanID: spanID}
}

func (d AgentDescriptor) ProcessEndpoint() ProcessEndpoint   { return d.process }
func (d AgentDescriptor) ApprovalEndpoint() ApprovalEndpoint { return d.approval }
func (d AgentDescriptor) SpanID() string                     { return d.spanID }

// SpawnMetadata is the durable, non-secret environment description used by the
// subagent/spawn event (FR-SBX-06). UpperDir is intentionally absent; UpperRef
// is the stable, host-state-relative identifier from the contracts schema.
type SpawnMetadata struct {
	Backend     gen.SubagentSpawnPayloadWorldBackend
	ProfileID   string
	ImageDigest string
	Mounts      []gen.SubagentSpawnMount
	Extensions  []gen.SubagentSpawnExtension
}

// Clone returns metadata whose mount slice does not alias the source.
func (m SpawnMetadata) Clone() SpawnMetadata {
	m.Mounts = append([]gen.SubagentSpawnMount(nil), m.Mounts...)
	m.Extensions = append([]gen.SubagentSpawnExtension(nil), m.Extensions...)
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
	// Ack is host-local and excluded from wire/log serialization. A world
	// backend may provide it to hold the proxy response until the surface has
	// durably recorded this attempt. Callers must invoke it at most once.
	Ack func(error) `json:"-"`
}

// EffectDecision records whether the sandbox allowed or denied an observed
// effect. Both decisions are part of the audit stream; an empty value is only
// retained for non-egress Fake/test records created before T10-4.
type EffectDecision string

const (
	EffectDecisionAllow EffectDecision = "allow"
	EffectDecisionDeny  EffectDecision = "deny"
)
