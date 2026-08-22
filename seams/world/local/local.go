// Package local implements the Linux rootless-Podman execution world
// (FR-SBX-01, FR-SBX-02, FR-SBX-03).
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
	"sync"
	"syscall"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/world/local/egressproxy"
)

const podmanBinary = "podman"

const (
	proxyListenPort      = "3128"
	proxySocketMount     = "/run/hx-audit"
	proxySocketPath      = proxySocketMount + "/" + auditSocketName
	proxyExecutable      = "/hxegressproxy"
	defaultAuditCapacity = 64
	proxyReadyTimeout    = 30 * time.Second
)

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

// Config fixes the trusted proxy helper image independently of the untrusted
// agent image. Both must be immutable digests with numeric non-root identities.
type Config struct {
	StateRoot            string
	ProxyImageRepository string
	ProxyImageDigest     string
	ProxyIdentity        world.AgentIdentity
	AuditQueueCapacity   int
	ApprovalCapacity     int
}

// Backend owns the rootless Podman runtime, per-spawn networks, the trusted
// proxy sidecar, its host-only audit broker, and a host state root.
type Backend struct {
	stateRoot        string
	proxyRepository  string
	proxyDigest      string
	proxyIdentity    world.AgentIdentity
	auditCapacity    int
	approvalCapacity int
	runner           commandRunner
	deviceID         func(string) (uint64, error)
	newEffectBroker  auditBrokerFactory
}

var _ world.Backend = (*Backend)(nil)

// NewBackend creates the production Linux backend. macOS is intentionally not
// supported: Podman there runs inside a VM and is not the deployment kernel.
func NewBackend(config Config) (*Backend, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("world/local: Linux rootless Podman 전용 backend (현재 %s)", runtime.GOOS)
	}
	return newBackend(config, execPodman{}, statDevice, startAuditBroker)
}

func newBackend(config Config, runner commandRunner, deviceID func(string) (uint64, error), brokerFactory auditBrokerFactory) (*Backend, error) {
	stateRoot := config.StateRoot
	if stateRoot == "" || !filepath.IsAbs(stateRoot) {
		return nil, fmt.Errorf("world/local: state root는 절대 경로여야 함: %q", stateRoot)
	}
	if !digestPattern.MatchString(config.ProxyImageDigest) {
		return nil, fmt.Errorf("world/local: proxy image는 sha256 digest여야 함(tag 금지): %q", config.ProxyImageDigest)
	}
	if err := validateRepository(config.ProxyImageRepository); err != nil {
		return nil, fmt.Errorf("world/local: proxy repository: %w", err)
	}
	if config.ProxyIdentity.UID == 0 || config.ProxyIdentity.GID == 0 {
		return nil, fmt.Errorf("world/local: proxy image는 숫자 non-root UID/GID여야 함")
	}
	if config.AuditQueueCapacity == 0 {
		config.AuditQueueCapacity = defaultAuditCapacity
	}
	if config.AuditQueueCapacity < 0 || brokerFactory == nil {
		return nil, fmt.Errorf("world/local: audit broker 설정 위반")
	}
	if config.ApprovalCapacity == 0 {
		config.ApprovalCapacity = defaultApprovalCapacity
	}
	if config.ApprovalCapacity < 0 {
		return nil, fmt.Errorf("world/local: approval broker 설정 위반")
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
	return &Backend{
		stateRoot: resolved, proxyRepository: config.ProxyImageRepository,
		proxyDigest: config.ProxyImageDigest, proxyIdentity: config.ProxyIdentity,
		auditCapacity: config.AuditQueueCapacity, approvalCapacity: config.ApprovalCapacity,
		runner: runner, deviceID: deviceID,
		newEffectBroker: brokerFactory,
	}, nil
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

// preparedLease contains only preflight and overlay state. Prepare never
// creates a Podman network/container or starts a broker.
type preparedLease struct {
	backend   *Backend
	id        world.PreparedID
	spec      world.SpawnSpec
	agentRef  world.ImageReference
	proxyRef  world.ImageReference
	allowlist []string
	layout    overlayLayout
	metadata  world.SpawnMetadata

	mu        sync.Mutex
	activated bool
	aborted   bool
}

var _ world.PreparedLease = (*preparedLease)(nil)

// Prepare resolves and re-authorizes the workspace and images before creating
// any Podman runtime object. Runtime execution always uses repository@digest;
// durable metadata retains only the inspected digest.
func (b *Backend) Prepare(ctx context.Context, spec world.SpawnSpec) (prepared world.PreparedLease, err error) {
	if err := b.preflight(ctx); err != nil {
		return nil, err
	}
	if err := validateImageReference(spec.Image()); err != nil {
		return nil, fmt.Errorf("world/local: agent image: %w", err)
	}
	if !tracePattern.MatchString(spec.TraceID()) || !spanPattern.MatchString(spec.SpanID()) {
		return nil, fmt.Errorf("world/local: trace/span ID 형식 오류")
	}
	if len(spec.AgentArgv()) == 0 || spec.AgentArgv()[0] == "" {
		return nil, fmt.Errorf("world/local: agent argv가 비어 있음")
	}
	allowlist, err := egressproxy.NormalizeAllowlist(spec.Policy().Egress())
	if err != nil {
		return nil, fmt.Errorf("world/local: egress policy: %w", err)
	}

	agentRef, err := b.inspectImage(ctx, spec.Image(), spec.AgentIdentity())
	if err != nil {
		return nil, err
	}
	proxyRef, err := b.inspectImage(ctx, world.NewImageReference(b.proxyRepository, b.proxyDigest), b.proxyIdentity)
	if err != nil {
		return nil, fmt.Errorf("world/local: proxy image: %w", err)
	}
	layout, err := b.prepareOverlay(spec)
	if err != nil {
		return nil, err
	}
	id, err := world.NewPreparedID(spec.SpanID())
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(layout.stateDir))
	}
	metadata := world.SpawnMetadata{
		Backend:   gen.SubagentSpawnPayloadWorldBackendLocalPodman,
		ProfileID: spec.Policy().ProfileID(), ImageDigest: agentRef.Digest(),
		Mounts: []gen.SubagentSpawnMount{{
			SourcePath: layout.lower, TargetPath: gen.SubagentSpawnMountTargetPathWorkspace,
			Mode: gen.SubagentSpawnMountModeOverlay, UpperRef: filepath.ToSlash(layout.upperRef),
		}},
	}
	return &preparedLease{
		backend: b, id: id, spec: spec, agentRef: agentRef, proxyRef: proxyRef,
		allowlist: append([]string(nil), allowlist...), layout: layout, metadata: metadata,
	}, nil
}

func (p *preparedLease) ID() world.PreparedID          { return p.id }
func (p *preparedLease) Metadata() world.SpawnMetadata { return p.metadata.Clone() }
func (p *preparedLease) UpperDir() string              { return p.layout.upper }

func (p *preparedLease) Abort(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.activated {
		return fmt.Errorf("world/local: active lease는 Abort할 수 없음")
	}
	if p.aborted {
		return nil
	}
	p.aborted = true
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := os.RemoveAll(p.layout.stateDir); err != nil {
		return fmt.Errorf("world/local: prepared state cleanup: %w", err)
	}
	return cleanupCtx.Err()
}

func (p *preparedLease) Activate(ctx context.Context, receipt world.SpawnReceipt) (world.ActiveLease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.aborted || p.activated {
		return nil, fmt.Errorf("world/local: prepared lease가 이미 소비됨")
	}
	if err := world.ValidateSpawnReceipt(receipt, p.id, p.metadata); err != nil {
		return nil, err
	}
	// Receipt is consumed before the first runtime side effect. A partial
	// activation cannot be retried against an ambiguous resource set.
	p.activated = true
	return p.backend.activate(ctx, p)
}

func (b *Backend) activate(ctx context.Context, prepared *preparedLease) (opened world.ActiveLease, err error) {
	spec, layout := prepared.spec, prepared.layout
	broker, err := b.newEffectBroker(layout.stateDir, spec.SpanID(), b.auditCapacity)
	if err != nil {
		return nil, err
	}
	approval, err := startApprovalBroker(ctx, layout.stateDir, spec.SpanID(), spec.Policy().Budget().TimeMs, b.approvalCapacity)
	if err != nil {
		_ = broker.Shutdown(context.Background())
		return nil, err
	}
	resources := runtimeResources{runner: b.runner, broker: broker, approval: approval}
	keepRuntime := false
	defer func() {
		if !keepRuntime {
			cleanupCtx, cancel := cleanupContext(ctx)
			err = errors.Join(err, resources.cleanupOpen(cleanupCtx))
			cancel()
			err = errors.Join(err, os.RemoveAll(layout.stateDir))
		}
	}()

	internalNetwork := "hx-" + spec.SpanID() + "-internal"
	externalNetwork := "hx-" + spec.SpanID() + "-egress"
	proxyName := "hx-" + spec.SpanID() + "-proxy"
	if _, err := b.runner.Run(ctx, "network", "create", "--internal", internalNetwork); err != nil {
		return nil, fmt.Errorf("world/local: internal network create: %w", err)
	}
	resources.networks = append(resources.networks, internalNetwork)
	if _, err := b.runner.Run(ctx, "network", "create", externalNetwork); err != nil {
		return nil, fmt.Errorf("world/local: external network create: %w", err)
	}
	resources.networks = append(resources.networks, externalNetwork)

	proxyCIDFile := filepath.Join(layout.stateDir, "proxy.cid")
	proxyArgs := b.proxyCreateArgs(prepared.allowlist, prepared.proxyRef.String(), broker.SocketDir(), proxyCIDFile, proxyName, internalNetwork, externalNetwork)
	proxyID, err := b.createContainer(ctx, proxyArgs, proxyCIDFile)
	if err != nil {
		return nil, fmt.Errorf("world/local: proxy container create: %w", err)
	}
	resources.proxyID = proxyID
	if _, err := b.runner.Run(ctx, "start", proxyID); err != nil {
		return nil, fmt.Errorf("world/local: proxy container start: %w", err)
	}
	readyCtx, cancelReady := context.WithTimeout(ctx, proxyReadyTimeout)
	err = broker.Ready(readyCtx)
	cancelReady()
	if err != nil {
		return nil, fmt.Errorf("world/local: proxy ready: %w", err)
	}

	cidFile := filepath.Join(layout.stateDir, "container.cid")
	identity := spec.AgentIdentity()
	uid := strconv.FormatUint(uint64(identity.UID), 10)
	gid := strconv.FormatUint(uint64(identity.GID), 10)
	volume := layout.lower + ":/workspace:O,upperdir=" + layout.upper + ",workdir=" + layout.work
	args := []string{
		"create", "--cidfile", cidFile,
		"--pull=never", "--network", internalNetwork,
		"--userns=keep-id:uid=" + uid + ",gid=" + gid,
		"--user", uid + ":" + gid,
		"--workdir", "/workspace",
		"--env", "HTTP_PROXY=http://" + proxyName + ":" + proxyListenPort,
		"--env", "HTTPS_PROXY=http://" + proxyName + ":" + proxyListenPort,
		"--env", "NO_PROXY=",
		"--env", "http_proxy=http://" + proxyName + ":" + proxyListenPort,
		"--env", "https_proxy=http://" + proxyName + ":" + proxyListenPort,
		"--env", "no_proxy=",
		"--env", "HX_APPROVAL_SOCKET=" + approvalRelayPath,
		"--volume", approval.RelayDir() + ":" + approvalRelayMount + ":ro",
		"--volume", volume,
		prepared.agentRef.String(),
	}
	args = append(args, spec.AgentArgv()...)
	containerID, err := b.createContainer(ctx, args, cidFile)
	if err != nil {
		return nil, fmt.Errorf("world/local: agent container create: %w", err)
	}
	resources.agentID = containerID
	process, err := startProcessBroker(ctx, spec.SpanID(), prepared.id.String(), containerID, b.runner)
	if err != nil {
		return nil, err
	}
	resources.process = process
	keepRuntime = true
	return &lease{
		runner: b.runner, containerID: containerID, proxyID: proxyID,
		internalNetwork: internalNetwork, externalNetwork: externalNetwork,
		stateDir: layout.stateDir, upperDir: layout.upper, workDir: layout.work,
		cidFile: cidFile, proxyCIDFile: proxyCIDFile,
		broker: broker, effects: broker.Effects(), effectsDone: broker.Done(),
		approval: approval, process: process,
		processEndpoint: process.Endpoint(), approvalEndpoint: approval.Endpoint(),
		closeToken: makeCloseToken(),
		metadata:   prepared.metadata.Clone(),
	}, nil
}

func (b *Backend) createContainer(ctx context.Context, args []string, cidFile string) (string, error) {
	out, createErr := b.runner.Run(ctx, args...)
	containerID := firstContainerID(out)
	if containerID == "" {
		if cidBytes, readErr := os.ReadFile(cidFile); readErr == nil {
			containerID = firstContainerID(cidBytes)
		}
	}
	if createErr != nil {
		if containerID == "" {
			return "", createErr
		}
		cleanupCtx, cancel := cleanupContext(ctx)
		_, cleanupErr := b.runner.Run(cleanupCtx, "rm", "--force", containerID)
		cancel()
		return "", errors.Join(createErr, cleanupErr)
	}
	if containerID == "" {
		return "", fmt.Errorf("container create가 유효한 ID를 반환하지 않음")
	}
	return containerID, nil
}

func (b *Backend) proxyCreateArgs(
	allowlist []string,
	proxyDigest, socketDir, cidFile, proxyName, internalNetwork, externalNetwork string,
) []string {
	uid := strconv.FormatUint(uint64(b.proxyIdentity.UID), 10)
	gid := strconv.FormatUint(uint64(b.proxyIdentity.GID), 10)
	args := []string{
		"create", "--cidfile", cidFile, "--name", proxyName,
		"--pull=never", "--network", internalNetwork, "--network", externalNetwork,
		"--network-alias", proxyName,
		"--userns=keep-id:uid=" + uid + ",gid=" + gid, "--user", uid + ":" + gid,
		"--read-only", "--cap-drop=all", "--security-opt=no-new-privileges",
		"--entrypoint", proxyExecutable,
		"--volume", socketDir + ":" + proxySocketMount + ":ro",
		proxyDigest,
		"--listen", ":" + proxyListenPort, "--audit-socket", proxySocketPath,
	}
	for _, domain := range allowlist {
		args = append(args, "--allow", domain)
	}
	return args
}

type runtimeResources struct {
	runner   commandRunner
	broker   effectBroker
	approval *approvalBroker
	process  *processBroker
	agentID  string
	proxyID  string
	networks []string
}

func (r *runtimeResources) cleanupOpen(ctx context.Context) error {
	var joined error
	if r.process != nil {
		joined = errors.Join(joined, r.process.Shutdown(ctx))
	}
	if r.approval != nil {
		joined = errors.Join(joined, r.approval.Cleanup())
	}
	for _, containerID := range []string{r.agentID, r.proxyID} {
		if containerID == "" {
			continue
		}
		_, err := r.runner.Run(ctx, "rm", "--force", containerID)
		joined = errors.Join(joined, err)
	}
	if r.broker != nil {
		joined = errors.Join(joined, r.broker.Shutdown(ctx))
	}
	for i := len(r.networks) - 1; i >= 0; i-- {
		_, err := r.runner.Run(ctx, "network", "rm", "--force", r.networks[i])
		joined = errors.Join(joined, err)
	}
	return joined
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

func (b *Backend) inspectImage(ctx context.Context, requested world.ImageReference, identity world.AgentIdentity) (world.ImageReference, error) {
	if err := validateImageReference(requested); err != nil {
		return world.ImageReference{}, err
	}
	out, err := b.runner.Run(ctx, "image", "inspect", requested.String())
	if err != nil {
		return world.ImageReference{}, fmt.Errorf("world/local: image inspect: %w", err)
	}
	var images []imageInspection
	if err := json.Unmarshal(out, &images); err != nil {
		return world.ImageReference{}, fmt.Errorf("world/local: image inspect JSON 형식 오류: %w", err)
	}
	if len(images) != 1 {
		return world.ImageReference{}, fmt.Errorf("world/local: image inspect 결과가 1개가 아님: %d", len(images))
	}
	got := images[0].Digest
	if !digestPattern.MatchString(got) || got != requested.Digest() {
		return world.ImageReference{}, fmt.Errorf("world/local: image digest 불일치 (요청=%q inspect=%q)", requested.Digest(), got)
	}
	declared, err := parseImageIdentity(images[0].Config.User)
	if err != nil {
		return world.ImageReference{}, err
	}
	if declared != identity {
		return world.ImageReference{}, fmt.Errorf("world/local: image UID/GID와 SpawnSpec 불일치 (image=%d:%d spec=%d:%d)",
			declared.UID, declared.GID, identity.UID, identity.GID)
	}
	return world.NewImageReference(requested.Repository(), got), nil
}

func validateImageReference(ref world.ImageReference) error {
	if err := validateRepository(ref.Repository()); err != nil {
		return err
	}
	if !digestPattern.MatchString(ref.Digest()) {
		return fmt.Errorf("digest는 sha256 형식이어야 함(tag 금지): %q", ref.Digest())
	}
	return nil
}

func validateRepository(repository string) error {
	if repository == "" || strings.ContainsAny(repository, "@ \t\n\r") {
		return fmt.Errorf("repository 형식 오류: %q", repository)
	}
	last := repository[strings.LastIndex(repository, "/")+1:]
	if last == "" || strings.Contains(last, ":") {
		return fmt.Errorf("repository에는 tag를 포함할 수 없음: %q", repository)
	}
	return nil
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
	runner           commandRunner
	containerID      string
	proxyID          string
	internalNetwork  string
	externalNetwork  string
	stateDir         string
	upperDir         string
	workDir          string
	cidFile          string
	proxyCIDFile     string
	metadata         world.SpawnMetadata
	broker           effectBroker
	effects          <-chan world.EffectAttempt
	effectsDone      <-chan struct{}
	approval         *approvalBroker
	process          *processBroker
	processEndpoint  world.ProcessEndpoint
	approvalEndpoint world.ApprovalEndpoint

	closeToken       chan struct{}
	processesStopped bool
	closed           bool
	closeErr         error
}

var _ world.ActiveLease = (*lease)(nil)

func (l *lease) ProcessEndpoint() world.ProcessEndpoint   { return l.processEndpoint }
func (l *lease) ApprovalEndpoint() world.ApprovalEndpoint { return l.approvalEndpoint }

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

	joined := l.closeErr
	// Quiesce approval before stopping the agent so every pending hook receives
	// a durable forced deny through the host adapter. This is control-plane
	// quiescing, not mount/network cleanup; container stop still precedes effect
	// drain and resource removal as required by world.ActiveLease.
	if l.approval != nil {
		if err := l.approval.Shutdown(ctx); err != nil {
			joined = errors.Join(joined, fmt.Errorf("world/local: approval drain: %w", err))
		}
	}
	if !l.processesStopped {
		if err := ctx.Err(); err != nil {
			return errors.Join(joined, err)
		}
		if l.process != nil {
			if err := l.process.Shutdown(ctx); err != nil {
				joined = errors.Join(joined, fmt.Errorf("world/local: agent process broker: %w", err))
			}
		}
		for _, process := range []struct{ name, id string }{{"proxy", l.proxyID}} {
			if _, err := l.runner.Run(ctx, "stop", "--time", "10", process.id); err != nil {
				joined = errors.Join(joined, fmt.Errorf("world/local: %s container stop: %w", process.name, err))
			}
			if _, err := l.runner.Run(ctx, "wait", process.id); err != nil {
				joined = errors.Join(joined, fmt.Errorf("world/local: %s container wait: %w", process.name, err))
			}
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(joined, err)
		}
		l.processesStopped, l.closeErr = true, joined
	}
	if err := l.broker.Shutdown(ctx); err != nil {
		return errors.Join(joined, fmt.Errorf("world/local: audit drain/ACK: %w", err))
	}
	// The broker closes effectsDone only after its listener and handlers have
	// stopped and every accepted attempt has left the bounded stream.
	select {
	case <-l.effectsDone:
		joined = errors.Join(joined, l.broker.Err())
	case <-ctx.Done():
		return errors.Join(joined, ctx.Err())
	}

	cleanupCtx, cancel := cleanupContext(ctx)
	for _, container := range []struct {
		name, id string
	}{{"agent", l.containerID}, {"proxy", l.proxyID}} {
		if _, err := l.runner.Run(cleanupCtx, "rm", "--force", container.id); err != nil {
			joined = errors.Join(joined, fmt.Errorf("world/local: %s container cleanup: %w", container.name, err))
		}
	}
	for _, network := range []string{l.internalNetwork, l.externalNetwork} {
		if _, err := l.runner.Run(cleanupCtx, "network", "rm", "--force", network); err != nil {
			joined = errors.Join(joined, fmt.Errorf("world/local: network cleanup %s: %w", network, err))
		}
	}
	cancel()
	if err := os.RemoveAll(l.broker.SocketDir()); err != nil {
		joined = errors.Join(joined, fmt.Errorf("world/local: audit socket cleanup: %w", err))
	}
	if l.approval != nil {
		if err := l.approval.Cleanup(); err != nil {
			joined = errors.Join(joined, fmt.Errorf("world/local: approval socket cleanup: %w", err))
		}
	}
	// upper is intentionally preserved for T11. Agent termination is not a
	// collector ACK and therefore must not erase filesystem evidence.
	if err := os.RemoveAll(l.workDir); err != nil {
		joined = errors.Join(joined, fmt.Errorf("world/local: workdir cleanup: %w", err))
	}
	if err := os.Remove(l.cidFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		joined = errors.Join(joined, fmt.Errorf("world/local: cidfile cleanup: %w", err))
	}
	if err := os.Remove(l.proxyCIDFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		joined = errors.Join(joined, fmt.Errorf("world/local: proxy cidfile cleanup: %w", err))
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
