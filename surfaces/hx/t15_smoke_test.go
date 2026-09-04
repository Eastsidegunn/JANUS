//go:build t15smoke

package main

// This is the human-only T15 smoke. It deliberately uses the production
// world/adapter assembly and a real Claude container; it is not a fake
// backend and is never selected by CI. Podman on macOS is the Linux VM
// runtime, so the world backend is enabled through its t15smoke-only entry
// point.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
	localworld "github.com/Eastsidegunn/JANUS/seams/world/local"
)

const t15SmokeTokenEnv = "HX_T15_ACCESS_TOKEN"

// TestT15HumanSmoke is intentionally not run by any workflow. An operator
// supplies a short-lived OAuth access token in HX_T15_ACCESS_TOKEN and the
// test proves the four runbook checkpoints against the real production path.
func TestT15HumanSmoke(t *testing.T) {
	token := os.Getenv(t15SmokeTokenEnv)
	if token == "" {
		t.Fatal("H smoke: HX_T15_ACCESS_TOKEN이 비어 있음 (값은 로그에 입력하거나 기록하지 말 것)")
	}
	// Do not allow an ambient credential to become an accidental second input.
	_ = os.Unsetenv(world.ClaudeOAuthTokenEnv)
	_ = os.Unsetenv(t15SmokeTokenEnv)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	requirePodmanPreconditions(t, ctx)
	artifacts := buildIntegrationArtifacts(t, ctx)
	claudeRepo, claudeDigest := buildClaudeImage(t, ctx, artifacts.root)
	adapter := buildT15SmokeAdapter(t, ctx, artifacts.root)
	backend := newT15SmokeBackend(t, filepath.Join(t.TempDir(), "state"), artifacts)

	capability, err := world.NewSecretCapability(
		world.ClaudeOAuthTokenEnv, token, time.Now().Add(15*time.Minute).UnixMilli(),
	)
	if err != nil {
		t.Fatal(err)
	}

	// (a)–(c): one denied and one allowed real tool request. The instruction
	// asks Claude to use only the named tools; the decider denies anything else.
	runT15SmokeCase(t, ctx, backend, claudeRepo, claudeDigest, adapter, capability, false)
	runT15SmokeCase(t, ctx, backend, claudeRepo, claudeDigest, adapter, capability, true)
	runT15ExpiryCase(t, ctx, backend, claudeRepo, claudeDigest, adapter, token)

	// (d) expiry is checked before any Podman resource is created. This uses the
	// same in-memory token only to exercise the timing gate; it is never logged,
	// serialized, or sent to a child.
	expired, err := world.NewSecretCapability(
		world.ClaudeOAuthTokenEnv, token, time.Now().Add(-time.Minute).UnixMilli(),
	)
	if err != nil {
		t.Fatal(err)
	}
	lower, _ := integrationPaths(t)
	traceID, parentSpan, childSpan := logd.NewTraceID(), logd.NewSpanID(), logd.NewSpanID()
	budget := gen.Budget{Tokens: 100_000, TimeMs: 180_000, MaxDepth: 2}
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "t15-human-expired", Workspace: lower, FSScope: []string{lower},
		Egress: []string{"example.com"}, Budget: budget, Approval: policy.ApprovalManual,
	})
	spawn := world.NewSpawnSpec(effective, world.NewImageReference(claudeRepo, claudeDigest), []string{"claude"}, 0,
		traceID, childSpan, world.AgentIdentity{UID: 1000, GID: 1000}, nil).WithSecretCapability(expired)
	store := newIntegrationStore(t, filepath.Join(t.TempDir(), "expired.ndjson"), false)
	writer, err := logd.NewWriter(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.InitBatch(ctx, []gen.EventRecord{{Ts: time.Now().UnixMilli(), TraceID: traceID, SpanID: parentSpan, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	_, err = startProductionWorld(ctx, worldLaunch{
		Backend: backend, SpawnSpec: spawn, Writer: writer, TraceID: traceID, ParentSpan: parentSpan,
		AdapterCommand: []string{adapter}, AdapterName: "claudecode", AdapterStderr: ioDiscard{},
		AdapterBaseEnv: []string{"PATH=" + os.Getenv("PATH")},
		Instruction:    "이 실행은 시작되면 안 된다.", Workspace: "/workspace", Budget: budget, Depth: 0, ProfileID: "t15-human-expired",
		Approval: subagent.Spec{Approval: policy.ApprovalManual, Decider: policy.DenyAll{}},
	})
	if err == nil || !strings.Contains(err.Error(), "만료") {
		t.Fatalf("만료 token이 명시적으로 거부되지 않음: %v", err)
	}
	assertNoRuntimeArtifacts(t, ctx, claudeRepo+"@"+claudeDigest, childSpan)

	t.Log("T15 H smoke: (a) 인증·경계, (b) 승인 relay, (c) overlay/egress, (d) redaction·종료 확인 완료")
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

type t15SmokeDecider struct {
	allow bool
	mu    sync.Mutex
	calls int
}

func (d *t15SmokeDecider) Decide(_ context.Context, req policy.ApprovalRequest) (policy.ApprovalDecision, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	if d.allow && (req.ToolName == "Write" || req.ToolName == "Bash") {
		return policy.ApprovalDecision{Allow: true}, nil
	}
	return policy.ApprovalDecision{Reason: "T15 smoke deny"}, nil
}

func buildT15SmokeAdapter(t *testing.T, ctx context.Context, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claudecode")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", path, "./seams/subagent/claudecode/cmd/claudecode")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("VERIFICATION: host Claude adapter build failed: %v\n%s", err, out)
	}
	return path
}

func newT15SmokeBackend(t *testing.T, stateRoot string, artifacts integrationArtifacts) world.Backend {
	t.Helper()
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := localworld.NewBackendForT15Smoke(localworld.Config{
		StateRoot: stateRoot, ProxyImageRepository: artifacts.proxyRepository, ProxyImageDigest: artifacts.proxyDigest,
		ProxyIdentity: world.AgentIdentity{UID: 1000, GID: 1000}, AuditQueueCapacity: 64, ApprovalCapacity: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func runT15SmokeCase(t *testing.T, parent context.Context, backend world.Backend, repo, digest, adapter string, secret world.SecretCapability, allow bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	lower, _ := integrationPaths(t)
	store := newIntegrationStore(t, filepath.Join(t.TempDir(), "events.ndjson"), false)
	writer, err := logd.NewWriter(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	traceID, parentSpan, childSpan := logd.NewTraceID(), logd.NewSpanID(), logd.NewSpanID()
	mustWrite(t, filepath.Join(lower, "t15-lower-sentinel.txt"), "lower-is-immutable\n")
	if err := writer.InitBatch(ctx, []gen.EventRecord{{Ts: time.Now().UnixMilli(), TraceID: traceID, SpanID: parentSpan, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	budget := gen.Budget{Tokens: 100_000, TimeMs: 180_000, MaxDepth: 2}
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "t15-human-smoke", Workspace: lower, FSScope: []string{lower}, Egress: []string{"example.com"},
		Budget: budget, Approval: policy.ApprovalManual,
	})
	spawn := world.NewSpawnSpec(effective, world.NewImageReference(repo, digest), []string{"claude"}, 0,
		traceID, childSpan, world.AgentIdentity{UID: 1000, GID: 1000}, nil).WithSecretCapability(secret)
	var stderr bytes.Buffer
	active, err := startProductionWorld(ctx, worldLaunch{
		Backend: backend, SpawnSpec: spawn, Writer: writer, TraceID: traceID, ParentSpan: parentSpan,
		AdapterCommand: []string{adapter}, AdapterName: "claudecode", AdapterStderr: &stderr,
		AdapterBaseEnv: []string{"PATH=" + os.Getenv("PATH")},
		Instruction:    "Use the Write tool to create /workspace/t15-smoke-marker.txt containing exactly T15-SMOKE. Then use the Bash tool to run `curl -fsS --max-time 5 https://example.com/ >/dev/null || true` and `curl --max-time 2 http://1.1.1.1/ >/dev/null || true`. Then respond exactly T15_SMOKE_DONE.",
		Workspace:      "/workspace", Budget: budget, Depth: 0, ProfileID: "t15-human-smoke",
		Approval: subagent.Spec{Approval: policy.ApprovalManual, Decider: &t15SmokeDecider{allow: allow}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = active.Lease.Close(context.Background()) }()
	waitRecord(t, store, gen.KindSubagentReady, 90*time.Second)
	cid := findAgentContainer(t, ctx, repo+"@"+digest)
	assertContainerIsolation(t, ctx, cid, adapter, active.Lease)
	marker := filepath.Join(active.Lease.UpperDir(), "t15-smoke-marker.txt")
	waitCtx, waitCancel := context.WithTimeout(ctx, 4*time.Minute)
	done, waitErr := active.Subagent.Wait(waitCtx)
	waitCancel()
	markerErr := waitForPath(marker, allow, 20*time.Second)
	if markerErr != nil {
		t.Fatal(markerErr)
	}
	if waitErr != nil && allow {
		t.Fatalf("Claude allow 실행 실패: %v", waitErr)
	}
	effects, effectErr := active.EffectSnapshot()
	if effectErr != nil {
		t.Fatal(effectErr)
	}
	if allow {
		var sawAllowed, sawDenied bool
		for _, effect := range effects {
			if effect.Target == "example.com" && effect.Decision == world.EffectDecisionAllow {
				sawAllowed = true
			}
			if effect.Target == "1.1.1.1" && effect.Decision == world.EffectDecisionDeny {
				sawDenied = true
			}
		}
		if !sawAllowed || !sawDenied {
			t.Fatalf("egress 관측 부족: allowed_example=%t denied_ip=%t", sawAllowed, sawDenied)
		}
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 45*time.Second)
	closeErr := active.FinalizeCollection(closeCtx)
	closeCancel()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	assertNoRuntimeArtifacts(t, ctx, repo+"@"+digest, childSpan)
	data, err := os.ReadFile(filepath.Join(lower, "t15-lower-sentinel.txt"))
	if err != nil || string(data) != "lower-is-immutable\n" {
		t.Fatalf("lower가 변경됨: err=%v", err)
	}
	if done.Status == "" {
		t.Fatal("subagent/done이 비어 있음")
	}
	recordBytes, err := json.Marshal(store.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(recordBytes, []byte(secret.Value())) || strings.Contains(stderr.String(), secret.Value()) {
		t.Fatal("access token이 durable record 또는 stderr에 노출됨")
	}
	if !hasKind(store.snapshot(), gen.KindSubagentApprovalRequest) {
		t.Fatal("approval_request가 없어 hook/relay가 발화하지 않음")
	}
	if allow && !hasKind(store.snapshot(), gen.KindCollectorFsChanged) {
		t.Fatal("allow 실행 후 collector/fs_changed가 없음")
	}
	if !allow && hasMarkerRecord(store.snapshot(), "t15-smoke-marker.txt") {
		t.Fatal("deny 실행 뒤 marker 효과가 기록됨")
	}
	t.Logf("T15 H smoke case allow=%t: done=%s approval=1 token=redacted", allow, done.Status)
}

func runT15ExpiryCase(t *testing.T, parent context.Context, backend world.Backend, repo, digest, adapter, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	lower, _ := integrationPaths(t)
	store := newIntegrationStore(t, filepath.Join(t.TempDir(), "expiry.ndjson"), false)
	writer, err := logd.NewWriter(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	traceID, parentSpan, childSpan := logd.NewTraceID(), logd.NewSpanID(), logd.NewSpanID()
	if err := writer.InitBatch(ctx, []gen.EventRecord{{Ts: time.Now().UnixMilli(), TraceID: traceID, SpanID: parentSpan, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	budget := gen.Budget{Tokens: 100_000, TimeMs: 0, MaxDepth: 2}
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "t15-human-expiry", Workspace: lower, FSScope: []string{lower}, Egress: []string{"example.com"},
		Budget: budget, Approval: policy.ApprovalManual,
	})
	// Leave enough time for the VM/container handshake, then make the command
	// outlive the capability so expiry is observed by the running adapter.
	secret, err := world.NewSecretCapability(world.ClaudeOAuthTokenEnv, token, time.Now().Add(90*time.Second).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	spawn := world.NewSpawnSpec(effective, world.NewImageReference(repo, digest), []string{"claude"}, 0,
		traceID, childSpan, world.AgentIdentity{UID: 1000, GID: 1000}, nil).WithSecretCapability(secret)
	var stderr bytes.Buffer
	active, err := startProductionWorld(ctx, worldLaunch{
		Backend: backend, SpawnSpec: spawn, Writer: writer, TraceID: traceID, ParentSpan: parentSpan,
		AdapterCommand: []string{adapter}, AdapterName: "claudecode", AdapterStderr: &stderr,
		AdapterBaseEnv: []string{"PATH=" + os.Getenv("PATH")},
		Instruction:    "Use the Bash tool to run `sleep 120`, then respond exactly T15_EXPIRY_DONE. Do not use any other tool.",
		Workspace:      "/workspace", Budget: budget, Depth: 0, ProfileID: "t15-human-expiry",
		Approval: subagent.Spec{Approval: policy.ApprovalManual, Decider: &t15SmokeDecider{allow: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = active.Lease.Close(context.Background()) }()
	waitRecord(t, store, gen.KindSubagentReady, 90*time.Second)
	waitCtx, waitCancel := context.WithTimeout(ctx, 150*time.Second)
	done, waitErr := active.Subagent.Wait(waitCtx)
	waitCancel()
	if waitErr != nil || done.Status != gen.DonePayloadStatusError || done.Result != "token expired" {
		t.Fatalf("실행 중 token 만료가 fail-closed로 관측되지 않음: status=%s result=%q err=%v", done.Status, done.Result, waitErr)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 45*time.Second)
	closeErr := active.FinalizeCollection(closeCtx)
	closeCancel()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	assertNoRuntimeArtifacts(t, ctx, repo+"@"+digest, childSpan)
	recordBytes, err := json.Marshal(store.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(recordBytes, []byte(token)) || strings.Contains(stderr.String(), token) {
		t.Fatal("만료 경로에서 access token이 기록 또는 stderr에 노출됨")
	}
	t.Log("T15 H smoke expiry: 실행 중 만료가 done{error, token expired}로 관측됨")
}

func waitForPath(path string, wantPresent bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		present := err == nil
		if present == wantPresent {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("marker presence=%t, want=%t", fileExists(path), wantPresent)
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func hasKind(records []gen.EventRecord, kind gen.Kind) bool {
	for _, record := range records {
		if record.Kind == kind {
			return true
		}
	}
	return false
}

func hasMarkerRecord(records []gen.EventRecord, name string) bool {
	for _, record := range records {
		if record.Kind != gen.KindCollectorFsChanged {
			continue
		}
		var payload gen.FsChangedPayload
		if json.Unmarshal(record.Payload, &payload) != nil {
			continue
		}
		for _, change := range payload.Changes {
			if change.Path == name {
				return true
			}
		}
	}
	return false
}
