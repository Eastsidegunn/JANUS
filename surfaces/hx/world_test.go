package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/worldtest"
	"github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

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
		AdapterCommand: []string{"unused"}, AdapterName: "world", Instruction: "x", Workspace: "/workspace",
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
		AdapterCommand: []string{"unused"}, AdapterName: "world", Instruction: "x", Workspace: lower,
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
