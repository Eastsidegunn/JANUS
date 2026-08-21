// Package local implements the Linux rootless-Podman execution world
// (FR-SBX-01, FR-SBX-02). This stage intentionally uses --network=none;
// the audited proxy network is added in T10 step 4.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
)

const podmanBinary = "podman"

var (
	digestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	tracePattern       = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanPattern        = regexp.MustCompile(`^[0-9a-f]{16}$`)
	containerIDPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
)

type commandRunner interface {
	Run(context.Context, ...string) ([]byte, error)
}

type execPodman struct{}

func (execPodman) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, podmanBinary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("podman %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Backend owns the rootless Podman runtime and a host state root. The state
// root is resolved once and must already have mode 0700.
type Backend struct {
	stateRoot string
	runner    commandRunner
	deviceID  func(string) (uint64, error)
}

var _ world.Backend = (*Backend)(nil)

// NewBackend creates the production Linux backend. macOS is intentionally not
// supported: Podman there runs inside a VM and is not the deployment kernel.
func NewBackend(stateRoot string) (*Backend, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("world/local: Linux rootless Podman 전용 backend (현재 %s)", runtime.GOOS)
	}
	return newBackend(stateRoot, execPodman{}, statDevice)
}

func newBackend(stateRoot string, runner commandRunner, deviceID func(string) (uint64, error)) (*Backend, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) {
		return nil, fmt.Errorf("world/local: state root는 절대 경로여야 함: %q", stateRoot)
	}
	resolved, err := filepath.EvalSymlinks(stateRoot)
	if err != nil {
		return nil, fmt.Errorf("world/local: state root 실경로: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("world/local: state root가 디렉터리가 아님: %q", resolved)
	}
	if info.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("world/local: state root mode는 0700이어야 함: %04o", info.Mode().Perm())
	}
	return &Backend{stateRoot: resolved, runner: runner, deviceID: deviceID}, nil
}

type podmanInfo struct {
	Host struct {
		Security struct {
			Rootless bool `json:"rootless"`
		} `json:"security"`
	} `json:"host"`
	Store struct {
		GraphDriverName string            `json:"graphDriverName"`
		GraphStatus     map[string]string `json:"graphStatus"`
	} `json:"store"`
}

type imageInspection struct {
	Digest string `json:"Digest"`
	Config struct {
		User string `json:"User"`
	} `json:"Config"`
}

// Open resolves and re-authorizes the workspace before creating any Podman
// object. The image is accepted and executed only by immutable digest.
func (b *Backend) Open(ctx context.Context, spec world.SpawnSpec) (opened world.Lease, err error) {
	if err := b.preflight(ctx); err != nil {
		return nil, err
	}
	if !digestPattern.MatchString(spec.ImageDigest()) {
		return nil, fmt.Errorf("world/local: image는 sha256 digest여야 함(tag 금지): %q", spec.ImageDigest())
	}
	if !tracePattern.MatchString(spec.TraceID()) || !spanPattern.MatchString(spec.SpanID()) {
		return nil, fmt.Errorf("world/local: trace/span ID 형식 오류")
	}
	if len(spec.AgentArgv()) == 0 || spec.AgentArgv()[0] == "" {
		return nil, fmt.Errorf("world/local: agent argv가 비어 있음")
	}

	inspected, err := b.inspectImage(ctx, spec.ImageDigest(), spec.AgentIdentity())
	if err != nil {
		return nil, err
	}
	layout, err := b.prepareOverlay(spec)
	if err != nil {
		return nil, err
	}
	keepState := false
	defer func() {
		if !keepState {
			err = errors.Join(err, os.RemoveAll(layout.stateDir))
		}
	}()

	cidFile := filepath.Join(layout.stateDir, "container.cid")
	identity := spec.AgentIdentity()
	uid := strconv.FormatUint(uint64(identity.UID), 10)
	gid := strconv.FormatUint(uint64(identity.GID), 10)
	volume := layout.lower + ":/workspace:O,upperdir=" + layout.upper + ",workdir=" + layout.work
	args := []string{
		"create", "--cidfile", cidFile,
		"--pull=never", "--network=none",
		"--userns=keep-id:uid=" + uid + ",gid=" + gid,
		"--user", uid + ":" + gid,
		"--workdir", "/workspace",
		"--volume", volume,
		inspected,
	}
	args = append(args, spec.AgentArgv()...)
	out, createErr := b.runner.Run(ctx, args...)
	containerID := firstContainerID(out)
	if containerID == "" {
		if cidBytes, readErr := os.ReadFile(cidFile); readErr == nil {
			containerID = firstContainerID(cidBytes)
		}
	}
	if createErr != nil {
		var cleanupErr error
		if containerID != "" {
			cleanupCtx, cancel := cleanupContext(ctx)
			_, cleanupErr = b.runner.Run(cleanupCtx, "rm", "--force", containerID)
			cancel()
		}
		return nil, errors.Join(fmt.Errorf("world/local: container create: %w", createErr), cleanupErr)
	}
	if containerID == "" {
		return nil, fmt.Errorf("world/local: container create가 유효한 ID를 반환하지 않음")
	}
	if _, err := b.runner.Run(ctx, "start", containerID); err != nil {
		cleanupCtx, cancel := cleanupContext(ctx)
		_, cleanupErr := b.runner.Run(cleanupCtx, "rm", "--force", containerID)
		cancel()
		return nil, errors.Join(fmt.Errorf("world/local: container start: %w", err), cleanupErr)
	}

	effectsDone := make(chan struct{})
	close(effectsDone) // T10-4 replaces this with the proxy audit drain/ACK gate.
	effects := make(chan world.EffectAttempt)
	close(effects)
	keepState = true
	return &lease{
		runner: b.runner, containerID: containerID,
		stateDir: layout.stateDir, upperDir: layout.upper, workDir: layout.work,
		cidFile: cidFile, effects: effects, effectsDone: effectsDone,
		closeToken: makeCloseToken(),
		metadata: world.SpawnMetadata{
			Backend:   gen.SubagentSpawnPayloadWorldBackendLocalPodman,
			ProfileID: spec.Policy().ProfileID(), ImageDigest: inspected,
			Mounts: []gen.SubagentSpawnMount{{
				SourcePath: layout.lower,
				TargetPath: gen.SubagentSpawnMountTargetPathWorkspace,
				Mode:       gen.SubagentSpawnMountModeOverlay,
				UpperRef:   filepath.ToSlash(layout.upperRef),
			}},
		},
	}, nil
}

func (b *Backend) preflight(ctx context.Context) error {
	out, err := b.runner.Run(ctx, "info", "--format", "json")
	if err != nil {
		return fmt.Errorf("world/local: podman info: %w", err)
	}
	var info podmanInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return fmt.Errorf("world/local: podman info JSON: %w", err)
	}
	if !info.Host.Security.Rootless {
		return fmt.Errorf("world/local: rootless Podman이 아님")
	}
	if info.Store.GraphDriverName != "overlay" || info.Store.GraphStatus["Native Overlay Diff"] != "true" {
		return fmt.Errorf("world/local: native overlayfs가 아님 (driver=%q native=%q)",
			info.Store.GraphDriverName, info.Store.GraphStatus["Native Overlay Diff"])
	}
	return nil
}

func (b *Backend) inspectImage(ctx context.Context, requested string, identity world.AgentIdentity) (string, error) {
	out, err := b.runner.Run(ctx, "image", "inspect", requested)
	if err != nil {
		return "", fmt.Errorf("world/local: image inspect: %w", err)
	}
	var images []imageInspection
	if err := json.Unmarshal(out, &images); err != nil {
		return "", fmt.Errorf("world/local: image inspect JSON 형식 오류: %w", err)
	}
	if len(images) != 1 {
		return "", fmt.Errorf("world/local: image inspect 결과가 1개가 아님: %d", len(images))
	}
	got := images[0].Digest
	if !digestPattern.MatchString(got) || got != requested {
		return "", fmt.Errorf("world/local: image digest 불일치 (요청=%q inspect=%q)", requested, got)
	}
	declared, err := parseImageIdentity(images[0].Config.User)
	if err != nil {
		return "", err
	}
	if declared != identity {
		return "", fmt.Errorf("world/local: image UID/GID와 SpawnSpec 불일치 (image=%d:%d spec=%d:%d)",
			declared.UID, declared.GID, identity.UID, identity.GID)
	}
	return got, nil
}

func parseImageIdentity(value string) (world.AgentIdentity, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return world.AgentIdentity{}, fmt.Errorf("world/local: image Config.User는 숫자 uid:gid여야 함: %q", value)
	}
	uid, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return world.AgentIdentity{}, fmt.Errorf("world/local: image UID가 숫자가 아님: %q", value)
	}
	gid, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return world.AgentIdentity{}, fmt.Errorf("world/local: image GID가 숫자가 아님: %q", value)
	}
	return world.AgentIdentity{UID: uint32(uid), GID: uint32(gid)}, nil
}

type overlayLayout struct {
	lower, stateDir, upper, work, upperRef string
}

func (b *Backend) prepareOverlay(spec world.SpawnSpec) (layout overlayLayout, err error) {
	lower, err := filepath.EvalSymlinks(spec.Policy().Workspace())
	if err != nil {
		return layout, fmt.Errorf("world/local: workspace 실경로: %w", err)
	}
	lower, err = filepath.Abs(lower)
	if err != nil {
		return layout, fmt.Errorf("world/local: workspace 절대경로: %w", err)
	}
	if !allowsResolvedWorkspace(spec.Policy(), lower) {
		return layout, fmt.Errorf("world/local: workspace 실경로 %q가 정책 scope 밖", lower)
	}
	info, err := os.Stat(lower)
	if err != nil || !info.IsDir() {
		return layout, fmt.Errorf("world/local: workspace가 디렉터리가 아님: %q", lower)
	}

	stateDir := filepath.Join(b.stateRoot, "world", spec.TraceID(), spec.SpanID())
	if pathWithin(lower, stateDir) {
		return layout, fmt.Errorf("world/local: state root가 lower 내부임: %q", stateDir)
	}
	upperRef := filepath.Join("world", spec.TraceID(), spec.SpanID(), "overlay", "upper")
	upper := filepath.Join(stateDir, "overlay", "upper")
	work := filepath.Join(stateDir, "overlay", "work")
	for _, p := range []string{lower, stateDir, upper, work} {
		if unsafeVolumePath(p) {
			return layout, fmt.Errorf("world/local: Podman volume grammar에 안전하지 않은 경로: %q", p)
		}
	}
	if err := os.MkdirAll(filepath.Dir(stateDir), 0o700); err != nil {
		return layout, fmt.Errorf("world/local: state parent 생성: %w", err)
	}
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		return layout, fmt.Errorf("world/local: spawn state가 이미 존재하거나 생성 불가: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			err = errors.Join(err, os.RemoveAll(stateDir))
		}
	}()
	for _, p := range []string{filepath.Join(stateDir, "overlay"), upper, work} {
		if err := os.Mkdir(p, 0o700); err != nil {
			return layout, fmt.Errorf("world/local: overlay 디렉터리 생성 %q: %w", p, err)
		}
		if err := os.Chmod(p, 0o700); err != nil {
			return layout, fmt.Errorf("world/local: overlay 디렉터리 mode %q: %w", p, err)
		}
	}
	upperDevice, err := b.deviceID(upper)
	if err != nil {
		return layout, fmt.Errorf("world/local: upper filesystem: %w", err)
	}
	workDevice, err := b.deviceID(work)
	if err != nil {
		return layout, fmt.Errorf("world/local: work filesystem: %w", err)
	}
	if upperDevice != workDevice {
		return layout, fmt.Errorf("world/local: upper/work가 서로 다른 filesystem (%d != %d)", upperDevice, workDevice)
	}
	cleanup = false
	return overlayLayout{lower: lower, stateDir: stateDir, upper: upper, work: work, upperRef: upperRef}, nil
}

func allowsResolvedWorkspace(effective world.EffectivePolicy, workspace string) bool {
	var resolvedScope []string
	for _, scope := range effective.FSScope() {
		resolved, err := filepath.EvalSymlinks(scope)
		if err != nil {
			continue
		}
		absolute, err := filepath.Abs(resolved)
		if err != nil {
			continue
		}
		resolvedScope = append(resolvedScope, filepath.ToSlash(absolute))
	}
	return policy.AllowsWorkspace(resolvedScope, filepath.ToSlash(workspace))
}

func statDevice(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat 정보 형식 %T", info.Sys())
	}
	return uint64(stat.Dev), nil
}

func pathWithin(base, candidate string) bool {
	rel, err := filepath.Rel(base, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func unsafeVolumePath(path string) bool {
	return strings.ContainsAny(path, ":,\x00\n\r")
}

func firstContainerID(out []byte) string {
	fields := strings.Fields(string(out))
	if len(fields) == 0 || !containerIDPattern.MatchString(fields[0]) {
		return ""
	}
	return fields[0]
}

type lease struct {
	runner      commandRunner
	containerID string
	stateDir    string
	upperDir    string
	workDir     string
	cidFile     string
	metadata    world.SpawnMetadata
	effects     <-chan world.EffectAttempt
	effectsDone <-chan struct{}

	closeToken chan struct{}
	closed     bool
	closeErr   error
}

var _ world.Lease = (*lease)(nil)

// AdapterEndpoint remains empty until the backend-neutral broker is assembled
// in T10 step 5. No surface consumes this lease in the current stage.
func (l *lease) AdapterEndpoint() world.Endpoint { return world.Endpoint{} }
func (l *lease) Metadata() world.SpawnMetadata   { return l.metadata.Clone() }

// UpperDir may contain subordinate-UID-owned character-device whiteouts.
// T11 must classify them with lstat(mode+rdev), never with an owner filter.
func (l *lease) UpperDir() string                    { return l.upperDir }
func (l *lease) Effects() <-chan world.EffectAttempt { return l.effects }

func (l *lease) Close(ctx context.Context) error {
	select {
	case <-l.closeToken:
		defer func() { l.closeToken <- struct{}{} }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if l.closed {
		return l.closeErr
	}

	var joined error
	if _, err := l.runner.Run(ctx, "stop", "--time", "10", l.containerID); err != nil {
		joined = errors.Join(joined, fmt.Errorf("world/local: container stop: %w", err))
	}
	if _, err := l.runner.Run(ctx, "wait", l.containerID); err != nil {
		joined = errors.Join(joined, fmt.Errorf("world/local: container wait: %w", err))
	}
	select {
	case <-l.effectsDone:
	case <-ctx.Done():
		return errors.Join(joined, ctx.Err())
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	_, cleanupErr := l.runner.Run(cleanupCtx, "rm", "--force", l.containerID)
	cancel()
	if cleanupErr != nil {
		joined = errors.Join(joined, fmt.Errorf("world/local: container cleanup: %w", cleanupErr))
		return joined
	}
	// upper is intentionally preserved for T11. Agent termination is not a
	// collector ACK and therefore must not erase filesystem evidence.
	if err := os.RemoveAll(l.workDir); err != nil {
		joined = errors.Join(joined, fmt.Errorf("world/local: workdir cleanup: %w", err))
	}
	if err := os.Remove(l.cidFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		joined = errors.Join(joined, fmt.Errorf("world/local: cidfile cleanup: %w", err))
	}
	l.closed, l.closeErr = true, joined
	return joined
}

func makeCloseToken() chan struct{} {
	token := make(chan struct{}, 1)
	token <- struct{}{}
	return token
}

func cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
}
