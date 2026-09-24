package local

import (
	"fmt"
	"regexp"
	"strings"
)

var agentEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateAgentEnvironment rejects ambiguous auth and malformed entries without
// echoing values. T23 production config carries gateway access keys only.
func ValidateAgentEnvironment(env []string) error {
	seen := map[string]bool{}
	auth := ""
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !agentEnvNamePattern.MatchString(name) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("env 항목은 NUL 없는 NAME=VALUE 형식이어야 함")
		}
		if seen[name] {
			return fmt.Errorf("env 이름 중복")
		}
		seen[name] = true
		if name == "CLAUDE_CODE_OAUTH_TOKEN" {
			return fmt.Errorf("구독 OAuth는 외부 gateway 소유이며 world-config env에 허용하지 않음")
		}
		if name == "ANTHROPIC_AUTH_TOKEN" || name == "ANTHROPIC_API_KEY" {
			if value != "" && auth != "" && auth != value {
				return fmt.Errorf("gateway 인증 env에는 하나의 접근키만 허용")
			}
			if value != "" {
				auth = value
			}
		}
	}
	return nil
}

// gatewayAccessKey feeds the existing stream redactor; it does not generate or
// replace HTTP authorization. Both standard Claude gateway env names work.
func gatewayAccessKey(env []string) string {
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if (name == "ANTHROPIC_AUTH_TOKEN" || name == "ANTHROPIC_API_KEY") && value != "" {
			return value
		}
	}
	return ""
}
