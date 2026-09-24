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
