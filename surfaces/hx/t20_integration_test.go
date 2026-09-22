//go:build t20integration

package main

// T20 Linux 실물 게이트: proxy-held credential 경로(credentialBroker + proxy
// 주입 규칙 + 실 agent 컨테이너)를 rootless Podman으로 조립하고, 파수꾼(가짜)
// credential 값으로 종단 배관을 관통 검증한다. 실 토큰은 필요 없다.
//
// overlay 종류는 무단정한다(fuse-overlayfs/native 무관). 이 게이트는 대표성
// 게이트가 아니라 [H] 서버 1차 배관 검증용이며, CI 편입은 리뷰어가 별도로
// 판단한다(.github/workflows/ci.yml에는 넣지 않는다).
//
// 검증 항목:
//   - 정적 alias(hx-egress-proxy)로 HTTP_PROXY를 준 agent의 평문 forward가
//     프록시에 도달함(감사 allow). 정적 alias가 없으면 forward 자체가 프록시에
//     닿지 못한다 — 따라서 이 게이트는 정적 alias의 실물 회귀 검증이다.
//   - 주입 대상 도메인 CONNECT 실물 거부(우회 구멍 없음, 완료 기준 ③ bypass).
//   - 미선언(허용) 도메인은 무주입 평문 forward(완료 기준 ④).
//   - `podman exec <agent> --dumpenv` 라이브 덤프·podman inspect(agent/proxy)·
//     agent argv·세션 로그·audit 어디에도 sentinel(값)·credential 이름 부재
//     (완료 기준 ⑤, t15의 forbidden 스캔 방식 계승).
//   - 실행 후 컨테이너·네트워크·credential 소켓 정리(잔존 0).
//
// 미실증(문서화): 프록시가 주입 헤더를 붙여 신뢰 가능한 TLS 업스트림으로 재발신해
// 업스트림이 헤더를 수신하는 부분(완료 기준 ②의 업스트림 수신)은 이 컨테이너
// 게이트에서 실증하지 않는다. scratch 프록시 이미지에 CA 번들이 없어 TLS 검증이
// 핸드셰이크 단계에서 실패하므로(=sentinel 미전송, fail-closed), 업스트림 수신은
// 관측 불가하다. 헤더 부착·TLS 재발신 자체는 egressproxy 단위 테스트
// (injection_test.go: 가짜 TLS 업스트림)에서 종단 실증되어 있다. 자세한 사유와
// 후보 해법은 BLOCKED.md의 T20-② 항목 참조.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
	localworld "github.com/Eastsidegunn/JANUS/seams/world/local"
)

// t20StaticAlias mirrors the unexported local.proxyStaticAlias. Hardcoding it
// here locks the on-the-wire alias contract that world-config env depends on.
const t20StaticAlias = "hx-egress-proxy"

const (
	t20Sentinel       = "sk-t20-proxy-held-sentinel-DO-NOT-LEAK-01"
	t20CredentialName = "CLAUDE_CODE_OAUTH_TOKEN"
	// Injection domain and an allowed-but-undeclared domain. Both are real,
	// resolvable, public domains so authorization reaches the injection-specific
	// CONNECT-deny check (a non-resolvable domain would short-circuit at DNS and
	// never exercise the bypass closure).
	t20InjectionDomain  = "example.com"
	t20UndeclaredDomain = "example.org"
)

func TestProxyHeldCredentialIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatalf("T20 proxy-held-credential 게이트는 Linux 실물 게이트다 (현재 %s); skip 금지", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	requirePodmanPreconditions(t, ctx)
	artifacts := buildIntegrationArtifacts(t, ctx)
	lower, stateRoot := integrationPaths(t)

	credSocketsBefore := listCredentialSockets(t)

	store := newIntegrationStore(t, filepath.Join(t.TempDir(), "events.ndjson"), true)
	writer, err := logd.NewWriter(ctx, store, logd.WithQueueCap(1))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	traceID, parentSpan, childSpan := logd.NewTraceID(), logd.NewSpanID(), logd.NewSpanID()
	if err := writer.InitBatch(ctx, []gen.EventRecord{{
		Ts: time.Now().UnixMilli(), TraceID: traceID, SpanID: parentSpan,
		Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}
	backend := newIntegrationBackend(t, stateRoot, artifacts)
	budget := gen.Budget{Tokens: 1_000_000, TimeMs: 240_000, MaxDepth: 2}
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "t20-proxy", Workspace: lower, FSScope: []string{lower},
		Egress: []string{t20InjectionDomain, t20UndeclaredDomain},
		Budget: budget, Approval: policy.ApprovalManual,
	})
	scenarioBytes, _ := json.Marshal(map[string]any{
		"mode": "t20proxy",
		// Static-alias forward path + undeclared (no-injection) forward.
		"allow_url": "http://" + t20UndeclaredDomain + "/",
		// Injection domain paths: plaintext forward (enters injection branch) and
		// an https CONNECT that must be denied (bypass closure).
		"injection_forward_url": "http://" + t20InjectionDomain + "/v1/messages",
		"injection_connect_url": "https://" + t20InjectionDomain + "/",
		"flood_count":           integrationFloodCount,
	})

	credential, err := world.NewProxyCredential(t20CredentialName, t20Sentinel, time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	rules := []world.ProxyInjection{{
		Domain: t20InjectionDomain, Header: "Authorization", ValuePrefix: "Bearer ", CredentialName: t20CredentialName,
	}}
	spawnSpec := world.NewSpawnSpec(
		effective, world.NewImageReference(artifacts.agentRepository, artifacts.agentDigest),
		[]string{"integration"}, 0, traceID, childSpan, world.AgentIdentity{UID: 1000, GID: 1000}, nil,
	).WithAgentEnv([]string{
		// Point the agent's forward proxy at the STATIC alias, overriding the
		// per-span dynamic HTTP_PROXY the backend injects. If the static alias were
		// not attached to the internal network, these forwards would never reach
		// the proxy and the audit-allow assertions below would fail.
		"HTTP_PROXY=http://" + t20StaticAlias + ":3128",
		"http_proxy=http://" + t20StaticAlias + ":3128",
		"HTTPS_PROXY=http://" + t20StaticAlias + ":3128",
		"https_proxy=http://" + t20StaticAlias + ":3128",
	}).WithProxyCredential(rules, []world.ProxyCredential{credential})

	active, err := startProductionWorld(ctx, worldLaunch{
		Backend: backend, SpawnSpec: spawnSpec, Writer: writer, TraceID: traceID, ParentSpan: parentSpan,
		AdapterCommand: []string{artifacts.adapter}, AdapterName: "world-testagent",
		ControlMode: gen.SubagentSpawnPayloadControlModeToolApproval, AdapterStderr: os.Stderr,
		Instruction: string(scenarioBytes), Workspace: localworld.ContainerWorkspacePath,
		Budget: budget, Depth: 0, ProfileID: "t20-proxy",
		Approval:       subagent.Spec{Approval: policy.ApprovalManual, Decider: policy.DenyAll{}},
		AdapterBaseEnv: []string{"PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalized := false
	defer func() {
		if !finalized {
			_ = active.Lease.Close(context.Background())
		}
	}()

	// The agent floods after its network probes, closing the writer gate. While
	// paused, the agent container is alive for the live env dump and inspection.
	waitSignal(t, store.floodSeen, 90*time.Second, "flood-start durable event")
	agentImage := artifacts.agentRepository + "@" + artifacts.agentDigest
	proxyImage := artifacts.proxyRepository + "@" + artifacts.proxyDigest
	agentCID := findAgentContainer(t, ctx, agentImage)
	proxyCID := findRunningContainer(t, ctx, proxyImage)

	// ⑤ Live env dump of the agent PID 1: neither the credential value nor its
	// name may appear. The scratch image has no coreutils, so the agent binary
	// prints /proc/1/environ itself.
	envDump := runOutput(t, ctx, "podman", "exec", agentCID, "/testagent", "--dumpenv")
	assertNoT20Secret(t, "agent live env dump", string(envDump))

	// ⑤ podman inspect of both containers (Config.Env, Cmd, CreateCommand, mounts).
	assertNoT20Secret(t, "agent inspect", string(runOutput(t, ctx, "podman", "inspect", agentCID)))
	assertNoT20Secret(t, "proxy inspect", string(runOutput(t, ctx, "podman", "inspect", proxyCID)))
	// ⑤ agent argv specifically.
	assertNoT20Secret(t, "agent argv", string(runOutput(t, ctx, "podman", "inspect", "--format", "{{json .Config.Cmd}} {{json .Config.Entrypoint}} {{json .Config.CreateCommand}}", agentCID)))

	store.releaseFlood()
	waitCtx, waitCancel := context.WithTimeout(ctx, 90*time.Second)
	done, waitErr := active.Subagent.Wait(waitCtx)
	waitCancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	closeErr := active.FinalizeCollection(closeCtx)
	closeCancel()
	finalized = true
	if waitErr != nil || done.Status != gen.DonePayloadStatusOk {
		t.Fatalf("t20 subagent: done=%+v err=%v", done, waitErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	// ⑤ session log: no credential value or name in any durable record.
	records := store.snapshot()
	recordBytes, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	assertNoT20Secret(t, "durable session log", string(recordBytes))

	// The agent must have observed the injection-domain CONNECT denial and both
	// forwards reaching the proxy.
	assertAgentMessages(t, records, map[string]bool{
		"injection-connect-denied": false,
		"allowed-forward-status=":  true, // prefix match
	})

	// ⑤/②-metadata + ③/④: audit effects carry metadata only (no credential),
	// prove the static-alias forward reached the proxy, and prove the injection
	// domain's CONNECT is denied by the bypass closure.
	effects, effectErr := active.EffectSnapshot()
	if effectErr != nil {
		t.Fatal(effectErr)
	}
	effectBytes, _ := json.Marshal(effects)
	assertNoT20Secret(t, "egress audit effects", string(effectBytes))
	for _, forbidden := range []string{"Authorization", "Bearer ", "Bearer"} {
		if strings.Contains(string(effectBytes), forbidden) {
			t.Fatalf("audit effects에 헤더/자격 흔적 유출: %q\n%s", forbidden, effectBytes)
		}
	}
	assertT20Effects(t, effects, childSpan)

	assertNoRuntimeArtifacts(t, ctx, agentImage, childSpan)

	// credential 소켓 정리: 이 실행이 만든 hxc-* 소켓 디렉터리가 남지 않아야 한다.
	credSocketsAfter := listCredentialSockets(t)
	for path := range credSocketsAfter {
		if !credSocketsBefore[path] {
			t.Fatalf("cleanup 뒤 credential 소켓 디렉터리 잔존: %s", path)
		}
	}
}

// assertNoT20Secret fails if the sentinel value or the credential name appears
// in the given live/durable surface (완료 기준 ⑤ 파수꾼 스캔).
func assertNoT20Secret(t *testing.T, where, blob string) {
	t.Helper()
	for _, forbidden := range []string{t20Sentinel, t20CredentialName} {
		if strings.Contains(blob, forbidden) {
			t.Fatalf("VERIFICATION: %s에 credential 흔적 유출: %q", where, forbidden)
		}
	}
}

// assertAgentMessages checks that the agent emitted the expected §5.2 messages.
// A prefix entry (value true) matches by HasPrefix; otherwise exact match.
func assertAgentMessages(t *testing.T, records []gen.EventRecord, want map[string]bool) {
	t.Helper()
	seen := map[string]bool{}
	for _, record := range records {
		if record.Kind != gen.KindSubagentMessage {
			continue
		}
		var payload gen.SubagentMessagePayload
		if json.Unmarshal(record.Payload, &payload) != nil {
			continue
		}
		for target, prefix := range want {
			if (prefix && strings.HasPrefix(payload.Text, target)) || (!prefix && payload.Text == target) {
				seen[target] = true
			}
		}
	}
	for target := range want {
		if !seen[target] {
			t.Fatalf("VERIFICATION: agent 메시지 %q 관측되지 않음", target)
		}
	}
}

// assertT20Effects verifies the audit stream: the injection domain's CONNECT is
// denied (③ bypass closure), the injection-domain plaintext forward is allowed
// (entered the injection branch), and the undeclared domain forward is allowed
// as a plain forward (④). All effects belong to the child span.
func assertT20Effects(t *testing.T, effects []world.EffectAttempt, childSpan string) {
	t.Helper()
	var injectionForwardAllow, undeclaredForwardAllow, injectionConnectDeny bool
	for _, effect := range effects {
		if effect.Kind == "egress" && effect.SpanID != "" && effect.SpanID != childSpan {
			t.Fatalf("egress effect span mismatch: got=%s want=%s (%+v)", effect.SpanID, childSpan, effect)
		}
		switch {
		case effect.Target == t20InjectionDomain && effect.Method == "CONNECT" && effect.Decision == world.EffectDecisionDeny:
			injectionConnectDeny = true
		case effect.Target == t20InjectionDomain && effect.Method == httpMethodPost && effect.Decision == world.EffectDecisionAllow:
			injectionForwardAllow = true
		case effect.Target == t20UndeclaredDomain && effect.Method == httpMethodPost && effect.Decision == world.EffectDecisionAllow:
			undeclaredForwardAllow = true
		}
	}
	if !injectionConnectDeny {
		t.Fatalf("VERIFICATION: 주입 대상 도메인 CONNECT deny 실물 관측되지 않음 (bypass 구멍): %+v", effects)
	}
	if !undeclaredForwardAllow {
		t.Fatalf("VERIFICATION: 정적 alias 경유 미선언 도메인 forward allow 관측되지 않음: %+v", effects)
	}
	if !injectionForwardAllow {
		t.Fatalf("VERIFICATION: 주입 도메인 평문 forward가 프록시에 도달(allow)하지 못함: %+v", effects)
	}
}

// findRunningContainer returns the single running container for an image.
func findRunningContainer(t *testing.T, ctx context.Context, image string) string {
	t.Helper()
	out := strings.Fields(string(runOutput(t, ctx, "podman", "ps", "--filter", "ancestor="+image, "--format", "{{.ID}}")))
	if len(out) != 1 {
		t.Fatalf("running container for %s = %v", image, out)
	}
	return out[0]
}

// listCredentialSockets snapshots the credential-broker socket roots (hxc-*
// under /tmp) so the test can prove none created during the run survive cleanup.
func listCredentialSockets(t *testing.T) map[string]bool {
	t.Helper()
	matches, err := filepath.Glob("/tmp/hxc-*")
	if err != nil {
		t.Fatal(err)
	}
	// A leftover matching path is only a real leak if its socket still exists.
	set := map[string]bool{}
	for _, path := range matches {
		if _, err := os.Stat(filepath.Join(path, "cred.sock")); err == nil {
			set[path] = true
		} else if info, err := os.Stat(path); err == nil && info.IsDir() {
			set[path] = true
		}
	}
	return set
}
