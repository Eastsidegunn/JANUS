package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/seams/accept"
)

// SCP-T26-001 / FR-SBX-03, FR-SBX-05: world-config egress_pins.

func sandboxWithEgress(egress ...string) policy.SandboxConfig {
	return policy.SandboxConfig{Egress: egress}
}

func assertKeyNotConsumed(t *testing.T, f *productionFixture) {
	t.Helper()
	registry, err := accept.Open(f.acceptRoot)
	if err != nil {
		t.Fatal(err)
	}
	status, err := registry.Lookup(f.request.Scope, f.request.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "not_submitted" {
		t.Fatalf("거부된 설정이 key를 소모함: %+v", status)
	}
}

// (d) 필드 부재·정상 핀 수용.
func TestParseWorldConfigAcceptsEgressPins(t *testing.T) {
	base, err := json.Marshal(validWorldConfig())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(base, []byte("egress_pins")) {
		t.Fatalf("핀 없는 설정 직렬화에 egress_pins가 나타남: %s", base)
	}
	cfg, err := parseWorldConfig(base)
	if err != nil || len(cfg.EgressPins) != 0 {
		t.Fatalf("egress_pins 부재 설정 거부: %v %+v", err, cfg.EgressPins)
	}
	for name, pins := range map[string][]worldEgressPin{
		"빈 목록":       {},
		"CGNAT":      {{Domain: "100-64-0-10.nip.io", Address: "100.64.0.10:8080"}},
		"RFC1918+v6": {{Domain: "gw.example", Address: "10.0.0.5:8080"}, {Domain: "gw6.example", Address: "[fd00::5]:443"}},
		"상한 4": {
			{Domain: "a.example", Address: "10.0.0.1:1"}, {Domain: "b.example", Address: "10.0.0.1:2"},
			{Domain: "c.example", Address: "10.0.0.1:3"}, {Domain: "d.example", Address: "10.0.0.1:65535"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validWorldConfig()
			cfg.EgressPins = pins
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseWorldConfig(data)
			if err != nil {
				t.Fatalf("정상 egress_pins 거부: %v", err)
			}
			if len(got.EgressPins) != len(pins) {
				t.Fatalf("egress_pins = %+v", got.EgressPins)
			}
		})
	}
}

// (d) 불량 핀은 claim 이전 world config 오류로 거부 — CLI 진입점도 key 미소모.
func TestParseWorldConfigRejectsBadEgressPins(t *testing.T) {
	gw := func(address string) []worldEgressPin {
		return []worldEgressPin{{Domain: "gw.example", Address: address}}
	}
	cases := map[string][]worldEgressPin{
		"loopback":          gw("127.0.0.1:8080"),
		"loopback v6":       gw("[::1]:8080"),
		"link-local":        gw("169.254.169.254:80"),
		"link-local v6":     gw("[fe80::1]:80"),
		"multicast":         gw("224.0.0.251:5353"),
		"unspecified":       gw("0.0.0.0:8080"),
		"호스트명 address":      gw("gateway.lan:8080"),
		"localhost address": gw("localhost:8080"),
		"port 없음":           gw("100.64.0.10"),
		"port 0":            gw("100.64.0.10:0"),
		"port 범위 밖":         gw("100.64.0.10:65536"),
		"4in6":              gw("[::ffff:100.64.0.10]:8080"),
		"빈 address":         gw(""),
		"IP 리터럴 domain":     {{Domain: "100.64.0.10", Address: "100.64.0.10:8080"}},
		"IPv6 리터럴 domain":   {{Domain: "[fd00::1]", Address: "[fd00::1]:8080"}},
		"wildcard domain":   {{Domain: "*.nip.io", Address: "100.64.0.10:8080"}},
		"빈 domain":          {{Domain: "", Address: "100.64.0.10:8080"}},
		"비ASCII domain":     {{Domain: "게이트웨이.example", Address: "100.64.0.10:8080"}},
		"중복":                {{Domain: "gw.example", Address: "10.0.0.1:1"}, {Domain: "gw.example", Address: "10.0.0.2:2"}},
		"정규화 후 중복":          {{Domain: "gw.example", Address: "10.0.0.1:1"}, {Domain: "GW.Example.", Address: "10.0.0.2:2"}},
		"상한 초과 5개": {
			{Domain: "a.example", Address: "10.0.0.1:1"}, {Domain: "b.example", Address: "10.0.0.1:1"},
			{Domain: "c.example", Address: "10.0.0.1:1"}, {Domain: "d.example", Address: "10.0.0.1:1"},
			{Domain: "e.example", Address: "10.0.0.1:1"},
		},
	}
	for name, pins := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validWorldConfig()
			cfg.EgressPins = pins
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseWorldConfig(data)
			if err == nil || !strings.Contains(err.Error(), "world config: egress_pins") {
				t.Fatalf("불량 egress_pins %+v: error=%v", pins, err)
			}
			// 계약 §3.3: 실제 CLI 진입점도 잘못된 설정을 claim 전에 거부.
			f := newProductionFixture(t)
			dir := t.TempDir()
			requestPath, configPath := filepath.Join(dir, "request.json"), filepath.Join(dir, "world.json")
			request, err := json.Marshal(f.request)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(requestPath, request, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			err = runProductionCmd(requestPath, f.profilePath, nil, f.acceptRoot, configPath, "", "")
			if err == nil || !strings.Contains(err.Error(), "egress_pins") {
				t.Fatalf("CLI error=%v, want egress_pins", err)
			}
			assertKeyNotConsumed(t, f)
		})
	}
}

func TestParseWorldConfigRejectsUnknownEgressPinField(t *testing.T) {
	cfg := validWorldConfig()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	withPin := bytes.Replace(data, []byte(`"adapters"`),
		[]byte(`"egress_pins":[{"domain":"gw.example","address":"10.0.0.1:1","wildcard":true}],"adapters"`), 1)
	if _, err := parseWorldConfig(withPin); err == nil {
		t.Fatal("egress_pins 미지 필드 수용")
	}
}

// (c) 핀 domain ∉ 병합 정책 allowlist → claim 이전 거부, key 미소모, launch 0회.
func TestProductionPinOutsidePolicyRejectedBeforeClaim(t *testing.T) {
	f := newProductionFixture(t)
	cfg := validWorldConfig()
	cfg.EgressPins = []worldEgressPin{{Domain: "gateway.internal", Address: "100.64.0.10:8080"}}
	launcher := &worldLauncher{config: cfg}
	data, err := json.Marshal(f.request)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runProduction(context.Background(), productionRun{
		RequestBytes: data, ProfilePath: f.profilePath, AcceptRoot: f.acceptRoot,
		Launcher: launcher, Stdout: &out,
	})
	var rerr *runError
	if !errors.As(err, &rerr) || rerr.Code != codePolicyDenied || !strings.Contains(rerr.Error(), "gateway.internal") {
		t.Fatalf("allowlist 밖 pin은 claim 전 POLICY_DENIED여야 함: %v\n%s", err, out.String())
	}
	msgs := decodeControls(t, out.String())
	if len(msgs) != 1 || msgs[0].Status != "rejected" {
		t.Fatalf("제어 응답 = %+v", msgs)
	}
	assertKeyNotConsumed(t, f)
}

// 정책 allowlist(example.com) 안의 pin은 사전 검사를 통과한다.
func TestWorldLauncherPreClaimCheckAcceptsPinWithinPolicy(t *testing.T) {
	cfg := validWorldConfig()
	cfg.EgressPins = []worldEgressPin{{Domain: "gw.example.com", Address: "100.64.0.10:8080"}}
	launcher := &worldLauncher{config: cfg}
	var checker preClaimChecker = launcher
	if err := checker.CheckBeforeClaim(sandboxWithEgress("example.com")); err != nil {
		t.Fatalf("allowlist 안 pin 거부: %v", err)
	}
	if err := checker.CheckBeforeClaim(sandboxWithEgress("other.example")); err == nil {
		t.Fatal("allowlist 밖 pin 수용")
	}
	if err := (&worldLauncher{config: validWorldConfig()}).CheckBeforeClaim(sandboxWithEgress()); err != nil {
		t.Fatalf("pin 없는 설정은 항상 통과해야 함: %v", err)
	}
}
