package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/seams/accept"
)

func validWorldConfig() worldConfig {
	return worldConfig{
		StateRoot:  "/var/lib/hx",
		ProxyImage: worldImageConfig{Repository: "registry.example/proxy", Digest: "sha256:" + strings.Repeat("a", 64), UID: 1000, GID: 1000},
		Adapters: map[string]worldAdapterConfig{
			"claudecode": {Bin: "/usr/bin/adapter", AgentArgv: []string{"agent"}, ControlMode: "tool_approval", Image: worldImageConfig{Repository: "registry.example/agent", Digest: "sha256:" + strings.Repeat("b", 64), UID: 1000, GID: 1000}},
		},
	}
}

func TestParseWorldConfigValidatesImageReferences(t *testing.T) {
	data, err := json.Marshal(validWorldConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseWorldConfig(data); err != nil {
		t.Fatalf("유효 설정 거부: %v", err)
	}
	// FR-SBX-01: proxy와 모든 agent에 동일한 이미지 규칙 적용.
	cases := []struct {
		name   string
		mutate func(*worldImageConfig)
	}{
		{"tag digest", func(c *worldImageConfig) { c.Digest = ":latest" }},
		{"empty digest", func(c *worldImageConfig) { c.Digest = "" }},
		{"malformed digest", func(c *worldImageConfig) { c.Digest = "sha256:bad" }},
		{"uid zero", func(c *worldImageConfig) { c.UID = 0 }},
		{"gid zero", func(c *worldImageConfig) { c.GID = 0 }},
		{"empty repository", func(c *worldImageConfig) { c.Repository = "" }},
		{"tagged repository", func(c *worldImageConfig) { c.Repository += ":latest" }},
	}
	for _, target := range []string{"proxy", "claudecode", "codex"} {
		for _, tc := range cases {
			t.Run(target+"/"+tc.name, func(t *testing.T) {
				cfg := validWorldConfig()
				cfg.Adapters["codex"] = cfg.Adapters["claudecode"]
				want := "proxy image"
				if target == "proxy" {
					tc.mutate(&cfg.ProxyImage)
				} else {
					adapter := cfg.Adapters[target]
					tc.mutate(&adapter.Image)
					cfg.Adapters[target] = adapter
					want = "어댑터 \"" + target + "\" image"
				}
				data, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				_, err = parseWorldConfig(data)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error=%v, want context %q", err, want)
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
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("CLI error=%v, want %q", err, want)
				}
				registry, err := accept.Open(f.acceptRoot)
				if err != nil {
					t.Fatal(err)
				}
				status, err := registry.Lookup(f.request.Scope, f.request.IdempotencyKey)
				if err != nil {
					t.Fatal(err)
				}
				if status.State != "not_submitted" {
					t.Fatalf("잘못된 설정이 key를 소모함: %+v", status)
				}
			})
		}
	}
}

func TestParseWorldConfigSecretEnvAndRedactionValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*worldConfig)
		want   string
	}{
		{"bad secret env name", func(cfg *worldConfig) {
			a := cfg.Adapters["claudecode"]
			a.SecretEnv = []string{"bad-name"}
			cfg.Adapters["claudecode"] = a
		}, "secret_env"},
		{"duplicate secret env name", func(cfg *worldConfig) {
			a := cfg.Adapters["claudecode"]
			a.SecretEnv = []string{"GATEWAY_ACCESS", "GATEWAY_ACCESS"}
			cfg.Adapters["claudecode"] = a
		}, "중복"},
		{"short secret value", func(cfg *worldConfig) {
			a := cfg.Adapters["claudecode"]
			a.Env = []string{"OPENAI_API_KEY=short"}
			cfg.Adapters["claudecode"] = a
		}, "secret 값이 너무 짧거나 일반값"},
		{"numeric secret value", func(cfg *worldConfig) {
			a := cfg.Adapters["claudecode"]
			a.Env = []string{"SERVICE_TOKEN=12345678"}
			cfg.Adapters["claudecode"] = a
		}, "secret 값이 너무 짧거나 일반값"},
		{"boolean secret value", func(cfg *worldConfig) {
			a := cfg.Adapters["claudecode"]
			a.SecretEnv = []string{"GATEWAY_ACCESS"}
			a.Env = []string{"GATEWAY_ACCESS=false"}
			cfg.Adapters["claudecode"] = a
		}, "secret 값이 너무 짧거나 일반값"},
		{"bad redaction regex", func(cfg *worldConfig) {
			cfg.RedactionPatterns = []string{"[unterminated"}
		}, "redaction_patterns"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validWorldConfig()
			tc.mutate(&cfg)
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseWorldConfig(data); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse error=%v, want %q", err, tc.want)
			}
			fixture := newProductionFixture(t)
			dir := t.TempDir()
			requestPath, configPath := filepath.Join(dir, "request.json"), filepath.Join(dir, "world.json")
			request, err := json.Marshal(fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(requestPath, request, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := runProductionCmd(requestPath, fixture.profilePath, nil, fixture.acceptRoot, configPath, "", ""); err == nil {
				t.Fatal("invalid config accepted by production command")
			}
			registry, err := accept.Open(fixture.acceptRoot)
			if err != nil {
				t.Fatal(err)
			}
			status, err := registry.Lookup(fixture.request.Scope, fixture.request.IdempotencyKey)
			if err != nil {
				t.Fatal(err)
			}
			if status.State != "not_submitted" {
				t.Fatalf("invalid config consumed claim: %+v", status)
			}
		})
	}
}
