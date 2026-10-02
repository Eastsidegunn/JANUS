package local

import (
	"context"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"testing"
)

// SCP-T26-001 / FR-SBX-03: world-config egress_pins in the local backend.

func pinnedTestConfig(stateRoot string, pins map[string]string) Config {
	config := testConfig(stateRoot)
	config.EgressPins = map[string]netip.AddrPort{}
	for domain, address := range pins {
		config.EgressPins[domain] = netip.MustParseAddrPort(address)
	}
	return config
}

func proxyArgsForTest(backend *Backend) []string {
	return backend.proxyCreateArgs(
		[]string{"api.example.com", "b.example"}, testProxyRepository+"@"+testProxyDigest,
		"/state/audit", "/state/proxy.cid", "hx-x-proxy", "hx-x-internal", "hx-x-egress",
	)
}

// (e) 핀이 없을 때 proxyCreateArgs argv는 핀 도입 이전과 바이트 동일.
func TestProxyCreateArgsWithoutPinsIsByteIdentical(t *testing.T) {
	_, stateRoot := testDirs(t)
	want := []string{
		"create", "--cidfile", "/state/proxy.cid", "--name", "hx-x-proxy",
		"--pull=never", "--network", "hx-x-internal", "--network", "hx-x-egress",
		"--network-alias", "hx-x-proxy",
		"--network-alias", "hx-egress-proxy",
		"--userns=keep-id:uid=65532,gid=65532", "--user", "65532:65532",
		"--read-only", "--cap-drop=all", "--security-opt=no-new-privileges",
		"--entrypoint", "/hxegressproxy",
		"--volume", "/state/audit:/run/hx-audit:ro",
		"localhost/hx-egress-proxy@sha256:7777777777777777777777777777777777777777777777777777777777777777",
		"--listen", ":3128", "--audit-socket", "/run/hx-audit/audit.sock",
		"--allow", "api.example.com", "--allow", "b.example",
	}
	for name, config := range map[string]Config{
		"nil pins":   testConfig(stateRoot),
		"empty pins": pinnedTestConfig(stateRoot, nil),
	} {
		t.Run(name, func(t *testing.T) {
			backend, err := newBackend(config, newFakePodman(testProxyDigest), statDevice, newFakeEffectBroker)
			if err != nil {
				t.Fatal(err)
			}
			got := proxyArgsForTest(backend)
			if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("핀 없는 proxy argv가 달라짐:\n got=%q\nwant=%q", got, want)
			}
		})
	}
}

// (e) 핀이 있으면 기존 argv 뒤에 --pin domain=ip:port가 정렬 순으로 붙는다.
func TestProxyCreateArgsAppendsPinFlags(t *testing.T) {
	_, stateRoot := testDirs(t)
	plain, err := newBackend(testConfig(stateRoot), newFakePodman(testProxyDigest), statDevice, newFakeEffectBroker)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := newBackend(pinnedTestConfig(stateRoot, map[string]string{
		"b.example": "[fd00::7]:8443", "API.example.com": "100.87.195.120:8317",
	}), newFakePodman(testProxyDigest), statDevice, newFakeEffectBroker)
	if err != nil {
		t.Fatal(err)
	}
	base := proxyArgsForTest(plain)
	got := proxyArgsForTest(pinned)
	want := append(append([]string(nil), base...),
		"--pin", "api.example.com=100.87.195.120:8317",
		"--pin", "b.example=[fd00::7]:8443",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned proxy argv:\n got=%q\nwant=%q", got, want)
	}
}

func TestNewBackendRejectsInvalidEgressPins(t *testing.T) {
	for name, pins := range map[string]map[string]string{
		"loopback":    {"api.example.com": "127.0.0.1:8317"},
		"link-local":  {"api.example.com": "169.254.169.254:80"},
		"multicast":   {"api.example.com": "239.1.1.1:80"},
		"unspecified": {"api.example.com": "0.0.0.0:80"},
		"IP literal":  {"10.0.0.1": "10.0.0.1:80"},
		"상한 초과": {
			"a.example": "10.0.0.1:1", "b.example": "10.0.0.1:1", "c.example": "10.0.0.1:1",
			"d.example": "10.0.0.1:1", "e.example": "10.0.0.1:1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, stateRoot := testDirs(t)
			if _, err := newBackend(pinnedTestConfig(stateRoot, pins), newFakePodman(testProxyDigest), statDevice, newFakeEffectBroker); err == nil {
				t.Fatalf("불량 pin %v로 backend 생성됨", pins)
			}
		})
	}
}

// (c) 핀 domain ∉ 병합 정책 allowlist → Prepare 거부, image inspect·overlay·
// network/container 생성 이전(외부 효과 이전).
func TestPreparePinOutsideAllowlistFailsBeforeRuntimeEffects(t *testing.T) {
	digest := "sha256:" + strings.Repeat("9", 64)
	lower, stateRoot := testDirs(t)
	runner := newFakePodman(digest)
	backend, err := newBackend(pinnedTestConfig(stateRoot, map[string]string{
		"gateway.internal.example": "100.87.195.120:8317",
	}), runner, statDevice, newFakeEffectBroker)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := backend.Prepare(context.Background(), testSpec(lower, digest))
	if err == nil {
		_ = prepared.Abort(context.Background())
		t.Fatal("allowlist 밖 pin으로 Prepare 성공")
	}
	if !strings.Contains(err.Error(), "gateway.internal.example") || !strings.Contains(err.Error(), "allowlist 밖") {
		t.Fatalf("Prepare error = %v", err)
	}
	if got := callKeys(runner.snapshot()); !reflect.DeepEqual(got, []string{"info"}) {
		t.Fatalf("거부 전에 Podman 효과 발생: %v", got)
	}
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("거부된 Prepare가 state를 남김: %v", entries)
	}
}

// 정책 allowlist 안의 pin(정확 일치·label 경계 subdomain)은 sidecar에 --pin으로 전달.
func TestOpenPassesPinWithinAllowlistToProxy(t *testing.T) {
	for _, domain := range []string{"api.example.com", "gw.api.example.com"} {
		t.Run(domain, func(t *testing.T) {
			digest := "sha256:" + strings.Repeat("8", 64)
			lower, stateRoot := testDirs(t)
			runner := newFakePodman(digest)
			backend, err := newBackend(pinnedTestConfig(stateRoot, map[string]string{domain: "100.87.195.120:8317"}),
				runner, statDevice, newFakeEffectBroker)
			if err != nil {
				t.Fatal(err)
			}
			active, err := openTestLease(t, backend, context.Background(), testSpec(lower, digest))
			if err != nil {
				t.Fatal(err)
			}
			defer active.Close(context.Background())
			calls := runner.snapshot()
			var proxyCreate string
			for _, call := range calls {
				if commandKey(call) == "proxy create" {
					proxyCreate = strings.Join(call, " ")
				}
			}
			if !strings.Contains(proxyCreate, "--allow api.example.com --pin "+domain+"=100.87.195.120:8317") {
				t.Fatalf("proxy create에 pin 없음: %s", proxyCreate)
			}
		})
	}
}

func TestValidateEgressPinsWithinPolicy(t *testing.T) {
	pins, err := NormalizeEgressPins([]EgressPinConfig{{Domain: "gw.example.com", Address: "100.87.195.120:8317"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateEgressPinsWithinPolicy(pins, []string{"example.com"}); err != nil {
		t.Fatalf("label 경계 subdomain pin 거부: %v", err)
	}
	if err := ValidateEgressPinsWithinPolicy(pins, []string{"gw.example.com"}); err != nil {
		t.Fatalf("정확 일치 pin 거부: %v", err)
	}
	for _, egress := range [][]string{nil, {"other.example"}, {"evil-example.com"}, {"api.gw.example.com"}} {
		if err := ValidateEgressPinsWithinPolicy(pins, egress); err == nil {
			t.Errorf("egress %v 밖 pin 수용", egress)
		}
	}
	if err := ValidateEgressPinsWithinPolicy(nil, nil); err != nil {
		t.Fatalf("pin 없음은 항상 통과해야 함: %v", err)
	}
}
