package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/collector"
	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/worldtest"
	"github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

func TestActiveWorldLifecycleKillAgentThenCleanupRecordsCollection(t *testing.T) {
	ctx := context.Background()
	lower, upper := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(upper, "created.txt"), []byte("created"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := collector.BuildBaseline(ctx, lower, collector.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	log, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	lease := worldtest.NewFakeActiveLease(
		world.NewProcessEndpoint("unix", "/tmp/process", "lease", "control", "output"),
		world.ApprovalEndpoint{}, upper, nil,
	)
	active := &activeWorldSubagent{
		Lease: lease, Writer: log.Writer, TraceID: strings.Repeat("1", 32),
		ChildSpan: strings.Repeat("2", 16), Baseline: baseline, effectsDone: make(chan struct{}),
	}
	go active.collectEffects()
	lifecycle := newActiveWorldLifecycle(active)
	if err := lifecycle.KillAgent(ctx); err != nil {
		t.Fatalf("kill agent: %v", err)
	}
	if err := lifecycle.Cleanup(ctx); err != nil {
		t.Fatalf("graceful cleanup after force close: %v", err)
	}
	events, err := log.Reader.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == gen.KindCollectorFsChanged {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("collector fs_changed count = %d", count)
	}
	if lease.FakeKillAgentCalls() != 1 {
		t.Fatalf("kill agent calls = %d", lease.FakeKillAgentCalls())
	}
}

func TestProductionWorldRejectsNoneWithoutActivation(t *testing.T) {
	ctx := context.Background()
	log, err := sqlite.Open(ctx, t.TempDir()+"/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	prepared := worldtest.NewFakePreparedLease(world.SpawnMetadata{
		Backend: gen.SubagentSpawnPayloadWorldBackendNone,
	}, "/host/upper", nil)
	backend := worldtest.NewFakeBackend(prepared)
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "p", Workspace: "/workspace", FSScope: []string{"/workspace"},
		Budget: gen.Budget{Tokens: 1, TimeMs: 1, MaxDepth: 1}, Approval: policy.ApprovalManual,
	})
	_, err = startProductionWorld(ctx, worldLaunch{
		Backend: backend, Writer: log.Writer, TraceID: strings.Repeat("1", 32), ParentSpan: strings.Repeat("2", 16),
		SpawnSpec:      world.NewSpawnSpec(effective, world.NewImageReference("repo", "sha256:"+strings.Repeat("a", 64)), []string{"agent"}, 0, strings.Repeat("1", 32), strings.Repeat("2", 16), world.AgentIdentity{UID: 1000, GID: 1000}, nil),
		AdapterCommand: []string{"unused"}, AdapterName: "world", ControlMode: gen.SubagentSpawnPayloadControlModeToolApproval, Instruction: "x", Workspace: "/workspace",
		Budget: gen.Budget{Tokens: 1, TimeMs: 1, MaxDepth: 1}, ProfileID: "p",
	})
	if err == nil || !strings.Contains(err.Error(), "world_backend") {
		t.Fatalf("production none이 거부되지 않음: %v", err)
	}
	if prepared.FakeActivationCount() != 0 || !prepared.FakeAborted() {
		t.Fatalf("none 거부 뒤 부작용 발생: activations=%d aborted=%v", prepared.FakeActivationCount(), prepared.FakeAborted())
	}
}

func TestProductionWorldActivationFailureIsNotMisreportedAsAbortFailure(t *testing.T) {
	ctx := context.Background()
	lower := t.TempDir()
	if err := os.WriteFile(lower+"/seed.txt", []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := sqlite.Open(ctx, t.TempDir()+"/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	activationErr := errors.New("runtime activation failed")
	profileID, digest := "p", "sha256:"+strings.Repeat("a", 64)
	prepared := worldtest.NewFakePreparedLease(world.SpawnMetadata{
		Backend: gen.SubagentSpawnPayloadWorldBackendLocalPodman, ProfileID: profileID,
		ImageDigest: digest, Mounts: []gen.SubagentSpawnMount{{
			SourcePath: lower, TargetPath: gen.SubagentSpawnMountTargetPathWorkspace,
			Mode: gen.SubagentSpawnMountModeOverlay, UpperRef: "world/upper",
		}},
	}, "/host/upper", nil)
	prepared.FakeSetActivateError(activationErr)
	backend := worldtest.NewFakeBackend(prepared)
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: profileID, Workspace: "/workspace", FSScope: []string{"/workspace"},
		Budget: gen.Budget{Tokens: 1, TimeMs: 1, MaxDepth: 1}, Approval: policy.ApprovalManual,
	})
	_, err = startProductionWorld(ctx, worldLaunch{
		Backend: backend, Writer: log.Writer, TraceID: strings.Repeat("1", 32), ParentSpan: strings.Repeat("3", 16),
		SpawnSpec: world.NewSpawnSpec(effective, world.NewImageReference("repo", digest), []string{"agent"}, 0,
			strings.Repeat("1", 32), strings.Repeat("2", 16), world.AgentIdentity{UID: 1000, GID: 1000}, nil),
		AdapterCommand: []string{"unused"}, AdapterName: "world", ControlMode: gen.SubagentSpawnPayloadControlModeToolApproval, Instruction: "x", Workspace: lower,
		Budget: gen.Budget{Tokens: 1, TimeMs: 1, MaxDepth: 1}, ProfileID: profileID,
	})
	if !errors.Is(err, activationErr) || strings.Contains(err.Error(), "Abort") {
		t.Fatalf("activation 원인이 보존되지 않음: %v", err)
	}
	if prepared.FakeActivationCount() != 1 || prepared.FakeAborted() {
		t.Fatalf("Activate 이후 cleanup 소유권이 backend로 넘어가지 않음: activations=%d aborted=%v",
			prepared.FakeActivationCount(), prepared.FakeAborted())
	}
}

func TestProductionWorldCodexRequiresContainerOnly(t *testing.T) {
	ctx := context.Background()
	lower := t.TempDir()
	log, err := sqlite.Open(ctx, t.TempDir()+"/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	digest := "sha256:" + strings.Repeat("b", 64)
	prepared := worldtest.NewFakePreparedLease(world.SpawnMetadata{
		Backend: gen.SubagentSpawnPayloadWorldBackendLocalPodman, ProfileID: "p", ImageDigest: digest,
		Mounts: []gen.SubagentSpawnMount{{SourcePath: lower, TargetPath: gen.SubagentSpawnMountTargetPathWorkspace, Mode: gen.SubagentSpawnMountModeOverlay, UpperRef: "upper"}},
	}, "/host/upper", nil)
	backend := worldtest.NewFakeBackend(prepared)
	effective := world.NewEffectivePolicy(policy.SandboxConfig{ProfileID: "p", Workspace: "/workspace", FSScope: []string{"/workspace"}, Budget: gen.Budget{Tokens: 1, TimeMs: 1, MaxDepth: 1}, Approval: policy.ApprovalManual})
	base := world.NewSpawnSpec(effective, world.NewImageReference("repo", digest), []string{"codex"}, 0, strings.Repeat("1", 32), strings.Repeat("2", 16), world.AgentIdentity{UID: 1000, GID: 1000}, nil)
	launch := worldLaunch{Backend: backend, Writer: log.Writer, TraceID: strings.Repeat("1", 32), ParentSpan: strings.Repeat("2", 16), SpawnSpec: base, AdapterCommand: []string{"unused"}, AdapterName: "codex", ControlMode: gen.SubagentSpawnPayloadControlModeToolApproval, Instruction: "x", Workspace: lower, Budget: effective.Budget(), ProfileID: "p"}
	if _, err := startProductionWorld(ctx, launch); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("codex tool_approval not rejected: %v", err)
	}
	if prepared.FakeActivationCount() != 0 || !prepared.FakeAborted() {
		t.Fatalf("rejected launch had side effects: activations=%d aborted=%v", prepared.FakeActivationCount(), prepared.FakeAborted())
	}
	launch.ControlMode = gen.SubagentSpawnPayloadControlModeContainerOnly
	if _, err := startProductionWorld(ctx, launch); err == nil || strings.Contains(err.Error(), "container_only") {
		t.Fatalf("container_only hit control guard: %v", err)
	}
}

func TestProductionWorldArmsActiveLeaseForSecondSignalBeforeLifecycle(t *testing.T) {
	lower := t.TempDir()
	log, err := sqlite.Open(context.Background(), t.TempDir()+"/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	traceID, parentSpan, childSpan := strings.Repeat("1", 32), strings.Repeat("3", 16), strings.Repeat("2", 16)
	digest := "sha256:" + strings.Repeat("c", 64)
	process := world.NewProcessEndpoint("unix", "/tmp/process", "lease", "control", "output")
	approval := world.NewApprovalEndpoint("unix", "/tmp/approval", "capability")
	lease := worldtest.NewFakeActiveLease(process, approval, t.TempDir(), nil)
	prepared := worldtest.NewFakePreparedLease(world.SpawnMetadata{
		Backend: gen.SubagentSpawnPayloadWorldBackendLocalPodman, ProfileID: "p", ImageDigest: digest,
		Mounts: []gen.SubagentSpawnMount{{SourcePath: lower, TargetPath: gen.SubagentSpawnMountTargetPathWorkspace, Mode: gen.SubagentSpawnMountModeOverlay, UpperRef: "upper"}},
	}, lease.UpperDir(), lease)
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "p", Workspace: lower, FSScope: []string{lower},
		Budget: gen.Budget{Tokens: 1, TimeMs: 1000, MaxDepth: 1}, Approval: policy.ApprovalManual,
	})
	hub := &FakeSignalHub{}
	forced := make(chan int, 1)
	hooks := hub.hooks(forced)
	launchCtx, signals := startSessionSignals(context.Background(), &hooks)
	defer signals.stop()
	script := `read task
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
read stop
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"stopped","result":"adapter stop"},"raw":""}'`
	active, err := startProductionWorld(launchCtx, worldLaunch{
		Backend: worldtest.NewFakeBackend(prepared), Writer: log.Writer, TraceID: traceID, ParentSpan: parentSpan,
		SpawnSpec:      world.NewSpawnSpec(effective, world.NewImageReference("repo", digest), []string{"agent"}, 0, traceID, childSpan, world.AgentIdentity{UID: 1000, GID: 1000}, nil),
		AdapterCommand: []string{"/bin/sh", "-c", script}, AdapterName: "world", ControlMode: gen.SubagentSpawnPayloadControlModeToolApproval,
		Instruction: "x", Workspace: lower, Budget: effective.Budget(), ProfileID: "p", Signals: signals,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub.Send(syscall.SIGTERM)
	hub.Send(syscall.SIGINT)
	select {
	case code := <-forced:
		if code != 130 {
			t.Fatalf("forced exit code=%d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("second signal did not force exit")
	}
	if got := lease.FakeCloseOrder(); len(got) != 4 {
		t.Fatalf("active lease cleanup stages=%v", got)
	}
	if err := active.Subagent.Stop(gen.StopPayloadReasonUser); err != nil {
		t.Fatal(err)
	}
	if _, err := active.Subagent.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
