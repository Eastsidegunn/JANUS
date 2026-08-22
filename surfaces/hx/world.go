package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

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
	AdapterName    string
	Instruction    string
	Workspace      string
	Budget         gen.Budget
	Depth          int64
	ProfileID      string
	Approval       subagent.Spec
}

type activeWorldSubagent struct {
	Subagent *subagent.Subagent
	Lease    world.ActiveLease
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
	profileID, imageDigest := metadata.ProfileID, metadata.ImageDigest
	payload, err := json.Marshal(gen.SubagentSpawnPayload{
		Adapter: launch.AdapterName, Instruction: launch.Instruction, Depth: launch.Depth,
		Budget: gen.SpawnBudget{
			Tokens: launch.Budget.Tokens, TimeMs: launch.Budget.TimeMs, MaxDepth: launch.Budget.MaxDepth,
		},
		WorldBackend: metadata.Backend, ProfileID: &profileID, ImageDigest: &imageDigest,
		Mounts: metadata.Mounts,
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
	lease, err := prepared.Activate(ctx, receipt)
	if err != nil {
		return nil, err
	}
	abort = false
	descriptor := world.NewAgentDescriptor(lease.ProcessEndpoint(), lease.ApprovalEndpoint(), childSpan)
	spec := launch.Approval
	spec.Adapter = launch.AdapterName
	spec.Command = append([]string(nil), launch.AdapterCommand...)
	baseEnv := launch.AdapterBaseEnv
	if baseEnv == nil {
		baseEnv = os.Environ()
	}
	spec.Env = worldadapter.Environment(baseEnv, descriptor)
	spec.Instruction, spec.Workspace = launch.Instruction, launch.Workspace
	spec.Budget, spec.Depth, spec.ProfileID = launch.Budget, launch.Depth, launch.ProfileID
	spec.Descriptor = descriptor
	sub, spawnErr := subagent.SpawnPrepared(ctx, launch.Writer, launch.TraceID, launch.ParentSpan, childSpan, 1, spec)
	if spawnErr != nil {
		return nil, errors.Join(spawnErr, lease.Close(context.Background()))
	}
	return &activeWorldSubagent{Subagent: sub, Lease: lease}, nil
}
