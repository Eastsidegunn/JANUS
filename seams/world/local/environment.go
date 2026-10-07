package local

import (
	"fmt"
	"regexp"
	"strings"
)

var agentEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var allDigitsPattern = regexp.MustCompile(`^[0-9]+$`)

var builtInSecretEnvNames = map[string]struct{}{
	"ANTHROPIC_AUTH_TOKEN": {},
	"ANTHROPIC_API_KEY":    {},
}

// ValidateSecretEnvNames validates operator-declared names before a world claim.
func ValidateSecretEnvNames(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !agentEnvNamePattern.MatchString(name) {
			return fmt.Errorf("secret_env 이름은 NAME 형식이어야 함")
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("secret_env 이름 중복")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func isSecretEnvName(name string, declared map[string]struct{}) bool {
	if _, ok := builtInSecretEnvNames[name]; ok {
		return true
	}
	if _, ok := declared[name]; ok {
		return true
	}
	upper := strings.ToUpper(name)
	for _, suffix := range []string{"_KEY", "_TOKEN", "_SECRET", "_PASSWORD"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

// SecretEnvironmentValues returns exact non-empty values. Short heuristic
// values remain included to fail closed against credential leakage.
func SecretEnvironmentValues(env, declared []string) []string {
	declaredSet := make(map[string]struct{}, len(declared))
	for _, name := range declared {
		declaredSet[name] = struct{}{}
	}
	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" || !isSecretEnvName(name, declaredSet) {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

// ValidateSecretEnvironmentValues rejects values that are too short or too
// obviously generic before a world claim; the value itself is never echoed.
func ValidateSecretEnvironmentValues(env, declared []string) error {
	declaredSet := make(map[string]struct{}, len(declared))
	for _, name := range declared {
		declaredSet[name] = struct{}{}
	}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" || !isSecretEnvName(name, declaredSet) {
			continue
		}
		if len(value) < 8 || allDigitsPattern.MatchString(value) || strings.EqualFold(value, "true") || strings.EqualFold(value, "false") {
			return fmt.Errorf("secret 값이 너무 짧거나 일반값 — secret_env/이름 변경 또는 값 교체")
		}
	}
	return nil
}

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
