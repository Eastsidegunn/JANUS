package world_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/worldtest"
)

func TestEffectivePolicyAndSpawnSpecAreSnapshots(t *testing.T) {
	egress := []string{"allowed.example"}
	fsScope := []string{"/workspace"}
	cfg := policy.SandboxConfig{
		ProfileID: "p", Workspace: "/workspace", FSScope: fsScope, Egress: egress,
		Budget:   gen.Budget{Tokens: 10, TimeMs: 20, MaxDepth: 2},
		Approval: policy.ApprovalManual,
	}
	effective := world.NewEffectivePolicy(cfg)
	egress[0], cfg.Egress[0], fsScope[0] = "widened.example", "also-widened.example", "/"
	if got := effective.Egress(); !reflect.DeepEqual(got, []string{"allowed.example"}) {
		t.Fatalf("정책 스냅샷이 입력 slice 변형으로 넓어짐: %v", got)
	}
	returned := effective.Egress()
	returned[0] = "mutated.example"
	if effective.Egress()[0] != "allowed.example" {
		t.Fatal("Egress 반환 slice를 통한 정책 변형이 가능함")
	}
	scope := effective.FSScope()
	scope[0] = "/"
	if effective.FSScope()[0] != "/workspace" {
		t.Fatal("FSScope 반환 slice를 통한 정책 확장이 가능함")
	}

	argv := []string{"agent", "--json"}
	credentials := []world.CredentialHandle{{ID: "cred-1", Scope: "repo:read", ExpiresAtUnixMs: 1000}}
	image := world.NewImageReference("localhost/hx-agent", "sha256:"+strings.Repeat("a", 64))
	spec := world.NewSpawnSpec(effective, image, argv, 0, "trace", "span", world.AgentIdentity{UID: 1000, GID: 1000}, credentials)
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

func TestExtensionBundleIsHostOnlyAndDefensivelyCopied(t *testing.T) {
	meta := []gen.SubagentSpawnExtension{{Name: "demo", Version: "1.0.0", Integrity: "sha256:" + strings.Repeat("a", 64), Source: "registry.example", ArtifactDigest: "sha256:" + strings.Repeat("b", 64)}}
	bundle := world.NewExtensionBundle("/state/bundle", "sha256:"+strings.Repeat("c", 64), meta)
	meta[0].Name = "mutated"
	got := bundle.Extensions()
	if got[0].Name != "demo" || bundle.Path() != "/state/bundle" {
		t.Fatalf("bundle alias/path leak: %+v", got)
	}
	cfg := policy.SandboxConfig{Extensions: []gen.Extension{{Name: "demo", Version: "1.0.0", Integrity: "sha256:" + strings.Repeat("d", 64), Source: "registry.example"}}}
	effective := world.NewEffectivePolicy(cfg)
	copied := effective.Extensions()
	copied[0].Name = "mutated"
	if effective.Extensions()[0].Name != "demo" {
		t.Fatal("effective policy extension snapshot aliases caller")
	}
}

func TestUpperDirExistsOnlyOnHostLeaseBoundary(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(world.SpawnSpec{}),
		reflect.TypeOf(world.SpawnMetadata{}),
		reflect.TypeOf(world.ProcessEndpoint{}),
		reflect.TypeOf(world.ApprovalEndpoint{}),
		reflect.TypeOf(world.AgentDescriptor{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			if strings.Contains(strings.ToLower(typ.Field(i).Name), "upperdir") {
				t.Fatalf("%s가 host-only UpperDir를 payload/descriptor 필드로 노출함", typ)
			}
		}
	}
	for _, leaseType := range []reflect.Type{
		reflect.TypeOf((*world.PreparedLease)(nil)).Elem(), reflect.TypeOf((*world.ActiveLease)(nil)).Elem(),
	} {
		if _, ok := leaseType.MethodByName("UpperDir"); !ok {
			t.Fatalf("%s에 host-only UpperDir 경계가 없음", leaseType)
		}
	}
}

func TestProcessAndApprovalEndpointsAreNominalAndDescriptorIsHostOnly(t *testing.T) {
	processType := reflect.TypeOf(world.ProcessEndpoint{})
	approvalType := reflect.TypeOf(world.ApprovalEndpoint{})
	if processType == approvalType || processType.ConvertibleTo(approvalType) || approvalType.ConvertibleTo(processType) {
		t.Fatal("process/approval endpoint가 상호 변환 가능한 generic capability임")
	}
	descriptorType := reflect.TypeOf(world.AgentDescriptor{})
	for i := 0; i < descriptorType.NumField(); i++ {
		if descriptorType.Field(i).IsExported() {
			t.Fatalf("AgentDescriptor가 raw endpoint field를 노출함: %s", descriptorType.Field(i).Name)
		}
	}
}

func TestFakeBackendRecordsPrepareAndPropagatesErrors(t *testing.T) {
	prepared := worldtest.NewFakePreparedLease(world.SpawnMetadata{}, "/host/upper", nil)
	backend := worldtest.NewFakeBackend(prepared)
	spec := world.NewSpawnSpec(world.EffectivePolicy{}, world.NewImageReference("repo", "digest"), []string{"agent"}, 1, "trace", "span", world.AgentIdentity{}, nil)
	got, err := backend.Prepare(context.Background(), spec)
	if err != nil || got != prepared {
		t.Fatalf("Prepare = (%v, %v), FakePreparedLease 기대", got, err)
	}
	if prepares := backend.FakePreparedSpecs(); len(prepares) != 1 || prepares[0].Image().Digest() != "digest" {
		t.Fatalf("Prepare 호출 기록 이상: %+v", prepares)
	}

	wantErr := errors.New("fake prepare")
	backend.FakeSetPrepareError(wantErr)
	if _, err := backend.Prepare(context.Background(), spec); !errors.Is(err, wantErr) {
		t.Fatalf("injected Prepare 오류 미전파: %v", err)
	}

	gate := make(chan struct{})
	backend.FakeSetPrepareError(nil)
	backend.FakeSetPrepareGate(gate)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.Prepare(ctx, spec); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare gate가 context 취소를 전파하지 않음: %v", err)
	}
}

func TestFakeActiveLeaseLifecycleOrderEffectsAndErrors(t *testing.T) {
	process := world.NewProcessEndpoint("unix", "/host/process.sock", "lease", "control-cap", "output-cap")
	approval := world.NewApprovalEndpoint("unix", "/host/approval.sock", "approval-cap")
	effects := []world.EffectAttempt{{ID: "e1", Kind: "egress", Target: "denied.example"}}
	lease := worldtest.NewFakeActiveLease(process, approval, "/host/state/upper", effects)

	// Returned values and effect streams cannot mutate the fake's configured state.
	var gotEffects []world.EffectAttempt
	for effect := range lease.Effects() {
		gotEffects = append(gotEffects, effect)
	}
	if !reflect.DeepEqual(gotEffects, effects) {
		t.Fatalf("effect stream = %+v, want %+v", gotEffects, effects)
	}
	if lease.ProcessEndpoint().ControlCapability() != "control-cap" || lease.ProcessEndpoint().OutputCapability() != "output-cap" ||
		lease.ApprovalEndpoint().Capability() != "approval-cap" || lease.UpperDir() != "/host/state/upper" {
		t.Fatal("FakeActiveLease 고정 descriptor 반환 이상")
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
	waitSignal(t, lease.FakeStageReached(worldtest.FakeCloseEffectsDrain), "effect drain 단계")
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
	err := waitError(t, done, "Lease.Close 완료")
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

func TestCommitSpawnMintsLeaseBoundReceiptOnlyAfterDurableAck(t *testing.T) {
	metadata := world.SpawnMetadata{
		Backend:   gen.SubagentSpawnPayloadWorldBackendLocalPodman,
		ProfileID: "profile", ImageDigest: "sha256:" + strings.Repeat("a", 64),
		Mounts: []gen.SubagentSpawnMount{{SourcePath: "/workspace", TargetPath: gen.SubagentSpawnMountTargetPathWorkspace, Mode: gen.SubagentSpawnMountModeOverlay, UpperRef: "world/t/s/overlay/upper"}},
	}
	active := worldtest.NewFakeActiveLease(world.ProcessEndpoint{}, world.ApprovalEndpoint{}, "/upper", nil)
	prepared := worldtest.NewFakePreparedLease(metadata, "/upper", active)
	other := worldtest.NewFakePreparedLease(metadata, "/upper2", nil)
	store := &memoryStore{}
	writer, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	payload, _ := json.Marshal(gen.SubagentSpawnPayload{
		Adapter: "world", Instruction: "test", Depth: 0, Budget: gen.SpawnBudget{Tokens: 1, TimeMs: 1, MaxDepth: 1},
		WorldBackend: metadata.Backend, ProfileID: &metadata.ProfileID, ImageDigest: &metadata.ImageDigest, Mounts: metadata.Mounts,
	})
	parent := "1111111111111111"
	record := world.SpawnRecord(gen.EventRecord{
		TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16), ParentSpanID: &parent,
		Ts: 1, Kind: gen.KindSubagentSpawn, Actor: "parent", Payload: payload,
	})
	receipt, err := world.CommitSpawn(context.Background(), writer, prepared, record)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.records()) != 1 {
		t.Fatalf("durable records=%d, want 1", len(store.records()))
	}
	if _, err := other.Activate(context.Background(), receipt); err == nil || other.FakeActivationCount() != 0 {
		t.Fatalf("cross-lease receipt가 부작용 없이 거부되지 않음: err=%v count=%d", err, other.FakeActivationCount())
	}
	got, err := prepared.Activate(context.Background(), receipt)
	if err != nil || got != active {
		t.Fatalf("Activate=(%v,%v)", got, err)
	}
	if _, err := prepared.Activate(context.Background(), receipt); err == nil || prepared.FakeActivationCount() != 1 {
		t.Fatalf("reused receipt가 거부되지 않음: err=%v count=%d", err, prepared.FakeActivationCount())
	}
}

func TestCommitSpawnRejectsMismatchBeforeWriter(t *testing.T) {
	metadata := world.SpawnMetadata{Backend: gen.SubagentSpawnPayloadWorldBackendLocalPodman, ProfileID: "p", ImageDigest: "sha256:" + strings.Repeat("a", 64)}
	prepared := worldtest.NewFakePreparedLease(metadata, "/upper", nil)
	store := &memoryStore{}
	writer, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	wrong := "sha256:" + strings.Repeat("b", 64)
	p := "p"
	payload, _ := json.Marshal(gen.SubagentSpawnPayload{Adapter: "world", Instruction: "x", Budget: gen.SpawnBudget{}, WorldBackend: metadata.Backend, ProfileID: &p, ImageDigest: &wrong})
	parent := "1111111111111111"
	_, err = world.CommitSpawn(context.Background(), writer, prepared, world.SpawnRecord(gen.EventRecord{TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16), ParentSpanID: &parent, Ts: 1, Kind: gen.KindSubagentSpawn, Actor: "parent", Payload: payload}))
	if err == nil || len(store.records()) != 0 {
		t.Fatalf("metadata mismatch가 writer 전 거부되지 않음: err=%v records=%d", err, len(store.records()))
	}
}

func TestCommitSpawnRejectsExtensionMountAndBasicMetadataMismatchBeforeWriter(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	metadata := world.SpawnMetadata{
		Backend: gen.SubagentSpawnPayloadWorldBackendLocalPodman, ProfileID: "profile", ImageDigest: digest,
		Mounts:     []gen.SubagentSpawnMount{{SourcePath: "/workspace", TargetPath: gen.SubagentSpawnMountTargetPathWorkspace, Mode: gen.SubagentSpawnMountModeOverlay, UpperRef: "world/t/s/upper"}},
		Extensions: []gen.SubagentSpawnExtension{{Name: "demo", Version: "1.0.0", Integrity: digest, Source: "registry.example", ArtifactDigest: digest}},
	}
	parent := "1111111111111111"
	for _, tc := range []struct {
		name   string
		mutate func(*gen.SubagentSpawnPayload)
	}{
		{"extensions", func(p *gen.SubagentSpawnPayload) {
			p.Extensions[0].ArtifactDigest = "sha256:" + strings.Repeat("b", 64)
		}},
		{"mounts", func(p *gen.SubagentSpawnPayload) { p.Mounts[0].UpperRef = "world/t/s/other" }},
		{"basic", func(p *gen.SubagentSpawnPayload) { v := "other"; p.ProfileID = &v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared := worldtest.NewFakePreparedLease(metadata, "/upper", nil)
			store := &memoryStore{}
			writer, err := logd.NewWriter(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			profile, image := metadata.ProfileID, metadata.ImageDigest
			payload := gen.SubagentSpawnPayload{Adapter: "world", Instruction: "test", Depth: 0, Budget: gen.SpawnBudget{Tokens: 1, TimeMs: 1, MaxDepth: 1}, WorldBackend: metadata.Backend, ProfileID: &profile, ImageDigest: &image, Mounts: metadata.Mounts, Extensions: metadata.Extensions}
			tc.mutate(&payload)
			bytes, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			_, err = world.CommitSpawn(context.Background(), writer, prepared, world.SpawnRecord(gen.EventRecord{TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16), ParentSpanID: &parent, Ts: 1, Kind: gen.KindSubagentSpawn, Actor: "parent", Payload: bytes}))
			if err == nil {
				t.Fatal("metadata mismatch가 거부되지 않음")
			}
			if got := len(store.records()); got != 0 {
				t.Fatalf("거부 후 durable spawn=%d", got)
			}
			if got := prepared.FakeActivationCount(); got != 0 {
				t.Fatalf("거부 후 runtime activation=%d", got)
			}
		})
	}
}

type memoryStore struct {
	mu   sync.Mutex
	recs []gen.EventRecord
}

func (s *memoryStore) LastSeq(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.recs)), nil
}
func (s *memoryStore) ReadFrom(context.Context, int64) ([]gen.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gen.EventRecord(nil), s.recs...), nil
}
func (s *memoryStore) Append(_ context.Context, rec gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
	return nil
}
func (s *memoryStore) AppendBatch(_ context.Context, recs []gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, recs...)
	return nil
}
func (s *memoryStore) Close() error { return nil }
func (s *memoryStore) records() []gen.EventRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gen.EventRecord(nil), s.recs...)
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s 대기 timeout", what)
	}
}

func waitError(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("%s 대기 timeout", what)
		return nil
	}
}
