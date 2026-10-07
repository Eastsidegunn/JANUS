package local

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/core/world/processwire"
)

// This is a command-construction guardian, not a real Podman/inspect claim.
func TestGatewayAccessKeyEnvironmentAndRedaction(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		t.Run(name, func(t *testing.T) {
			const key = "gateway-access-sentinel-23"
			const subscription = "subscription-sentinel-never-provided"
			digest := "sha256:" + strings.Repeat("a", 64)
			lower, root := testDirs(t)
			runner := newFakePodman(digest)
			backend := mustBackend(t, root, runner, statDevice)
			env := []string{"ANTHROPIC_BASE_URL=http://gateway.example", name + "=" + key}
			spec := testSpec(lower, digest).WithAgentEnv(env)
			prepared, err := backend.Prepare(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := json.Marshal(prepared.Metadata())
			if err != nil {
				t.Fatal(err)
			}
			if err := prepared.Abort(context.Background()); err != nil {
				t.Fatal(err)
			}
			active, err := openTestLease(t, backend, context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			defer active.Close(context.Background())
			calls := runner.envSnapshot()
			if len(calls) != 1 || !containsArgValue(calls[0].args, "--env", name) {
				t.Fatal("access key must use name-only agent env transfer")
			}
			if strings.Join(calls[0].env, "\n") != strings.Join(env, "\n") {
				t.Fatal("plaintext gateway env not preserved")
			}
			for _, call := range runner.snapshot() {
				text := strings.Join(call, " ")
				if strings.Contains(text, key) || strings.Contains(text, subscription) || strings.Contains(text, "--inject") || strings.Contains(text, "/run/hx-cred") {
					t.Fatal("credential or retired injection in argv")
				}
			}
			if strings.Contains(string(metadata), key) || strings.Contains(string(metadata), subscription) {
				t.Fatal("metadata leak")
			}
			if strings.Contains(strings.Join(calls[0].env, "\n"), subscription) {
				t.Fatal("subscription in env")
			}
			b := active.(*lease).process
			if string(b.redaction) != key {
				t.Fatal("access key not connected to stream redactor")
			}
			for _, stream := range []processwire.Stream{processwire.StreamStdout, processwire.StreamStderr} {
				output := append(b.redactChunk(stream, []byte("echo gateway-access-")), b.redactChunk(stream, []byte("sentinel-23 "+strings.Repeat(".", len(key))))...)
				if strings.Contains(string(output), key) || !strings.Contains(string(output), "<redacted>") {
					t.Fatal("split access key not redacted")
				}
			}
		})
	}
}

func TestSecretEnvironmentValuesIncludesBuiltInDeclaredAndHeuristicNames(t *testing.T) {
	env := []string{
		"OPENAI_API_KEY=openai-sentinel",
		"GATEWAY_ACCESS=declared-sentinel",
		"PLAIN_VALUE=ordinary",
		"SHORT_TOKEN=x",
	}
	got := SecretEnvironmentValues(env, []string{"GATEWAY_ACCESS"})
	for _, want := range []string{"openai-sentinel", "declared-sentinel", "x"} {
		found := false
		for _, value := range got {
			if value == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("secret value %q missing from %v", want, got)
		}
	}
	for _, value := range got {
		if value == "ordinary" {
			t.Fatal("non-secret env value classified")
		}
	}
}

func TestValidateSecretEnvironmentValuesRejectsShortGenericValues(t *testing.T) {
	cases := []struct {
		name     string
		env      []string
		declared []string
	}{
		{"short builtin", []string{"OPENAI_API_KEY=short"}, nil},
		{"numeric heuristic", []string{"SERVICE_TOKEN=12345678"}, nil},
		{"boolean declared", []string{"GATEWAY_ACCESS=true"}, []string{"GATEWAY_ACCESS"}},
		{"boolean case insensitive", []string{"GATEWAY_ACCESS=FALSE"}, []string{"GATEWAY_ACCESS"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSecretEnvironmentValues(tc.env, tc.declared)
			if err == nil || !strings.Contains(err.Error(), "secret 값이 너무 짧거나 일반값") {
				t.Fatalf("unexpected validation error: %v", err)
			}
			for _, item := range tc.env {
				_, value, _ := strings.Cut(item, "=")
				if strings.Contains(err.Error(), value) {
					t.Fatalf("secret value echoed: %v", err)
				}
			}
		})
	}
	if err := ValidateSecretEnvironmentValues([]string{"OPENAI_API_KEY=valid-sentinel-value"}, nil); err != nil {
		t.Fatalf("valid secret rejected: %v", err)
	}
}
