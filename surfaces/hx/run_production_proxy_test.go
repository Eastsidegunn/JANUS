package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseWorldConfigAcceptsEnvAndInject fixes that plaintext env and proxy
// injection rules (names only) are valid world-config and convert to the
// host-only capability input intact.
func TestParseWorldConfigAcceptsEnvAndInject(t *testing.T) {
	cfg := validWorldConfig()
	adapter := cfg.Adapters["claudecode"]
	adapter.Env = []string{"ANTHROPIC_BASE_URL=http://api.anthropic.com"}
	adapter.Inject = []worldInjectRule{{
		Domain: "api.anthropic.com", Header: "Authorization", ValuePrefix: "Bearer ", Credential: "CLAUDE_CODE_OAUTH_TOKEN",
	}}
	cfg.Adapters["claudecode"] = adapter

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseWorldConfig(data)
	if err != nil {
		t.Fatalf("env/inject를 포함한 유효 설정을 거부함: %v", err)
	}
	rule := parsed.Adapters["claudecode"].Inject[0].toProxyInjection()
	if err := rule.Validate(); err != nil {
		t.Fatalf("변환된 injection 규칙이 유효하지 않음: %v", err)
	}
	if rule.Domain != "api.anthropic.com" || rule.Header != "Authorization" ||
		rule.ValuePrefix != "Bearer " || rule.CredentialName != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("injection 규칙 변환 손실: %+v", rule)
	}
}

// TestParseWorldConfigRejectsMalformedEnvAndInject fails closed on bad shapes.
func TestParseWorldConfigRejectsMalformedEnvAndInject(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*worldAdapterConfig)
		want   string
	}{
		{"env without =", func(a *worldAdapterConfig) { a.Env = []string{"ANTHROPIC_BASE_URL"} }, "env 항목"},
		{"env bad name", func(a *worldAdapterConfig) { a.Env = []string{"1BAD=x"} }, "env 항목"},
		{"inject empty header", func(a *worldAdapterConfig) {
			a.Inject = []worldInjectRule{{Domain: "api.anthropic.com", Header: "", Credential: "TOKEN"}}
		}, "inject"},
		{"inject bad credential name", func(a *worldAdapterConfig) {
			a.Inject = []worldInjectRule{{Domain: "api.anthropic.com", Header: "Authorization", Credential: "lower"}}
		}, "inject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validWorldConfig()
			adapter := cfg.Adapters["claudecode"]
			tc.mutate(&adapter)
			cfg.Adapters["claudecode"] = adapter
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseWorldConfig(data); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want context %q", err, tc.want)
			}
		})
	}
}

// TestTwoVendorWorldConfigsParse is 완료 기준 ③ at the config layer: two vendors
// of the same model differ only in adapter env and injection rule, and both
// produce valid configs with the expected proxy-injection rules.
func TestTwoVendorWorldConfigsParse(t *testing.T) {
	vendors := []struct {
		baseURL, domain, header, prefix, credential string
	}{
		{"http://api.anthropic.com", "api.anthropic.com", "Authorization", "Bearer ", "CLAUDE_CODE_OAUTH_TOKEN"},
		{"http://api.vendor2.example", "api.vendor2.example", "x-api-key", "", "VENDOR2_API_KEY"},
	}
	for _, v := range vendors {
		cfg := validWorldConfig()
		adapter := cfg.Adapters["claudecode"]
		adapter.Env = []string{"ANTHROPIC_BASE_URL=" + v.baseURL}
		adapter.Inject = []worldInjectRule{{Domain: v.domain, Header: v.header, ValuePrefix: v.prefix, Credential: v.credential}}
		cfg.Adapters["claudecode"] = adapter
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parseWorldConfig(data)
		if err != nil {
			t.Fatalf("vendor %s 설정을 거부함: %v", v.domain, err)
		}
		rule := parsed.Adapters["claudecode"].Inject[0].toProxyInjection()
		if rule.CredentialName != v.credential || rule.Header != v.header || rule.Domain != v.domain {
			t.Fatalf("vendor %s 규칙 불일치: %+v", v.domain, rule)
		}
	}
}
