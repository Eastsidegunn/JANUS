//go:build t15integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
)

const t15ClaudeBase = "docker.io/library/node@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5"

// TestClaudeWorldIntegration is the credential-free half of the T15 gate. It
// uses the real pinned Claude image and the production world assembly, but no
// OAuth/API credential. The existing TestWorldIntegration covers the other
// five boundary assertions with the real container test agent.
func TestClaudeWorldIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatalf("VERIFICATION: T15 Linux gate requires Linux; skip 금지 (현재 %s)", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	requirePodmanPreconditions(t, ctx)
	artifacts := buildIntegrationArtifacts(t, ctx)
	claudeRepo, claudeDigest := buildClaudeImage(t, ctx, artifacts.root)
	claudeAdapter := buildT15ClaudeAdapter(t, ctx, artifacts.root)

	lower, stateRoot := integrationPaths(t)
	store := newIntegrationStore(t, filepath.Join(t.TempDir(), "events.ndjson"), false)
	writer, err := logd.NewWriter(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	traceID, parentSpan, childSpan := logd.NewTraceID(), logd.NewSpanID(), logd.NewSpanID()
	if err := writer.InitBatch(ctx, []gen.EventRecord{{
		Ts: time.Now().UnixMilli(), TraceID: traceID, SpanID: parentSpan,
		Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}
	backend := newIntegrationBackend(t, stateRoot, artifacts)
	adapterHashBefore := fileSHA256(t, claudeAdapter)
	budget := gen.Budget{Tokens: 100_000, TimeMs: 180_000, MaxDepth: 2}
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "t15-claude-auth", Workspace: lower, FSScope: []string{lower},
		Egress: []string{"example.com"}, Budget: budget, Approval: policy.ApprovalManual,
	})
	spawnSpec := world.NewSpawnSpec(effective, world.NewImageReference(claudeRepo, claudeDigest), []string{"claude"}, 0, traceID, childSpan, world.AgentIdentity{UID: 1000, GID: 1000}, nil)
	var adapterStderr bytes.Buffer
	active, err := startProductionWorld(ctx, worldLaunch{
		Backend: backend, SpawnSpec: spawnSpec, Writer: writer, TraceID: traceID, ParentSpan: parentSpan,
		AdapterCommand: []string{claudeAdapter}, AdapterName: "claudecode", AdapterStderr: &adapterStderr,
		Instruction: "Respond with exactly OK.", Workspace: "/workspace", Budget: budget, Depth: 0,
		ProfileID: "t15-claude-auth", Approval: subagent.Spec{Approval: policy.ApprovalManual, Decider: policy.DenyAll{}},
		// Keep the host adapter environment minimal. In particular, a runner
		// credential can never accidentally become a container credential.
		AdapterBaseEnv: []string{"PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalized := false
	defer func() {
		if !finalized {
			_ = active.Lease.Close(context.Background())
		}
	}()
	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Minute)
	done, waitErr := active.Subagent.Wait(waitCtx)
	waitCancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	closeErr := active.FinalizeCollection(closeCtx)
	closeCancel()
	finalized = true
	if waitErr != nil {
		t.Logf("tokenless Claude diagnostic stderr=%q records=%d", adapterStderr.String(), len(store.snapshot()))
		t.Fatalf("VERIFICATION: tokenless Claude did not produce terminal done/error: %v", waitErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if got := fileSHA256(t, claudeAdapter); got != adapterHashBefore {
		t.Fatalf("VERIFICATION: host Claude adapter binary was modified: before=%s after=%s", adapterHashBefore, got)
	}
	if done.Status != gen.DonePayloadStatusError {
		t.Fatalf("VERIFICATION: tokenless Claude status=%s, want error (result=%q)", done.Status, done.Result)
	}
	result := strings.ToLower(done.Result)
	if !strings.Contains(result, "auth") && !strings.Contains(result, "login") &&
		!strings.Contains(result, "logged") && !strings.Contains(result, "credential") &&
		!strings.Contains(result, "token") {
		t.Fatalf("VERIFICATION: done error does not identify authentication failure: %q", done.Result)
	}
	recordBytes, err := json.Marshal(store.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "Bearer ", "oauth-token"} {
		if strings.Contains(string(recordBytes), forbidden) {
			t.Fatalf("VERIFICATION: credential material/name leaked into durable records: %q", forbidden)
		}
		if strings.Contains(adapterStderr.String(), forbidden) {
			t.Fatalf("VERIFICATION: credential material/name leaked into adapter stderr: %q", forbidden)
		}
	}
	assertNoRuntimeArtifacts(t, ctx, claudeRepo+"@"+claudeDigest, childSpan)
}

func buildT15ClaudeAdapter(t *testing.T, ctx context.Context, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claudecode")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", path, "./seams/subagent/claudecode/cmd/claudecode")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("VERIFICATION: claudecode adapter build failed: %v\n%s", err, out)
	}
	return path
}

func buildClaudeImage(t *testing.T, ctx context.Context, root string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "podman", "pull", "--quiet", t15ClaudeBase).CombinedOutput(); err != nil {
		t.Fatalf("INFRASTRUCTURE: pinned Node base pull failed: %s: %v\n%s", t15ClaudeBase, err, out)
	}
	hook := filepath.Join(dir, "hxapprove")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", hook, "./seams/subagent/claudecode/hxapprove")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("VERIFICATION: hxapprove build failed: %v\n%s", err, out)
	}
	imageDir := filepath.Join(dir, "image")
	if err := os.Mkdir(imageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(hook, filepath.Join(imageDir, "hxapprove")); err != nil {
		t.Fatal(err)
	}
	containerfile := fmt.Sprintf("FROM %s\nRUN npm install --global @anthropic-ai/claude-code@2.1.252 && npm cache clean --force\nCOPY --chown=1000:1000 hxapprove /usr/local/bin/hxapprove\nUSER 1000:1000\nWORKDIR /workspace\n", t15ClaudeBase)
	if err := os.WriteFile(filepath.Join(imageDir, "Containerfile"), []byte(containerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	repository := "localhost/hx-claude-t15"
	tag := repository + ":integration"
	if out, err := exec.CommandContext(ctx, "podman", "build", "--pull=never", "-t", tag, imageDir).CombinedOutput(); err != nil {
		t.Fatalf("INFRASTRUCTURE: pinned Claude image build failed: %v\n%s", err, out)
	}
	digestBytes, err := exec.CommandContext(ctx, "podman", "image", "inspect", "--format", "{{.Digest}}", tag).CombinedOutput()
	if err != nil {
		t.Fatalf("VERIFICATION: Claude image digest inspect failed: %v\n%s", err, digestBytes)
	}
	digest := strings.TrimSpace(string(digestBytes))
	if len(digest) != 71 || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("VERIFICATION: invalid Claude image digest %q", digest)
	}
	if out, err := exec.CommandContext(ctx, "podman", "image", "inspect", repository+"@"+digest).CombinedOutput(); err != nil {
		t.Fatalf("VERIFICATION: repository@digest lookup failed: %v\n%s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "podman", "run", "--rm", "--pull=never", repository+"@"+digest, "claude", "--version").CombinedOutput(); err != nil {
		t.Fatalf("VERIFICATION: pinned Claude version probe failed: %v\n%s", err, out)
	} else if !strings.Contains(string(out), "2.1.252") {
		t.Fatalf("VERIFICATION: Claude version mismatch: %s", out)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanup, "podman", "image", "rm", "--force", repository+"@"+digest).Run()
		_ = exec.CommandContext(cleanup, "podman", "image", "rm", "--force", tag).Run()
	})
	return repository, digest
}
