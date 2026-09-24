package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestWorldConfigGatewayEnvironment(t *testing.T) {
	cfg := validWorldConfig()
	adapter := cfg.Adapters["claudecode"]
	adapter.Env = []string{"ANTHROPIC_BASE_URL=http://gateway.example", "ANTHROPIC_AUTH_TOKEN=gateway-access-sentinel"}
	cfg.Adapters["claudecode"] = adapter
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseWorldConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Adapters["claudecode"].Env, adapter.Env) {
		t.Fatal("gateway env changed")
	}
	// Old inject configs fail closed instead of silently losing authentication.
	data = []byte(strings.Replace(string(data), `"control_mode":`, `"inject":[],"control_mode":`, 1))
	if _, err := parseWorldConfig(data); err == nil {
		t.Fatal("obsolete inject accepted")
	}
}

func TestWorldConfigEnvironmentErrorsDoNotEchoValues(t *testing.T) {
	for _, env := range [][]string{
		{"bad-name=gateway-access-sentinel"}, {"gateway-access-sentinel"},
		{"ANTHROPIC_AUTH_TOKEN=gateway-access-sentinel\x00"},
		{"CLAUDE_CODE_OAUTH_TOKEN=subscription-sentinel"},
		{"ANTHROPIC_AUTH_TOKEN=gateway-access-sentinel", "ANTHROPIC_AUTH_TOKEN=another-key"},
		{"ANTHROPIC_AUTH_TOKEN=gateway-access-sentinel", "ANTHROPIC_API_KEY=another-key"},
	} {
		cfg := validWorldConfig()
		adapter := cfg.Adapters["claudecode"]
		adapter.Env = env
		cfg.Adapters["claudecode"] = adapter
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = parseWorldConfig(data)
		if err == nil {
			t.Fatal("invalid env accepted")
		}
		for _, value := range []string{"gateway-access-sentinel", "subscription-sentinel", "another-key"} {
			if strings.Contains(err.Error(), value) {
				t.Fatal("value leaked in config error")
			}
		}
	}
}
