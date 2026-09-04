package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
)

const (
	fakeAgentID         = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fakeProxyID         = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	testAgentRepository = "localhost/hx-agent"
	testProxyRepository = "localhost/hx-egress-proxy"
	testProxyDigest     = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
)

type fakePodman struct {
	mu        sync.Mutex
	calls     [][]string
	envCalls  []fakeEnvCall
	notify    chan struct{}
	failures  map[string]error
	hook      func([]string) ([]byte, error, bool)
	digest    string
	imageUser string
	proxyUser string
}

type fakeEnvCall struct {
	env  []string
	args []string
}

func newFakePodman(digest string) *fakePodman {
	return &fakePodman{
		notify: make(chan struct{}, 64), failures: map[string]error{}, digest: digest,
		imageUser: "1000:1001", proxyUser: "65532:65532",
	}
}

func (f *fakePodman) Run(ctx context.Context, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	copyArgs := append([]string(nil), args...)
	f.mu.Lock()
	f.calls = append(f.calls, copyArgs)
	hook := f.hook
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
	if hook != nil {
		if out, err, handled := hook(copyArgs); handled {
			return out, err
		}
	}
	key := commandKey(args)
	if err := f.failures[key]; err != nil {
		return nil, err
	}
	switch key {
	case "info":
		return []byte(`{"host":{"security":{"rootless":true}},"store":{"graphDriverName":"overlay","graphStatus":{"Native Overlay Diff":"true"}}}`), nil
	case "image inspect":
		requested := args[2]
		digest, user := f.digest, f.imageUser
		if requested == testProxyRepository+"@"+testProxyDigest {
			digest, user = testProxyDigest, f.proxyUser
		}
		return []byte(`[{"Digest":"` + digest + `","Config":{"User":"` + user + `"}}]`), nil
	case "proxy create":
		return []byte(fakeProxyID + "\n"), nil
	case "agent create":
		return []byte(fakeAgentID + "\n"), nil
	case "unshare":
		if len(args) != 5 || args[1] != "rm" || args[2] != "-rf" || args[3] != "--" {
			return nil, fmt.Errorf("unexpected unshare argv: %v", args)
		}
		return nil, os.RemoveAll(args[4])
	default:
		return nil, nil
	}
}

func (f *fakePodman) RunWithEnv(ctx context.Context, env []string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.envCalls = append(f.envCalls, fakeEnvCall{env: append([]string(nil), env...), args: append([]string(nil), args...)})
	f.mu.Unlock()
	return f.Run(ctx, args...)
}

func (f *fakePodman) envSnapshot() []fakeEnvCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeEnvCall, len(f.envCalls))
	for i := range f.envCalls {
		out[i] = fakeEnvCall{env: append([]string(nil), f.envCalls[i].env...), args: append([]string(nil), f.envCalls[i].args...)}
	}
	return out
}

func (f *fakePodman) Start(context.Context, ...string) (startedCommand, error) {
	return nil, errors.New("fakePodman: streaming Start는 process broker 전용 테스트에서만 사용")
}

func commandKey(args []string) string {
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		return "image inspect"
	}
	if len(args) == 0 {
		return ""
	}
	if args[0] == "network" && len(args) >= 2 {
		return "network " + args[1]
	}
	if args[0] == "create" {
		if containsArgValue(args, "--name", "hx-2222222222222222-proxy") {
			return "proxy create"
		}
		return "agent create"
	}
	if len(args) >= 2 {
		switch args[0] {
		case "start", "stop", "wait", "rm":
			id := args[len(args)-1]
			if id == fakeProxyID {
				return "proxy " + args[0]
			}
			return "agent " + args[0]
		}
	}
	return args[0]
}

func containsArgValue(args []string, name, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name && args[i+1] == value {
			return true
		}
	}
	return false
}

func (f *fakePodman) snapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i := range f.calls {
		out[i] = append([]string(nil), f.calls[i]...)
	}
	return out
}

func TestOpenBuildsRootlessOverlayAndMetadata(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	leaseValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	got := leaseValue.(*lease)

	calls := runner.snapshot()
	if gotKeys := callKeys(calls); !reflect.DeepEqual(gotKeys, []string{
		"info", "image inspect", "image inspect", "network create", "network create",
		"proxy create", "proxy start", "agent create",
	}) {
		t.Fatalf("Podman 호출 순서 = %v", gotKeys)
	}
	if calls[1][2] != testAgentRepository+"@"+digest || calls[2][2] != testProxyRepository+"@"+testProxyDigest {
		t.Fatalf("image inspect가 repository@digest를 사용하지 않음: agent=%q proxy=%q", calls[1][2], calls[2][2])
	}
	proxyCreate := strings.Join(calls[5], " ")
	for _, required := range []string{
		"--network hx-2222222222222222-internal", "--network hx-2222222222222222-egress",
		"--read-only", "--cap-drop=all", "--security-opt=no-new-privileges",
		":/run/hx-audit:ro", testProxyRepository + "@" + testProxyDigest, proxyExecutable, "--allow api.example.com",
	} {
		if !strings.Contains(proxyCreate, required) {
			t.Errorf("proxy create args에 %q 없음: %s", required, proxyCreate)
		}
	}
	create := strings.Join(calls[7], " ")
	for _, required := range []string{
		"--interactive", "--pull=never", "--network hx-2222222222222222-internal",
		"--userns=keep-id:uid=1000,gid=1001", "--user 1000:1001",
		"--workdir /workspace", ":/workspace:O,upperdir=", ",workdir=", testAgentRepository + "@" + digest,
		"HTTP_PROXY=http://hx-2222222222222222-proxy:3128",
	} {
		if !strings.Contains(create, required) {
			t.Errorf("create args에 %q 없음: %s", required, create)
		}
	}
	if strings.Contains(create, ":U") {
		t.Fatalf("lower를 chown하는 :U가 포함됨: %s", create)
	}
	if strings.Contains(create, "hx-2222222222222222-egress") || strings.Contains(create, "/run/hx-audit") {
		t.Fatalf("agent가 external network 또는 audit endpoint를 받음: %s", create)
	}

	metadata := got.metadata.Clone()
	if metadata.Backend != gen.SubagentSpawnPayloadWorldBackendLocalPodman ||
		metadata.ProfileID != "profile" || metadata.ImageDigest != digest || len(metadata.Mounts) != 1 {
		t.Fatalf("spawn metadata 이상: %+v", metadata)
	}
	mount := metadata.Mounts[0]
	resolvedLower, _ := filepath.EvalSymlinks(lower)
	if mount.SourcePath != resolvedLower || mount.TargetPath != gen.SubagentSpawnMountTargetPathWorkspace ||
		mount.Mode != gen.SubagentSpawnMountModeOverlay || filepath.IsAbs(mount.UpperRef) {
		t.Fatalf("mount metadata 이상: %+v", mount)
	}
	processEndpoint, approvalEndpoint := got.ProcessEndpoint(), got.ApprovalEndpoint()
	if processEndpoint.Network() != "unix" || processEndpoint.Address() == "" ||
		processEndpoint.ControlCapability() == "" || processEndpoint.OutputCapability() == "" ||
		approvalEndpoint.Network() != "unix" || approvalEndpoint.Address() == "" || approvalEndpoint.Capability() == "" {
		t.Fatal("process/approval endpoint가 완전하지 않음")
	}
	for _, secret := range []string{processEndpoint.Address(), processEndpoint.ControlCapability(), processEndpoint.OutputCapability(), approvalEndpoint.Address(), approvalEndpoint.Capability()} {
		if strings.Contains(create, secret) {
			t.Fatalf("agent create args에 host endpoint/capability가 노출됨: %q", secret)
		}
	}
	if pathWithin(got.approval.RelayDir(), approvalEndpoint.Address()) {
		t.Fatalf("host adapter socket이 agent에 mount되는 relay subtree 안에 있음: relay=%q host=%q",
			got.approval.RelayDir(), approvalEndpoint.Address())
	}
	for _, required := range []string{
		"HX_APPROVAL_SOCKET=" + approvalRelayPath,
		":" + approvalRelayMount + ":ro",
	} {
		if !strings.Contains(create, required) {
			t.Errorf("agent create args에 approval relay %q 없음: %s", required, create)
		}
	}
	for _, dir := range []string{got.stateDir, got.upperDir, got.workDir} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("state dir %q mode/존재 이상: mode=%v err=%v", dir, infoMode(info), err)
		}
	}
}

func TestSecretCapabilityInjectedOnceViaEnvironmentNotArgv(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	capability, err := world.NewSecretCapability(world.ClaudeOAuthTokenEnv, "synthetic-token-not-an-argv", time.Now().Add(10*time.Minute).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec(lower, digest).WithSecretCapability(capability)
	leaseValue, err := openTestLease(t, backend, context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseValue.Close(context.Background())
	calls := runner.snapshot()
	for _, call := range calls {
		if strings.Contains(strings.Join(call, " "), capability.Value()) {
			t.Fatalf("secret이 Podman argv에 노출됨: %v", call)
		}
	}
	envCalls := runner.envSnapshot()
	if len(envCalls) != 1 {
		t.Fatalf("secret 환경 주입 횟수=%d, want 1", len(envCalls))
	}
	if !containsArgValue(envCalls[0].args, "--env", world.ClaudeOAuthTokenEnv) {
		t.Fatalf("agent create가 이름 기반 env 전달을 사용하지 않음: %v", envCalls[0].args)
	}
	if len(envCalls[0].env) != 1 || envCalls[0].env[0] != world.ClaudeOAuthTokenEnv+"="+capability.Value() {
		t.Fatalf("환경 주입 값/횟수 이상: %+v", envCalls[0].env)
	}
	metadata, err := json.Marshal(leaseValue.(*lease).metadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), capability.Value()) {
		t.Fatalf("spawn metadata에 secret이 노출됨: %s", metadata)
	}
}

func TestMergeEnvironmentReplacesAmbientCredential(t *testing.T) {
	merged := mergeEnvironment([]string{"PATH=/bin", world.ClaudeOAuthTokenEnv + "=ambient"}, []string{world.ClaudeOAuthTokenEnv + "=synthetic"})
	count := 0
	for _, item := range merged {
		if strings.HasPrefix(item, world.ClaudeOAuthTokenEnv+"=") {
			count++
			if item != world.ClaudeOAuthTokenEnv+"=synthetic" {
				t.Fatalf("ambient credential가 유지됨: %v", merged)
			}
		}
	}
	if count != 1 {
		t.Fatalf("credential 환경 항목 수=%d: %v", count, merged)
	}
}

func TestExpiredSecretFailsBeforeRuntimeSideEffects(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	capability, err := world.NewSecretCapability(world.ClaudeOAuthTokenEnv, "synthetic-expired-token", time.Now().Add(time.Second).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	_, err = openTestLease(t, backend, context.Background(), testSpec(lower, digest).WithSecretCapability(capability))
	if err == nil {
		t.Fatal("예산보다 빨리 만료되는 secret을 수락함")
	}
	if containsCall(runner.snapshot(), "network create") || containsAnyCreate(runner.snapshot()) {
		t.Fatalf("secret 만료가 runtime side effect까지 도달함: %v", callKeys(runner.snapshot()))
	}
	if strings.Contains(err.Error(), capability.Value()) {
		t.Fatalf("만료 오류에 secret 원문이 노출됨: %v", err)
	}
}

func TestActivateExpiryRecheckCleansPreparedState(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	good, err := world.NewSecretCapability(world.ClaudeOAuthTokenEnv, "synthetic-activate-token", time.Now().Add(10*time.Minute).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	preparedValue, err := backend.Prepare(context.Background(), testSpec(lower, digest).WithSecretCapability(good))
	if err != nil {
		t.Fatal(err)
	}
	prepared := preparedValue.(*preparedLease)
	stateDir := prepared.layout.stateDir
	receipt, writer, err := commitPreparedForTest(testSpec(lower, digest), prepared, &localMemoryStore{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	expired, err := world.NewSecretCapability(world.ClaudeOAuthTokenEnv, "synthetic-activate-token", time.Now().Add(time.Second).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	prepared.spec = prepared.spec.WithSecretCapability(expired)
	if _, err := prepared.Activate(context.Background(), receipt); err == nil {
		t.Fatal("Activate가 만료된 secret 재검사를 통과시킴")
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Activate 만료 오류 후 prepared state가 남음: %v", err)
	}
	if containsCall(runner.snapshot(), "network create") || containsAnyCreate(runner.snapshot()) {
		t.Fatalf("Activate 만료가 runtime side effect까지 도달함: %v", callKeys(runner.snapshot()))
	}
}

func TestSecretBudgetValidationFailsClosed(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	capability, err := world.NewSecretCapability(world.ClaudeOAuthTokenEnv, "synthetic-budget-token", 1_000_000+31_000)
	if err != nil {
		t.Fatal(err)
	}
	effective := world.NewEffectivePolicy(policy.SandboxConfig{Budget: gen.Budget{TimeMs: 1000}})
	spec := world.NewSpawnSpec(effective, world.NewImageReference(testAgentRepository, digest), []string{"agent"}, 0, strings.Repeat("1", 32), strings.Repeat("2", 16), world.AgentIdentity{UID: 1000, GID: 1001}, nil).WithSecretCapability(capability)
	if err := validateSecretCapability(spec, time.UnixMilli(1_000_000)); err != nil {
		t.Fatal("충분한 만료 여유를 거부함: ", err)
	}
	if err := validateSecretCapability(spec, time.UnixMilli(1_000_001)); err == nil {
		t.Fatal("clock-skew를 포함한 만료 경계를 거부하지 않음")
	}
}

func TestPrepareCreatesNoRuntimeObjectsAndAbortCleansState(t *testing.T) {
	digest := "sha256:" + strings.Repeat("0", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	prepared, err := backend.Prepare(context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	if got := callKeys(runner.snapshot()); !reflect.DeepEqual(got, []string{"info", "image inspect", "image inspect"}) {
		t.Fatalf("Prepare가 runtime side effect를 만듦: %v", got)
	}
	upper := prepared.UpperDir()
	if _, err := os.Stat(upper); err != nil {
		t.Fatalf("prepared upper 없음: %v", err)
	}
	if err := prepared.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Dir(filepath.Dir(upper))
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Abort 뒤 state 잔존: %v", err)
	}
}

func TestReceiptFailuresDoNotCreatePodmanResources(t *testing.T) {
	digest := "sha256:" + strings.Repeat("4", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	specA := testSpec(lower, digest)
	preparedAValue, err := backend.Prepare(context.Background(), specA)
	if err != nil {
		t.Fatal(err)
	}
	preparedA := preparedAValue.(*preparedLease)
	before := len(runner.snapshot())
	if _, err := preparedA.Activate(context.Background(), world.SpawnReceipt{}); err == nil {
		t.Fatal("zero receipt 수용")
	}
	if len(runner.snapshot()) != before {
		t.Fatal("zero receipt가 Podman side effect를 만듦")
	}
	receiptA, writerA, err := commitPreparedForTest(specA, preparedA, &localMemoryStore{})
	if err != nil {
		t.Fatal(err)
	}
	defer writerA.Close()
	specB := world.NewSpawnSpec(specA.Policy(), specA.Image(), specA.AgentArgv(), specA.Depth(), specA.TraceID(), strings.Repeat("4", 16), specA.AgentIdentity(), specA.Credentials())
	preparedBValue, err := backend.Prepare(context.Background(), specB)
	if err != nil {
		t.Fatal(err)
	}
	preparedB := preparedBValue.(*preparedLease)
	before = len(runner.snapshot())
	if _, err := preparedB.Activate(context.Background(), receiptA); err == nil {
		t.Fatal("cross-lease receipt 수용")
	}
	if len(runner.snapshot()) != before {
		t.Fatal("cross-lease receipt가 Podman side effect를 만듦")
	}
	if err := preparedB.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	active, err := preparedA.Activate(context.Background(), receiptA)
	if err != nil {
		t.Fatal(err)
	}
	before = len(runner.snapshot())
	if _, err := preparedA.Activate(context.Background(), receiptA); err == nil {
		t.Fatal("reused receipt 수용")
	}
	if len(runner.snapshot()) != before {
		t.Fatal("reused receipt가 Podman side effect를 반복함")
	}
	if err := active.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionReceiptFailuresPreserveUpper(t *testing.T) {
	digest := "sha256:" + strings.Repeat("7", 64)

	t.Run("zero receipt", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		activeValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
		if err != nil {
			t.Fatal(err)
		}
		active := activeValue.(*lease)
		if err := active.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		upper := active.UpperDir()
		if err := active.AcknowledgeCollection(world.CollectionReceipt{}); err == nil {
			t.Fatal("zero collection receipt가 수용됨")
		}
		assertUpperPreserved(t, upper)
	})

	t.Run("cross lease receipt", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		specA := testSpec(lower, digest)
		specB := world.NewSpawnSpec(specA.Policy(), specA.Image(), specA.AgentArgv(), specA.Depth(), specA.TraceID(), strings.Repeat("4", 16), specA.AgentIdentity(), specA.Credentials())
		activeAValue, err := openTestLease(t, backend, context.Background(), specA)
		if err != nil {
			t.Fatal(err)
		}
		activeBValue, err := openTestLease(t, backend, context.Background(), specB)
		if err != nil {
			t.Fatal(err)
		}
		activeA, activeB := activeAValue.(*lease), activeBValue.(*lease)
		if err := activeA.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := activeB.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		receiptA := commitCollectionReceiptForTest(t, activeA)
		upperB := activeB.UpperDir()
		if err := activeB.AcknowledgeCollection(receiptA); err == nil {
			t.Fatal("cross-lease collection receipt가 수용됨")
		}
		assertUpperPreserved(t, upperB)
		if err := activeA.AcknowledgeCollection(receiptA); err != nil {
			t.Fatalf("원래 lease의 collection receipt가 거부됨: %v", err)
		}
	})

	t.Run("receipt reuse", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		activeValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
		if err != nil {
			t.Fatal(err)
		}
		active := activeValue.(*lease)
		if err := active.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		receipt := commitCollectionReceiptForTest(t, active)
		if err := active.AcknowledgeCollection(receipt); err != nil {
			t.Fatalf("첫 collection ACK가 실패함: %v", err)
		}
		cleanupCalls := countCall(callKeys(runner.snapshot()), "unshare")
		if err := active.AcknowledgeCollection(receipt); err == nil {
			t.Fatal("재사용 collection receipt가 수용됨")
		}
		if got := countCall(callKeys(runner.snapshot()), "unshare"); got != cleanupCalls {
			t.Fatalf("재사용 receipt가 upper cleanup을 반복함: %d → %d", cleanupCalls, got)
		}
	})
}

func TestCollectionReceiptIsOneShot(t *testing.T) {
	digest := "sha256:" + strings.Repeat("8", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	activeValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	active := activeValue.(*lease)
	if err := active.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt := commitCollectionReceiptForTest(t, active)
	if err := active.AcknowledgeCollection(receipt); err != nil {
		t.Fatalf("첫 collection ACK가 실패함: %v", err)
	}
	cleanupCalls := countCall(callKeys(runner.snapshot()), "unshare")
	err = active.AcknowledgeCollection(receipt)
	if err == nil || !strings.Contains(err.Error(), "이미 소비됨") {
		t.Fatalf("같은 lease에서 receipt 재사용이 명확한 오류로 거부되지 않음: %v", err)
	}
	if got := countCall(callKeys(runner.snapshot()), "unshare"); got != cleanupCalls {
		t.Fatalf("재사용 receipt가 upper cleanup을 반복함: %d → %d", cleanupCalls, got)
	}
}

func TestCollectionReceiptRequiresClose(t *testing.T) {
	digest := "sha256:" + strings.Repeat("9", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	activeValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	active := activeValue.(*lease)
	receipt := commitCollectionReceiptForTest(t, active)
	upper := active.UpperDir()
	if err := active.AcknowledgeCollection(receipt); err == nil || !strings.Contains(err.Error(), "Close 이후") {
		t.Fatalf("Close 전 collection ACK가 명확한 오류로 거부되지 않음: %v", err)
	}
	assertUpperPreserved(t, upper)
	if got := countCall(callKeys(runner.snapshot()), "unshare"); got != 0 {
		t.Fatalf("Close 전 collection ACK가 upper cleanup을 수행함: %d", got)
	}
	if err := active.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func commitCollectionReceiptForTest(t *testing.T, active *lease) world.CollectionReceipt {
	t.Helper()
	store := &localMemoryStore{}
	writer, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(gen.FsChangedPayload{Changes: []gen.FsChangedPayloadChangesItem{}})
	if err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	raw := ""
	record := gen.EventRecord{TraceID: strings.Repeat("a", 32), SpanID: active.spanID, Ts: 2,
		Kind: gen.KindCollectorFsChanged, Actor: "collector", Payload: payload, Raw: &raw}
	receipt, err := world.CommitCollection(context.Background(), writer, active, record)
	if closeErr := writer.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func assertUpperPreserved(t *testing.T, upper string) {
	t.Helper()
	if _, err := os.Stat(upper); err != nil {
		t.Fatalf("잘못된 collection receipt 뒤 upper가 제거됨: %v", err)
	}
}

func TestSpawnCommitFailureLeavesContainerCreateAtZero(t *testing.T) {
	digest := "sha256:" + strings.Repeat("5", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	spec := testSpec(lower, digest)
	preparedValue, err := backend.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	prepared := preparedValue.(*preparedLease)
	storeErr := errors.New("durable append failed")
	_, writer, err := commitPreparedForTest(spec, prepared, &localMemoryStore{appendErr: storeErr})
	if writer != nil {
		_ = writer.Close()
	}
	if !errors.Is(err, storeErr) {
		t.Fatalf("commit err=%v", err)
	}
	if containsAnyCreate(runner.snapshot()) || containsCall(runner.snapshot(), "network create") {
		t.Fatalf("record failure 뒤 runtime side effect: %v", callKeys(runner.snapshot()))
	}
	if err := prepared.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnDurableAckPrecedesRuntimeCreation(t *testing.T) {
	digest := "sha256:" + strings.Repeat("6", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	var mu sync.Mutex
	acked, createBeforeAck := false, false
	runner.hook = func(args []string) ([]byte, error, bool) {
		if commandKey(args) == "network create" || commandKey(args) == "proxy create" || commandKey(args) == "agent create" {
			mu.Lock()
			if !acked {
				createBeforeAck = true
			}
			mu.Unlock()
		}
		return nil, nil, false
	}
	backend := mustBackend(t, stateRoot, runner, statDevice)
	spec := testSpec(lower, digest)
	preparedValue, err := backend.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	prepared := preparedValue.(*preparedLease)
	store := &localMemoryStore{appendHook: func() { mu.Lock(); acked = true; mu.Unlock() }}
	receipt, writer, err := commitPreparedForTest(spec, prepared, store)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	active, err := prepared.Activate(context.Background(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	violated := createBeforeAck
	mu.Unlock()
	if violated {
		t.Fatal("runtime create가 durable spawn ACK보다 먼저 발생함")
	}
	if containsCall(runner.snapshot(), "agent start") {
		t.Fatal("Activate가 process START 전에 agent를 실행함")
	}
	if err := active.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseCloseWaitsForEffectsThenCleansContainerAndPreservesUpper(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	leaseValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	got := leaseValue.(*lease)
	effectsDone := make(chan struct{})
	got.broker.(*fakeEffectBroker).shutdownGate = effectsDone

	closeResult := make(chan error, 1)
	go func() { closeResult <- got.Close(context.Background()) }()
	waitForCall(t, runner, "proxy wait")
	if containsCall(runner.snapshot(), "agent rm") || containsCall(runner.snapshot(), "network rm") {
		t.Fatal("effect drain/ACK 전에 container cleanup이 시작됨")
	}
	close(effectsDone)
	if err := receiveError(t, closeResult, "Lease.Close"); err != nil {
		t.Fatal(err)
	}
	keys := callKeys(runner.snapshot())
	if !ordered(keys, "proxy stop", "proxy wait", "agent rm", "proxy rm", "network rm", "network rm") {
		t.Fatalf("Close Podman 순서 이상: %v", keys)
	}
	wantCleanup := []string{"unshare", "rm", "-rf", "--", got.workDir}
	foundCleanup := false
	for _, call := range runner.snapshot() {
		if reflect.DeepEqual(call, wantCleanup) {
			foundCleanup = true
			break
		}
	}
	if !foundCleanup {
		t.Fatalf("Close가 exact work capability로 cleanup하지 않음: calls=%v", runner.snapshot())
	}
	if _, err := os.Stat(got.upperDir); err != nil {
		t.Fatalf("collector ACK 전 upper가 삭제됨: %v", err)
	}
	if _, err := os.Stat(got.workDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("container cleanup 뒤 workdir가 남음: %v", err)
	}
	if _, err := os.Stat(got.broker.SocketDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proxy cleanup 뒤 audit socket dir가 남음: %v", err)
	}
	before := len(runner.snapshot())
	if err := got.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := len(runner.snapshot()); after != before {
		t.Fatalf("두 번째 Close가 lifecycle을 반복함: %d → %d", before, after)
	}
}

func TestCloseContinuesAfterStopError(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	stopErr := errors.New("stop failed")
	runner.failures["proxy stop"] = stopErr
	backend := mustBackend(t, stateRoot, runner, statDevice)
	leaseValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	err = leaseValue.Close(context.Background())
	if !errors.Is(err, stopErr) {
		t.Fatalf("stop 오류 체인 소실: %v", err)
	}
	if !ordered(callKeys(runner.snapshot()), "proxy stop", "proxy wait", "agent rm", "proxy rm") {
		t.Fatalf("stop 오류가 cleanup을 단락함: %v", callKeys(runner.snapshot()))
	}
}

func TestCloseAttemptsRuntimeCleanupAfterEveryStageError(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	tests := []struct {
		name   string
		inject func(*lease, *fakePodman) error
		ctx    func() context.Context
	}{
		{"approval drain", func(got *lease, _ *fakePodman) error {
			err := errors.New("approval drain failed")
			got.approval.mu.Lock()
			got.approval.firstErr = err
			got.approval.mu.Unlock()
			return err
		}, nil},
		{"process shutdown", func(got *lease, _ *fakePodman) error {
			err := errors.New("process shutdown failed")
			got.process.mu.Lock()
			got.process.firstErr = err
			got.process.mu.Unlock()
			return err
		}, nil},
		{"proxy stop", func(_ *lease, runner *fakePodman) error {
			err := errors.New("proxy stop failed")
			runner.failures["proxy stop"] = err
			return err
		}, nil},
		{"proxy wait", func(_ *lease, runner *fakePodman) error {
			err := errors.New("proxy wait failed")
			runner.failures["proxy wait"] = err
			return err
		}, nil},
		{"audit drain", func(got *lease, _ *fakePodman) error {
			err := errors.New("audit drain failed")
			got.broker.(*fakeEffectBroker).shutdownErr = err
			return err
		}, nil},
		{"work cleanup", func(_ *lease, runner *fakePodman) error {
			err := errors.New("unshare failed")
			runner.failures["unshare"] = err
			return err
		}, nil},
		{"caller canceled", func(_ *lease, _ *fakePodman) error { return context.Canceled }, func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lower, stateRoot := testDirs(t)
			runner := newFakePodman(digest)
			backend := mustBackend(t, stateRoot, runner, statDevice)
			leaseValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
			if err != nil {
				t.Fatal(err)
			}
			got := leaseValue.(*lease)
			wantErr := test.inject(got, runner)
			closeCtx := context.Background()
			if test.ctx != nil {
				closeCtx = test.ctx()
			}
			err = got.Close(closeCtx)
			if !errors.Is(err, wantErr) {
				t.Fatalf("오류 체인 소실: got=%v want=%v", err, wantErr)
			}
			keys := callKeys(runner.snapshot())
			for _, required := range []string{"agent rm", "proxy rm"} {
				if !containsCall(runner.snapshot(), required) {
					t.Fatalf("%s 뒤 cleanup 누락 %s: %v", test.name, required, keys)
				}
			}
			if countCall(keys, "network rm") != 2 {
				t.Fatalf("%s 뒤 network cleanup 누락: %v", test.name, keys)
			}
		})
	}
}

func TestConcurrentCloseHonorsContextWhileLifecycleIsGated(t *testing.T) {
	digest := "sha256:" + strings.Repeat("8", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	leaseValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	got := leaseValue.(*lease)
	effectsDone := make(chan struct{})
	got.broker.(*fakeEffectBroker).shutdownGate = effectsDone
	first := make(chan error, 1)
	go func() { first <- got.Close(context.Background()) }()
	waitForCall(t, runner, "proxy wait")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := got.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("동시 Close가 자신의 context를 지키지 않음: %v", err)
	}
	close(effectsDone)
	if err := receiveError(t, first, "첫 Lease.Close"); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledCloseStillAttemptsEveryCleanupPhase(t *testing.T) {
	digest := "sha256:" + strings.Repeat("6", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	leaseValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := leaseValue.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("취소 Close = %v", err)
	}
	if !ordered(callKeys(runner.snapshot()), "proxy stop", "proxy wait", "agent rm", "proxy rm") {
		t.Fatalf("취소 lifecycle cleanup 누락: %v", callKeys(runner.snapshot()))
	}
}

func TestOpenFailsClosedBeforeContainerStart(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	t.Run("invalid egress allowlist", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		effective := world.NewEffectivePolicy(policy.SandboxConfig{
			ProfileID: "profile", Workspace: lower, FSScope: []string{lower}, Egress: []string{"*.example.com"},
			Budget: gen.Budget{Tokens: 10, TimeMs: 1000, MaxDepth: 2}, Approval: policy.ApprovalManual,
		})
		spec := world.NewSpawnSpec(
			effective, world.NewImageReference(testAgentRepository, digest), []string{"agent"}, 0, strings.Repeat("1", 32), strings.Repeat("2", 16),
			world.AgentIdentity{UID: 1000, GID: 1001}, nil,
		)
		_, err := openTestLease(t, backend, context.Background(), spec)
		if err == nil || containsAnyCreate(runner.snapshot()) || containsCall(runner.snapshot(), "network create") {
			t.Fatalf("위반 allowlist가 network/container 생성까지 도달함: err=%v calls=%v", err, callKeys(runner.snapshot()))
		}
	})

	t.Run("tag", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := openTestLease(t, backend, context.Background(), testSpec(lower, "alpine:latest"))
		if err == nil || containsAnyCreate(runner.snapshot()) {
			t.Fatalf("tag가 container create까지 도달함: err=%v calls=%v", err, callKeys(runner.snapshot()))
		}
	})

	t.Run("inspect digest mismatch", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman("sha256:" + strings.Repeat("e", 64))
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
		if err == nil || containsAnyCreate(runner.snapshot()) {
			t.Fatalf("digest 불일치가 create까지 도달함: err=%v", err)
		}
	})

	for name, imageUser := range map[string]string{
		"named image user":        "agent:agent",
		"image identity mismatch": "1000:1002",
	} {
		t.Run(name, func(t *testing.T) {
			lower, stateRoot := testDirs(t)
			runner := newFakePodman(digest)
			runner.imageUser = imageUser
			backend := mustBackend(t, stateRoot, runner, statDevice)
			_, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
			if err == nil || containsAnyCreate(runner.snapshot()) {
				t.Fatalf("부적합 image user가 create까지 도달함: err=%v", err)
			}
		})
	}

	t.Run("symlink scope escape", func(t *testing.T) {
		base := t.TempDir()
		scope := filepath.Join(base, "scope")
		outside := filepath.Join(base, "outside")
		stateRoot := filepath.Join(base, "state")
		mustMkdir(t, scope, 0o755)
		mustMkdir(t, outside, 0o755)
		mustMkdir(t, stateRoot, 0o700)
		link := filepath.Join(scope, "link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		spec := specWithScope(link, []string{scope}, digest)
		_, err := openTestLease(t, backend, context.Background(), spec)
		if err == nil || containsAnyCreate(runner.snapshot()) {
			t.Fatalf("symlink 탈출이 create까지 도달함: err=%v", err)
		}
	})

	t.Run("state root below lower", func(t *testing.T) {
		lower := t.TempDir()
		stateRoot := filepath.Join(lower, "state")
		mustMkdir(t, stateRoot, 0o700)
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
		if err == nil || containsAnyCreate(runner.snapshot()) {
			t.Fatalf("lower 내부 state root가 create까지 도달함: err=%v", err)
		}
	})

	t.Run("upper work device mismatch", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		var n int
		device := func(string) (uint64, error) {
			n++
			return uint64(n), nil
		}
		backend := mustBackend(t, stateRoot, runner, device)
		_, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
		if err == nil || containsAnyCreate(runner.snapshot()) {
			t.Fatalf("device mismatch가 create까지 도달함: err=%v", err)
		}
	})

	t.Run("unsafe volume grammar", func(t *testing.T) {
		base := t.TempDir()
		lower := filepath.Join(base, "lower,comma")
		stateRoot := filepath.Join(base, "state")
		mustMkdir(t, lower, 0o755)
		mustMkdir(t, stateRoot, 0o700)
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
		if err == nil || containsAnyCreate(runner.snapshot()) {
			t.Fatalf("unsafe path가 create까지 도달함: err=%v", err)
		}
	})
}

func TestBackendAndPodmanPreconditionsFailClosed(t *testing.T) {
	digest := "sha256:" + strings.Repeat("9", 64)
	t.Run("proxy image tag", func(t *testing.T) {
		_, stateRoot := testDirs(t)
		config := testConfig(stateRoot)
		config.ProxyImageDigest = "hxegressproxy:latest"
		if _, err := newBackend(config, newFakePodman(digest), statDevice, newFakeEffectBroker); err == nil {
			t.Fatal("proxy image tag를 수용함")
		}
	})
	t.Run("proxy repository tag", func(t *testing.T) {
		_, stateRoot := testDirs(t)
		config := testConfig(stateRoot)
		config.ProxyImageRepository = "localhost/hx-egress-proxy:latest"
		if _, err := newBackend(config, newFakePodman(digest), statDevice, newFakeEffectBroker); err == nil {
			t.Fatal("tag가 포함된 proxy repository를 수용함")
		}
	})
	t.Run("root proxy identity", func(t *testing.T) {
		_, stateRoot := testDirs(t)
		config := testConfig(stateRoot)
		config.ProxyIdentity = world.AgentIdentity{}
		if _, err := newBackend(config, newFakePodman(digest), statDevice, newFakeEffectBroker); err == nil {
			t.Fatal("root/영값 proxy identity를 수용함")
		}
	})
	t.Run("state root mode", func(t *testing.T) {
		stateRoot := filepath.Join(t.TempDir(), "state")
		mustMkdir(t, stateRoot, 0o755)
		if _, err := newBackend(testConfig(stateRoot), newFakePodman(digest), statDevice, newFakeEffectBroker); err == nil {
			t.Fatal("0700이 아닌 state root를 수용함")
		}
	})

	for name, infoJSON := range map[string]string{
		"not rootless":       `{"host":{"security":{"rootless":false}},"store":{"graphDriverName":"overlay","graphStatus":{"Native Overlay Diff":"true"}}}`,
		"not native overlay": `{"host":{"security":{"rootless":true}},"store":{"graphDriverName":"overlay","graphStatus":{"Native Overlay Diff":"false"}}}`,
		"wrong graph driver": `{"host":{"security":{"rootless":true}},"store":{"graphDriverName":"vfs","graphStatus":{"Native Overlay Diff":"true"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			lower, stateRoot := testDirs(t)
			runner := newFakePodman(digest)
			runner.hook = func(args []string) ([]byte, error, bool) {
				if commandKey(args) == "info" {
					return []byte(infoJSON), nil, true
				}
				return nil, nil, false
			}
			backend := mustBackend(t, stateRoot, runner, statDevice)
			_, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
			if err == nil || containsAnyCreate(runner.snapshot()) {
				t.Fatalf("부적합 preflight가 create까지 도달함: err=%v calls=%v", err, callKeys(runner.snapshot()))
			}
		})
	}
}

func TestCreateAndStartFailuresCleanResources(t *testing.T) {
	digest := "sha256:" + strings.Repeat("f", 64)
	t.Run("proxy start error", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		runner.failures["proxy start"] = errors.New("proxy start failed")
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
		if err == nil || containsCall(runner.snapshot(), "agent create") ||
			!ordered(callKeys(runner.snapshot()), "proxy create", "proxy start", "proxy rm", "network rm", "network rm") {
			t.Fatalf("proxy start 오류 cleanup 누락: err=%v calls=%v", err, callKeys(runner.snapshot()))
		}
		assertNoSpawnState(t, stateRoot)
	})

	t.Run("proxy ready error", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		readyErr := errors.New("proxy ready failed")
		factory := func(stateDir, spanID string, capacity int) (effectBroker, error) {
			value, err := newFakeEffectBroker(stateDir, spanID, capacity)
			if err == nil {
				value.(*fakeEffectBroker).readyErr = readyErr
			}
			return value, err
		}
		backend, err := newBackend(testConfig(stateRoot), runner, statDevice, factory)
		if err != nil {
			t.Fatal(err)
		}
		_, err = openTestLease(t, backend, context.Background(), testSpec(lower, digest))
		if !errors.Is(err, readyErr) || containsCall(runner.snapshot(), "agent create") ||
			!ordered(callKeys(runner.snapshot()), "proxy start", "proxy rm", "network rm", "network rm") {
			t.Fatalf("proxy ready 오류 cleanup 누락: err=%v calls=%v", err, callKeys(runner.snapshot()))
		}
		assertNoSpawnState(t, stateRoot)
	})

	t.Run("create error with cidfile", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		runner.hook = func(args []string) ([]byte, error, bool) {
			if commandKey(args) != "agent create" {
				return nil, nil, false
			}
			for i := range args {
				if args[i] == "--cidfile" && i+1 < len(args) {
					if err := os.WriteFile(args[i+1], []byte(fakeAgentID), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			return nil, errors.New("create failed after allocation"), true
		}
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
		if err == nil || !containsCall(runner.snapshot(), "agent rm") {
			t.Fatalf("create 오류 orphan cleanup 누락: err=%v calls=%v", err, callKeys(runner.snapshot()))
		}
		assertNoSpawnState(t, stateRoot)
	})

	t.Run("activation leaves agent created until broker start", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		leaseValue, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
		if err != nil {
			t.Fatal(err)
		}
		if !containsCall(runner.snapshot(), "agent create") || containsCall(runner.snapshot(), "agent start") {
			t.Fatalf("Activate가 START frame 전에 agent를 실행함: calls=%v", callKeys(runner.snapshot()))
		}
		if err := leaseValue.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func testDirs(t *testing.T) (lower, stateRoot string) {
	t.Helper()
	base := t.TempDir()
	lower, stateRoot = filepath.Join(base, "lower"), filepath.Join(base, "state")
	mustMkdir(t, lower, 0o755)
	mustMkdir(t, stateRoot, 0o700)
	return lower, stateRoot
}

func mustMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mustBackend(t *testing.T, stateRoot string, runner commandRunner, device func(string) (uint64, error)) *Backend {
	t.Helper()
	backend, err := newBackend(testConfig(stateRoot), runner, device, newFakeEffectBroker)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func testConfig(stateRoot string) Config {
	return Config{
		StateRoot: stateRoot, ProxyImageRepository: testProxyRepository, ProxyImageDigest: testProxyDigest,
		ProxyIdentity: world.AgentIdentity{UID: 65532, GID: 65532}, AuditQueueCapacity: 2,
	}
}

type fakeEffectBroker struct {
	dir          string
	effects      chan world.EffectAttempt
	done         chan struct{}
	shutdownGate <-chan struct{}
	readyErr     error
	shutdownErr  error
	closeOnce    sync.Once
}

func newFakeEffectBroker(stateDir, _ string, _ int) (effectBroker, error) {
	dir := filepath.Join(stateDir, "audit")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	return &fakeEffectBroker{dir: dir, effects: make(chan world.EffectAttempt), done: make(chan struct{})}, nil
}

func (f *fakeEffectBroker) SocketDir() string                   { return f.dir }
func (f *fakeEffectBroker) Ready(context.Context) error         { return f.readyErr }
func (f *fakeEffectBroker) Effects() <-chan world.EffectAttempt { return f.effects }
func (f *fakeEffectBroker) Done() <-chan struct{}               { return f.done }
func (f *fakeEffectBroker) Err() error                          { return nil }
func (f *fakeEffectBroker) Shutdown(ctx context.Context) error {
	if f.shutdownGate != nil {
		select {
		case <-f.shutdownGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.closeOnce.Do(func() {
		close(f.effects)
		close(f.done)
	})
	return f.shutdownErr
}

func testSpec(lower, digest string) world.SpawnSpec {
	return specWithScope(lower, []string{lower}, digest)
}

func specWithScope(workspace string, scope []string, digest string) world.SpawnSpec {
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "profile", Workspace: workspace, FSScope: scope, Egress: []string{"api.example.com"},
		Budget:   gen.Budget{Tokens: 10, TimeMs: 1000, MaxDepth: 2},
		Approval: policy.ApprovalManual,
	})
	return world.NewSpawnSpec(
		effective, world.NewImageReference(testAgentRepository, digest), []string{"agent", "--serve"}, 0,
		strings.Repeat("1", 32), strings.Repeat("2", 16),
		world.AgentIdentity{UID: 1000, GID: 1001}, nil,
	)
}

// openTestLease exercises the production durable-start gate. It is a test
// helper, not a compatibility Open API: Prepare, writer ACK, and Activate stay
// separate and failures preserve their real stage.
func openTestLease(t *testing.T, backend *Backend, ctx context.Context, spec world.SpawnSpec) (world.ActiveLease, error) {
	t.Helper()
	preparedValue, err := backend.Prepare(ctx, spec)
	if err != nil {
		return nil, err
	}
	prepared := preparedValue.(*preparedLease)
	receipt, writer, err := commitPreparedForTest(spec, prepared, &localMemoryStore{})
	if err != nil {
		_ = prepared.Abort(context.Background())
		return nil, err
	}
	defer writer.Close()
	active, err := prepared.Activate(ctx, receipt)
	if err != nil {
		return nil, err
	}
	return active, nil
}

func commitPreparedForTest(spec world.SpawnSpec, prepared *preparedLease, store *localMemoryStore) (world.SpawnReceipt, *logd.Writer, error) {
	writer, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		return world.SpawnReceipt{}, nil, err
	}
	metadata := prepared.Metadata()
	profileID, digest := metadata.ProfileID, metadata.ImageDigest
	budget := spec.Policy().Budget()
	payload, err := json.Marshal(gen.SubagentSpawnPayload{
		Adapter: "world", Instruction: "test", Depth: spec.Depth(),
		Budget:       gen.SpawnBudget{Tokens: budget.Tokens, TimeMs: budget.TimeMs, MaxDepth: budget.MaxDepth},
		WorldBackend: metadata.Backend, ProfileID: &profileID, ImageDigest: &digest, Mounts: metadata.Mounts,
	})
	if err != nil {
		_ = writer.Close()
		return world.SpawnReceipt{}, writer, err
	}
	parent := strings.Repeat("3", 16)
	receipt, err := world.CommitSpawn(context.Background(), writer, prepared, world.SpawnRecord(gen.EventRecord{
		TraceID: spec.TraceID(), SpanID: spec.SpanID(), ParentSpanID: &parent, Ts: 1,
		Kind: gen.KindSubagentSpawn, Actor: "parent", Payload: payload,
	}))
	if err != nil {
		return world.SpawnReceipt{}, writer, err
	}
	return receipt, writer, nil
}

type localMemoryStore struct {
	mu         sync.Mutex
	recs       []gen.EventRecord
	appendErr  error
	appendHook func()
}

func (s *localMemoryStore) LastSeq(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.recs)), nil
}
func (s *localMemoryStore) ReadFrom(context.Context, int64) ([]gen.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gen.EventRecord(nil), s.recs...), nil
}
func (s *localMemoryStore) Append(_ context.Context, rec gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return s.appendErr
	}
	if s.appendHook != nil {
		s.appendHook()
	}
	s.recs = append(s.recs, rec)
	return nil
}
func (s *localMemoryStore) AppendBatch(_ context.Context, recs []gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, recs...)
	return nil
}
func (s *localMemoryStore) Close() error { return nil }

func callKeys(calls [][]string) []string {
	out := make([]string, len(calls))
	for i := range calls {
		out[i] = commandKey(calls[i])
	}
	return out
}

func containsCall(calls [][]string, key string) bool {
	for _, call := range calls {
		if commandKey(call) == key {
			return true
		}
	}
	return false
}

func containsAnyCreate(calls [][]string) bool {
	return containsCall(calls, "proxy create") || containsCall(calls, "agent create")
}

func countCall(keys []string, want string) int {
	count := 0
	for _, key := range keys {
		if key == want {
			count++
		}
	}
	return count
}

func ordered(keys []string, want ...string) bool {
	pos := -1
	for _, target := range want {
		found := false
		for i := pos + 1; i < len(keys); i++ {
			if keys[i] == target {
				pos, found = i, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func waitForCall(t *testing.T, runner *fakePodman, key string) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		if containsCall(runner.snapshot(), key) {
			return
		}
		select {
		case <-runner.notify:
		case <-timer.C:
			t.Fatalf("Podman %s 호출 대기 timeout; calls=%v", key, callKeys(runner.snapshot()))
		}
	}
}

func receiveError(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("%s 대기 timeout", what)
		return nil
	}
}

func assertNoSpawnState(t *testing.T, stateRoot string) {
	t.Helper()
	root := filepath.Join(stateRoot, "world", strings.Repeat("1", 32), strings.Repeat("2", 16))
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("실패한 spawn state가 남음: %v", err)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}
