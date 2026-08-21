package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
)

const fakeContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakePodman struct {
	mu        sync.Mutex
	calls     [][]string
	notify    chan struct{}
	failures  map[string]error
	hook      func([]string) ([]byte, error, bool)
	digest    string
	imageUser string
}

func newFakePodman(digest string) *fakePodman {
	return &fakePodman{notify: make(chan struct{}, 32), failures: map[string]error{}, digest: digest, imageUser: "1000:1001"}
}

func (f *fakePodman) Run(_ context.Context, args ...string) ([]byte, error) {
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
		return []byte(`[{"Digest":"` + f.digest + `","Config":{"User":"` + f.imageUser + `"}}]`), nil
	case "create":
		return []byte(fakeContainerID + "\n"), nil
	default:
		return nil, nil
	}
}

func commandKey(args []string) string {
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		return "image inspect"
	}
	if len(args) == 0 {
		return ""
	}
	return args[0]
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
	leaseValue, err := backend.Open(context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	got := leaseValue.(*lease)

	calls := runner.snapshot()
	if gotKeys := callKeys(calls); !reflect.DeepEqual(gotKeys, []string{"info", "image inspect", "create", "start"}) {
		t.Fatalf("Podman 호출 순서 = %v", gotKeys)
	}
	create := strings.Join(calls[2], " ")
	for _, required := range []string{
		"--pull=never", "--network=none",
		"--userns=keep-id:uid=1000,gid=1001", "--user 1000:1001",
		"--workdir /workspace", ":/workspace:O,upperdir=", ",workdir=", digest,
	} {
		if !strings.Contains(create, required) {
			t.Errorf("create args에 %q 없음: %s", required, create)
		}
	}
	if strings.Contains(create, ":U") {
		t.Fatalf("lower를 chown하는 :U가 포함됨: %s", create)
	}

	metadata := got.Metadata()
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
	if got.AdapterEndpoint() != (world.Endpoint{}) {
		t.Fatal("T10-5 이전에 broker endpoint가 노출됨")
	}
	select {
	case _, ok := <-got.Effects():
		if ok {
			t.Fatal("T10-4 이전 effect stream에 값이 있음")
		}
	case <-time.After(time.Second):
		t.Fatal("빈 effect stream 종료 대기 timeout")
	}
	for _, dir := range []string{got.stateDir, got.upperDir, got.workDir} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("state dir %q mode/존재 이상: mode=%v err=%v", dir, infoMode(info), err)
		}
	}
}

func TestLeaseCloseWaitsForEffectsThenCleansContainerAndPreservesUpper(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	leaseValue, err := backend.Open(context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	got := leaseValue.(*lease)
	effectsDone := make(chan struct{})
	got.effectsDone = effectsDone

	closeResult := make(chan error, 1)
	go func() { closeResult <- got.Close(context.Background()) }()
	waitForCall(t, runner, "wait")
	if containsCall(runner.snapshot(), "rm") {
		t.Fatal("effect drain/ACK 전에 container cleanup이 시작됨")
	}
	close(effectsDone)
	if err := receiveError(t, closeResult, "Lease.Close"); err != nil {
		t.Fatal(err)
	}
	keys := callKeys(runner.snapshot())
	if !ordered(keys, "stop", "wait", "rm") {
		t.Fatalf("Close Podman 순서 이상: %v", keys)
	}
	if _, err := os.Stat(got.upperDir); err != nil {
		t.Fatalf("collector ACK 전 upper가 삭제됨: %v", err)
	}
	if _, err := os.Stat(got.workDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("container cleanup 뒤 workdir가 남음: %v", err)
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
	runner.failures["stop"] = stopErr
	backend := mustBackend(t, stateRoot, runner, statDevice)
	leaseValue, err := backend.Open(context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	err = leaseValue.Close(context.Background())
	if !errors.Is(err, stopErr) {
		t.Fatalf("stop 오류 체인 소실: %v", err)
	}
	if !ordered(callKeys(runner.snapshot()), "stop", "wait", "rm") {
		t.Fatalf("stop 오류가 cleanup을 단락함: %v", callKeys(runner.snapshot()))
	}
}

func TestConcurrentCloseHonorsContextWhileLifecycleIsGated(t *testing.T) {
	digest := "sha256:" + strings.Repeat("8", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	leaseValue, err := backend.Open(context.Background(), testSpec(lower, digest))
	if err != nil {
		t.Fatal(err)
	}
	got := leaseValue.(*lease)
	effectsDone := make(chan struct{})
	got.effectsDone = effectsDone
	first := make(chan error, 1)
	go func() { first <- got.Close(context.Background()) }()
	waitForCall(t, runner, "wait")

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

func TestOpenFailsClosedBeforeContainerStart(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	t.Run("tag", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := backend.Open(context.Background(), testSpec(lower, "alpine:latest"))
		if err == nil || containsCall(runner.snapshot(), "create") {
			t.Fatalf("tag가 container create까지 도달함: err=%v calls=%v", err, callKeys(runner.snapshot()))
		}
	})

	t.Run("inspect digest mismatch", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman("sha256:" + strings.Repeat("e", 64))
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := backend.Open(context.Background(), testSpec(lower, digest))
		if err == nil || containsCall(runner.snapshot(), "create") {
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
			_, err := backend.Open(context.Background(), testSpec(lower, digest))
			if err == nil || containsCall(runner.snapshot(), "create") {
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
		_, err := backend.Open(context.Background(), spec)
		if err == nil || containsCall(runner.snapshot(), "create") {
			t.Fatalf("symlink 탈출이 create까지 도달함: err=%v", err)
		}
	})

	t.Run("state root below lower", func(t *testing.T) {
		lower := t.TempDir()
		stateRoot := filepath.Join(lower, "state")
		mustMkdir(t, stateRoot, 0o700)
		runner := newFakePodman(digest)
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := backend.Open(context.Background(), testSpec(lower, digest))
		if err == nil || containsCall(runner.snapshot(), "create") {
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
		_, err := backend.Open(context.Background(), testSpec(lower, digest))
		if err == nil || containsCall(runner.snapshot(), "create") {
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
		_, err := backend.Open(context.Background(), testSpec(lower, digest))
		if err == nil || containsCall(runner.snapshot(), "create") {
			t.Fatalf("unsafe path가 create까지 도달함: err=%v", err)
		}
	})
}

func TestBackendAndPodmanPreconditionsFailClosed(t *testing.T) {
	digest := "sha256:" + strings.Repeat("9", 64)
	t.Run("state root mode", func(t *testing.T) {
		stateRoot := filepath.Join(t.TempDir(), "state")
		mustMkdir(t, stateRoot, 0o755)
		if _, err := newBackend(stateRoot, newFakePodman(digest), statDevice); err == nil {
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
			_, err := backend.Open(context.Background(), testSpec(lower, digest))
			if err == nil || containsCall(runner.snapshot(), "create") {
				t.Fatalf("부적합 preflight가 create까지 도달함: err=%v calls=%v", err, callKeys(runner.snapshot()))
			}
		})
	}
}

func TestCreateAndStartFailuresCleanResources(t *testing.T) {
	digest := "sha256:" + strings.Repeat("f", 64)
	t.Run("create error with cidfile", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		runner.hook = func(args []string) ([]byte, error, bool) {
			if commandKey(args) != "create" {
				return nil, nil, false
			}
			for i := range args {
				if args[i] == "--cidfile" && i+1 < len(args) {
					if err := os.WriteFile(args[i+1], []byte(fakeContainerID), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			return nil, errors.New("create failed after allocation"), true
		}
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := backend.Open(context.Background(), testSpec(lower, digest))
		if err == nil || !containsCall(runner.snapshot(), "rm") {
			t.Fatalf("create 오류 orphan cleanup 누락: err=%v calls=%v", err, callKeys(runner.snapshot()))
		}
		assertNoSpawnState(t, stateRoot)
	})

	t.Run("start error", func(t *testing.T) {
		lower, stateRoot := testDirs(t)
		runner := newFakePodman(digest)
		runner.failures["start"] = errors.New("start failed")
		backend := mustBackend(t, stateRoot, runner, statDevice)
		_, err := backend.Open(context.Background(), testSpec(lower, digest))
		if err == nil || !ordered(callKeys(runner.snapshot()), "create", "start", "rm") {
			t.Fatalf("start 오류 cleanup 누락: err=%v calls=%v", err, callKeys(runner.snapshot()))
		}
		assertNoSpawnState(t, stateRoot)
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
	backend, err := newBackend(stateRoot, runner, device)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func testSpec(lower, digest string) world.SpawnSpec {
	return specWithScope(lower, []string{lower}, digest)
}

func specWithScope(workspace string, scope []string, digest string) world.SpawnSpec {
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "profile", Workspace: workspace, FSScope: scope,
		Budget:   gen.Budget{Tokens: 10, TimeMs: 1000, MaxDepth: 2},
		Approval: policy.ApprovalManual,
	})
	return world.NewSpawnSpec(
		effective, digest, []string{"agent", "--serve"}, 0,
		strings.Repeat("1", 32), strings.Repeat("2", 16),
		world.AgentIdentity{UID: 1000, GID: 1001}, nil,
	)
}

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
