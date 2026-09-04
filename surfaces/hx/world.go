package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/Eastsidegunn/JANUS/collector"
	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
	"github.com/Eastsidegunn/JANUS/seams/subagent/worldadapter"
)

type worldLaunch struct {
	Backend        world.Backend
	SpawnSpec      world.SpawnSpec
	Writer         *logd.Writer
	TraceID        string
	ParentSpan     string
	AdapterCommand []string
	AdapterBaseEnv []string
	AdapterStderr  io.Writer
	AdapterName    string
	Instruction    string
	Workspace      string
	Budget         gen.Budget
	Depth          int64
	ProfileID      string
	Approval       subagent.Spec
}

type activeWorldSubagent struct {
	Subagent    *subagent.Subagent
	Lease       world.ActiveLease
	Writer      *logd.Writer
	TraceID     string
	ChildSpan   string
	Baseline    collector.Manifest
	effectsDone chan struct{}
	effectsMu   sync.Mutex
	effects     []world.EffectAttempt
	effectsErr  error
}

// startProductionWorld is the sole production assembly path for a sandboxed
// subagent. Schema support for world_backend:none exists only for legacy/test
// records; this surface accepts local-podman metadata and nothing else.
//
// The order is deliberate and covered by the Linux gate:
// Prepare(metadata, no runtime) → durable spawn ACK → Activate(runtime ready)
// → host adapter start. A writer failure therefore cannot start a container.
func startProductionWorld(ctx context.Context, launch worldLaunch) (_ *activeWorldSubagent, err error) {
	if launch.Backend == nil || launch.Writer == nil || len(launch.AdapterCommand) == 0 {
		return nil, fmt.Errorf("hx: world launch 입력이 비어 있음")
	}
	prepared, err := launch.Backend.Prepare(ctx, launch.SpawnSpec)
	if err != nil {
		return nil, err
	}
	abort := true
	defer func() {
		if abort {
			err = errors.Join(err, prepared.Abort(context.Background()))
		}
	}()
	metadata := prepared.Metadata()
	if metadata.Backend != gen.SubagentSpawnPayloadWorldBackendLocalPodman {
		return nil, fmt.Errorf("hx: production surface는 world_backend %q를 거부함", metadata.Backend)
	}
	if len(metadata.Mounts) != 1 || metadata.Mounts[0].SourcePath == "" {
		return nil, fmt.Errorf("hx: world spawn metadata에 workspace mount가 없음")
	}
	baseline, err := collector.BuildBaseline(ctx, metadata.Mounts[0].SourcePath, collector.DefaultLimits())
	if err != nil {
		return nil, fmt.Errorf("hx: collector lower baseline: %w", err)
	}
	profileID, imageDigest := metadata.ProfileID, metadata.ImageDigest
	payload, err := json.Marshal(gen.SubagentSpawnPayload{
		Adapter: launch.AdapterName, Instruction: launch.Instruction, Depth: launch.Depth,
		Budget: gen.SpawnBudget{
			Tokens: launch.Budget.Tokens, TimeMs: launch.Budget.TimeMs, MaxDepth: launch.Budget.MaxDepth,
		},
		WorldBackend: metadata.Backend, ProfileID: &profileID, ImageDigest: &imageDigest,
		Mounts:     metadata.Mounts,
		Extensions: metadata.Extensions,
	})
	if err != nil {
		return nil, err
	}
	childSpan := launch.SpawnSpec.SpanID()
	receipt, err := world.CommitSpawn(ctx, launch.Writer, prepared, world.SpawnRecord(gen.EventRecord{
		Ts: time.Now().UnixMilli(), TraceID: launch.TraceID, SpanID: childSpan,
		ParentSpanID: &launch.ParentSpan, Kind: gen.KindSubagentSpawn, Actor: "parent", Payload: payload,
	}))
	if err != nil {
		return nil, err
	}
	// Activate consumes the receipt before its first runtime side effect. From
	// this point the backend owns cleanup even when activation fails; calling
	// Abort would be both invalid and a misleading secondary error.
	abort = false
	lease, err := prepared.Activate(ctx, receipt)
	if err != nil {
		return nil, err
	}
	descriptor := world.NewAgentDescriptor(lease.ProcessEndpoint(), lease.ApprovalEndpoint(), childSpan)
	spec := launch.Approval
	spec.Adapter = launch.AdapterName
	spec.Command = append([]string(nil), launch.AdapterCommand...)
	baseEnv := launch.AdapterBaseEnv
	if baseEnv == nil {
		baseEnv = os.Environ()
	}
	spec.Env = worldadapter.Environment(baseEnv, descriptor)
	spec.Stderr = launch.AdapterStderr
	spec.Instruction, spec.Workspace = launch.Instruction, launch.Workspace
	spec.Budget, spec.Depth, spec.ProfileID = launch.Budget, launch.Depth, launch.ProfileID
	if secret := launch.SpawnSpec.SecretCapability(); !secret.IsZero() {
		spec.TokenExpiresAtUnixMs = secret.ExpiresAtUnixMs()
	}
	spec.Descriptor = descriptor
	sub, spawnErr := subagent.SpawnPrepared(ctx, launch.Writer, launch.TraceID, launch.ParentSpan, childSpan, 1, spec)
	if spawnErr != nil {
		return nil, errors.Join(spawnErr, lease.Close(context.Background()))
	}
	active := &activeWorldSubagent{
		Subagent: sub, Lease: lease, Writer: launch.Writer, TraceID: launch.TraceID,
		ChildSpan: childSpan, Baseline: baseline, effectsDone: make(chan struct{}),
	}
	go active.collectEffects()
	return active, nil
}

// collectEffects is the sole surface-side consumer of the host-only effect
// stream. It copies T10 observations into collector-owned values and commits
// each synthetic event through the existing writer; collector itself never
// imports core/world or the writer.
func (a *activeWorldSubagent) collectEffects() {
	defer close(a.effectsDone)
	for effect := range a.Lease.Effects() {
		a.effectsMu.Lock()
		a.effects = append(a.effects, effect)
		a.effectsMu.Unlock()
		attempt := collector.EgressAttempt{
			Domain: effect.Target, Method: effect.Method, SizeBytes: effect.RequestBytes,
			AtMs: effect.AtUnixMs, Decision: string(effect.Decision), Reason: effect.Reason,
		}
		record, err := collector.NewEgressRecord(a.TraceID, a.ChildSpan, attempt)
		if err == nil {
			_, err = a.Writer.Submit(context.Background(), record)
		}
		if effect.Ack != nil {
			effect.Ack(err)
		}
		if err != nil {
			a.effectsMu.Lock()
			a.effectsErr = errors.Join(a.effectsErr, fmt.Errorf("egress collector: %w", err))
			a.effectsMu.Unlock()
		}
	}
}

func (a *activeWorldSubagent) EffectSnapshot() ([]world.EffectAttempt, error) {
	a.effectsMu.Lock()
	defer a.effectsMu.Unlock()
	return append([]world.EffectAttempt(nil), a.effects...), a.effectsErr
}

// FinalizeCollection closes runtime resources, drains effect observations,
// commits the complete fs snapshot, and only then presents the opaque receipt
// that permits the backend to remove upper.
func (a *activeWorldSubagent) FinalizeCollection(ctx context.Context) error {
	closeErr := a.Lease.Close(ctx)
	select {
	case <-a.effectsDone:
	case <-ctx.Done():
		return errors.Join(closeErr, fmt.Errorf("collector effect drain: %w", ctx.Err()))
	}
	_, effectErr := a.EffectSnapshot()
	joined := errors.Join(closeErr, effectErr)
	if effectErr != nil {
		return joined
	}
	payload, err := collector.Diff(ctx, a.Baseline, a.Lease.UpperDir(), collector.DefaultLimits())
	if err != nil {
		return errors.Join(joined, fmt.Errorf("collector fs diff: %w", err))
	}
	record, err := collector.NewFsChangedRecord(a.TraceID, a.ChildSpan, time.Now().UnixMilli(), payload)
	if err != nil {
		return errors.Join(joined, err)
	}
	receipt, err := world.CommitCollection(ctx, a.Writer, a.Lease, record)
	if err != nil {
		return errors.Join(joined, err)
	}
	if err := a.Lease.AcknowledgeCollection(receipt); err != nil {
		return errors.Join(joined, fmt.Errorf("collector upper ACK: %w", err))
	}
	return joined
}
