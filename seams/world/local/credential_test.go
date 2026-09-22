package local

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/world/local/egressproxy"
)

// proxyCredentialSpec builds a spawn spec whose egress admits the injection
// domain and which routes a proxy-held credential (never a container secret).
func proxyCredentialSpec(lower, digest, domain, header, prefix, credName, credValue string) world.SpawnSpec {
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "profile", Workspace: lower, FSScope: []string{lower}, Egress: []string{domain},
		Budget:   gen.Budget{Tokens: 10, TimeMs: 1000, MaxDepth: 2},
		Approval: policy.ApprovalManual,
	})
	spec := world.NewSpawnSpec(
		effective, world.NewImageReference(testAgentRepository, digest), []string{"agent", "--serve"}, 0,
		strings.Repeat("1", 32), strings.Repeat("2", 16),
		world.AgentIdentity{UID: 1000, GID: 1001}, nil,
	).WithAgentEnv([]string{"ANTHROPIC_BASE_URL=http://" + domain})
	credential, err := world.NewProxyCredential(credName, credValue, time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		panic(err)
	}
	rules := []world.ProxyInjection{{Domain: domain, Header: header, ValuePrefix: prefix, CredentialName: credName}}
	return spec.WithProxyCredential(rules, []world.ProxyCredential{credential})
}

// TestProxyCredentialAbsentFromContainerYetServedToProxy is 완료 기준 ①·⑤: the
// credential value is absent from every Podman argv, from every environment
// passed to Podman, from the agent container's --env set, and from durable
// spawn metadata — while it is delivered to the proxy sidecar only over the
// mounted credential socket. The proxy create args carry only the (non-secret)
// injection rule names, the credential socket path, and the socket mount.
func TestProxyCredentialAbsentFromContainerYetServedToProxy(t *testing.T) {
	const sentinel = "sk-proxy-held-secret-DO-NOT-LEAK-01"
	digest := "sha256:" + strings.Repeat("a", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	spec := proxyCredentialSpec(lower, digest, "api.anthropic.com", "Authorization", "Bearer ", "CLAUDE_CODE_OAUTH_TOKEN", sentinel)

	leaseValue, err := openTestLease(t, backend, context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	got := leaseValue.(*lease)
	defer got.Close(context.Background())

	// ① sentinel never appears in any Podman argv.
	for _, call := range runner.snapshot() {
		if strings.Contains(strings.Join(call, " "), sentinel) {
			t.Fatalf("credential이 Podman argv에 노출됨: %v", call)
		}
	}
	// ⑤ sentinel never appears in any environment passed to Podman, and the
	// credential is never injected into the container as env at all.
	for _, envCall := range runner.envSnapshot() {
		for _, entry := range envCall.env {
			if strings.Contains(entry, sentinel) {
				t.Fatalf("credential이 Podman 환경으로 전달됨: %q", entry)
			}
		}
	}

	proxyCreate := joinCall(t, runner.snapshot(), "proxy create")
	for _, required := range []string{
		"--inject api.anthropic.com|Authorization|Bearer |CLAUDE_CODE_OAUTH_TOKEN",
		"--credential-socket " + credentialSocketPath,
		":" + credentialMount + ":ro",
	} {
		if !strings.Contains(proxyCreate, required) {
			t.Errorf("proxy create args에 %q 없음: %s", required, proxyCreate)
		}
	}

	agentCreate := joinCall(t, runner.snapshot(), "agent create")
	if !strings.Contains(agentCreate, "ANTHROPIC_BASE_URL=http://api.anthropic.com") {
		t.Errorf("agent create가 평문 ANTHROPIC_BASE_URL을 받지 못함: %s", agentCreate)
	}
	// The credential NAME must also never reach the agent container (env-injection
	// path is not used for proxy-held credentials).
	if strings.Contains(agentCreate, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("credential 이름이 agent 컨테이너에 노출됨: %s", agentCreate)
	}

	metadata, err := json.Marshal(got.metadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), sentinel) || strings.Contains(string(metadata), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("spawn metadata에 credential이 노출됨: %s", metadata)
	}

	// The value reaches the proxy ONLY over the mounted socket. Fetch it as the
	// sidecar would, proving the in-memory channel works end to end.
	if got.credential == nil {
		t.Fatal("credential broker가 시작되지 않음")
	}
	fetched, err := egressproxy.UnixCredentialSource{Path: got.credential.SocketDir() + "/" + credentialSocketName}.
		Fetch(context.Background(), []string{"CLAUDE_CODE_OAUTH_TOKEN"})
	if err != nil {
		t.Fatalf("credential socket fetch 실패: %v", err)
	}
	if fetched["CLAUDE_CODE_OAUTH_TOKEN"] != sentinel {
		t.Fatalf("socket이 credential 값을 전달하지 못함")
	}
}

// TestProxyCredentialMultiVendorConfigOnly is 완료 기준 ③: two vendors of the
// same model differ only in their injection rule and credential name/value; the
// identical assertions hold for both. This is the config-only vendor switch.
func TestProxyCredentialMultiVendorConfigOnly(t *testing.T) {
	vendors := []struct {
		name, domain, header, prefix, credName, credValue string
	}{
		{"anthropic", "api.anthropic.com", "Authorization", "Bearer ", "CLAUDE_CODE_OAUTH_TOKEN", "sk-vendorA-DO-NOT-LEAK"},
		{"vendor2", "api.vendor2.example", "x-api-key", "", "VENDOR2_API_KEY", "key-vendorB-DO-NOT-LEAK"},
	}
	for _, v := range vendors {
		t.Run(v.name, func(t *testing.T) {
			digest := "sha256:" + strings.Repeat("b", 64)
			lower, stateRoot := testDirs(t)
			runner := newFakePodman(digest)
			backend := mustBackend(t, stateRoot, runner, statDevice)
			spec := proxyCredentialSpec(lower, digest, v.domain, v.header, v.prefix, v.credName, v.credValue)

			leaseValue, err := openTestLease(t, backend, context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			got := leaseValue.(*lease)
			defer got.Close(context.Background())

			for _, call := range runner.snapshot() {
				if strings.Contains(strings.Join(call, " "), v.credValue) {
					t.Fatalf("%s credential이 Podman argv에 노출됨", v.name)
				}
			}
			proxyCreate := joinCall(t, runner.snapshot(), "proxy create")
			wantInject := "--inject " + v.domain + "|" + v.header + "|" + v.prefix + "|" + v.credName
			if !strings.Contains(proxyCreate, wantInject) {
				t.Fatalf("%s proxy create args에 %q 없음: %s", v.name, wantInject, proxyCreate)
			}
			fetched, err := egressproxy.UnixCredentialSource{Path: got.credential.SocketDir() + "/" + credentialSocketName}.
				Fetch(context.Background(), []string{v.credName})
			if err != nil {
				t.Fatalf("%s credential fetch 실패: %v", v.name, err)
			}
			if fetched[v.credName] != v.credValue {
				t.Fatalf("%s socket이 credential 값을 전달하지 못함", v.name)
			}
		})
	}
}

// TestExpiredProxyCredentialFailsBeforeRuntimeSideEffects mirrors the secret
// path: a proxy credential that expires before the budget window is refused
// before any network/container is created, and the error does not leak the value.
func TestExpiredProxyCredentialFailsBeforeRuntimeSideEffects(t *testing.T) {
	const sentinel = "sk-expired-proxy-secret-DO-NOT-LEAK"
	digest := "sha256:" + strings.Repeat("c", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend := mustBackend(t, stateRoot, runner, statDevice)
	credential, err := world.NewProxyCredential("CLAUDE_CODE_OAUTH_TOKEN", sentinel, time.Now().Add(time.Second).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	base := proxyCredentialSpec(lower, digest, "api.anthropic.com", "Authorization", "Bearer ", "CLAUDE_CODE_OAUTH_TOKEN", sentinel)
	rules := []world.ProxyInjection{{Domain: "api.anthropic.com", Header: "Authorization", ValuePrefix: "Bearer ", CredentialName: "CLAUDE_CODE_OAUTH_TOKEN"}}
	spec := base.WithProxyCredential(rules, []world.ProxyCredential{credential})

	_, err = openTestLease(t, backend, context.Background(), spec)
	if err == nil {
		t.Fatal("예산보다 빨리 만료되는 proxy credential을 수락함")
	}
	if containsCall(runner.snapshot(), "network create") || containsAnyCreate(runner.snapshot()) {
		t.Fatalf("만료가 runtime side effect까지 도달함: %v", callKeys(runner.snapshot()))
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("만료 오류에 credential 원문이 노출됨: %v", err)
	}
}

func joinCall(t *testing.T, calls [][]string, key string) string {
	t.Helper()
	for _, call := range calls {
		if commandKey(call) == key {
			return strings.Join(call, " ")
		}
	}
	t.Fatalf("%s 호출을 찾지 못함: %v", key, callKeys(calls))
	return ""
}
