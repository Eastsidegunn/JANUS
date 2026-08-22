package main

import (
	"context"
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
