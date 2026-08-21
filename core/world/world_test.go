package world_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/worldtest"
)

func TestEffectivePolicyAndSpawnSpecAreSnapshots(t *testing.T) {
	egress := []string{"allowed.example"}
	cfg := policy.SandboxConfig{
		ProfileID: "p", Workspace: "/workspace", Egress: egress,
		Budget:   gen.Budget{Tokens: 10, TimeMs: 20, MaxDepth: 2},
		Approval: policy.ApprovalManual,
	}
	effective := world.NewEffectivePolicy(cfg)
	egress[0], cfg.Egress[0] = "widened.example", "also-widened.example"
	if got := effective.Egress(); !reflect.DeepEqual(got, []string{"allowed.example"}) {
		t.Fatalf("정책 스냅샷이 입력 slice 변형으로 넓어짐: %v", got)
	}
	returned := effective.Egress()
	returned[0] = "mutated.example"
	if effective.Egress()[0] != "allowed.example" {
		t.Fatal("Egress 반환 slice를 통한 정책 변형이 가능함")
	}

	argv := []string{"agent", "--json"}
	credentials := []world.CredentialHandle{{ID: "cred-1", Scope: "repo:read", ExpiresAtUnixMs: 1000}}
	spec := world.NewSpawnSpec(effective, "sha256:"+strings.Repeat("a", 64), argv, 0, "trace", "span", credentials)
	argv[0], credentials[0].Scope = "shell", "*"
	if got := spec.AgentArgv(); got[0] != "agent" {
		t.Fatalf("agent argv가 caller 변형을 공유함: %v", got)
	}
	if got := spec.Credentials(); got[0].Scope != "repo:read" {
		t.Fatalf("credential handle이 caller 변형을 공유함: %v", got)
	}
	gotArgv := spec.AgentArgv()
	gotArgv[0] = "changed"
	if spec.AgentArgv()[0] != "agent" {
		t.Fatal("AgentArgv 반환 slice를 통한 spec 변형이 가능함")
	}
}

func TestUpperDirExistsOnlyOnHostLeaseBoundary(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(world.SpawnSpec{}),
		reflect.TypeOf(world.SpawnMetadata{}),
		reflect.TypeOf(world.Endpoint{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			if strings.Contains(strings.ToLower(typ.Field(i).Name), "upperdir") {
				t.Fatalf("%s가 host-only UpperDir를 payload/descriptor 필드로 노출함", typ)
			}
		}
	}
	leaseType := reflect.TypeOf((*world.Lease)(nil)).Elem()
	if _, ok := leaseType.MethodByName("UpperDir"); !ok {
		t.Fatal("Lease에 host-only UpperDir 경계가 없음")
	}
}

func TestFakeBackendRecordsOpenAndPropagatesErrors(t *testing.T) {
	lease := worldtest.NewFakeLease(world.Endpoint{}, world.SpawnMetadata{}, "/host/upper", nil)
	backend := worldtest.NewFakeBackend(lease)
	spec := world.NewSpawnSpec(world.EffectivePolicy{}, "digest", []string{"agent"}, 1, "trace", "span", nil)
	got, err := backend.Open(context.Background(), spec)
	if err != nil || got != lease {
		t.Fatalf("Open = (%v, %v), FakeLease 기대", got, err)
	}
	if opens := backend.FakeOpenedSpecs(); len(opens) != 1 || opens[0].ImageDigest() != "digest" {
		t.Fatalf("Open 호출 기록 이상: %+v", opens)
	}

	wantErr := errors.New("fake open")
	backend.FakeSetOpenError(wantErr)
	if _, err := backend.Open(context.Background(), spec); !errors.Is(err, wantErr) {
		t.Fatalf("injected Open 오류 미전파: %v", err)
	}

	gate := make(chan struct{})
	backend.FakeSetOpenError(nil)
	backend.FakeSetOpenGate(gate)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.Open(ctx, spec); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open gate가 context 취소를 전파하지 않음: %v", err)
	}
}

func TestFakeLeaseLifecycleOrderEffectsAndErrors(t *testing.T) {
	endpoint := world.NewEndpoint("unix", "/host/broker.sock", "one-shot")
	metadata := world.SpawnMetadata{
		Backend: gen.SubagentSpawnPayloadWorldBackendLocalPodman,
		Mounts:  []gen.SubagentSpawnMount{{UpperRef: "state/upper"}},
	}
	effects := []world.EffectAttempt{{ID: "e1", Kind: "egress", Target: "denied.example"}}
	lease := worldtest.NewFakeLease(endpoint, metadata, "/host/state/upper", effects)

	// Returned values and effect streams cannot mutate the fake's configured state.
	gotMetadata := lease.Metadata()
	gotMetadata.Mounts[0].UpperRef = "changed"
	if lease.Metadata().Mounts[0].UpperRef != "state/upper" {
		t.Fatal("Metadata mount slice가 alias됨")
	}
	var gotEffects []world.EffectAttempt
	for effect := range lease.Effects() {
		gotEffects = append(gotEffects, effect)
	}
	if !reflect.DeepEqual(gotEffects, effects) {
		t.Fatalf("effect stream = %+v, want %+v", gotEffects, effects)
	}
	if lease.AdapterEndpoint().Capability() != "one-shot" || lease.UpperDir() != "/host/state/upper" {
		t.Fatal("FakeLease 고정 descriptor 반환 이상")
	}

	stopErr := errors.New("stop")
	ackErr := errors.New("ack")
	cleanupErr := errors.New("cleanup")
	lease.FakeSetCloseError(worldtest.FakeCloseProcessStop, stopErr)
	lease.FakeSetCloseError(worldtest.FakeCloseEffectsAck, ackErr)
	lease.FakeSetCloseError(worldtest.FakeCloseCleanup, cleanupErr)
	drainGate := make(chan struct{})
	lease.FakeSetCloseGate(worldtest.FakeCloseEffectsDrain, drainGate)

	done := make(chan error, 1)
	go func() { done <- lease.Close(context.Background()) }()
	<-lease.FakeStageReached(worldtest.FakeCloseEffectsDrain)
	if got := lease.FakeCloseOrder(); !reflect.DeepEqual(got, []worldtest.FakeCloseStage{
		worldtest.FakeCloseProcessStop, worldtest.FakeCloseEffectsDrain,
	}) {
		t.Fatalf("drain gate 전 close 순서 = %v", got)
	}
	select {
	case <-lease.FakeStageReached(worldtest.FakeCloseCleanup):
		t.Fatal("effect drain gate 전에 cleanup이 시작됨")
	default:
	}
	close(drainGate)
	err := <-done
	for _, want := range []error{stopErr, ackErr, cleanupErr} {
		if !errors.Is(err, want) {
			t.Errorf("Close 오류 체인에 %v 없음: %v", want, err)
		}
	}
	wantOrder := []worldtest.FakeCloseStage{
		worldtest.FakeCloseProcessStop,
		worldtest.FakeCloseEffectsDrain,
		worldtest.FakeCloseEffectsAck,
		worldtest.FakeCloseCleanup,
	}
	if got := lease.FakeCloseOrder(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("Close 순서 = %v, want %v", got, wantOrder)
	}
	if err2 := lease.Close(context.Background()); !errors.Is(err2, cleanupErr) {
		t.Fatalf("두 번째 Close가 저장된 결과를 반환하지 않음: %v", err2)
	}
	if got := lease.FakeCloseOrder(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("두 번째 Close가 lifecycle을 반복함: %v", got)
	}
}
