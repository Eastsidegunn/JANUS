//go:build t27integration

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
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
	"github.com/Eastsidegunn/JANUS/seams/subagent/claudecode"
)

// T27(b) uses the recorded T8 approval-denied fixture: one native Write
// tool_use, then the permission_denied/tool_result that a denied hook yields.
const (
	t27Fixture      = "contracts/fixtures/claude-code/05-approval-denied.ndjson"
	t27ToolCallID   = "toolu_015RSsY1h8UtCcm35pts1TAc"
	t27ToolName     = "Write"
	t27DenyReason   = "기본 거부 정책" // policy.DenyAll — proves the parent Decider, not a forced relay deny
	t27ApprovalWait = 10 * time.Second
)

// TestContainerToolUseApprovalIntegration is the T27(b) gate: a fake Claude
// inside a real rootless Podman container emits a native tool_use and runs the
// image's hxapprove against the mounted relay socket; the host claudecode
// adapter drives it through the ProcessEndpoint and the parent decides with
// DenyAll. Both hook orders are exercised because real claude-code's order is
// not pinned:
//
//   - native-first: tool_use line, then the blocking hook.
//   - hook-first:   the blocking hook, and the tool_use line only after it
//     returns. Background: before T27 (e), the world broker
//     parked such a hook in `waiting` until the adapter registered the tool
//     intent, which it only did after seeing that tool_use line — a circular
//     wait. The current design delivers the hook immediately and correlates
//     the native intent after the fact (mismatch is broker-fatal). This
//     subtest guards against that regression: it asserts the approval request
//     is recorded within t27ApprovalWait of ready; when it is not, it fails
//     labelled DEADLOCK only for the exact circular-wait signature and
//     "approval_request missing (not deadlock)" otherwise.
//
// Token-free: no claude-code, credential, or model is involved.
func TestContainerToolUseApprovalIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatalf("VERIFICATION: T27 Linux gate requires Linux; skip 금지 (현재 %s)", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	requirePodmanPreconditions(t, ctx)
	artifacts := buildIntegrationArtifacts(t, ctx)
	dir := t.TempDir()
	adapter := buildT27Binary(t, ctx, artifacts.root, dir, "claudecode", "./seams/subagent/claudecode/cmd/claudecode")
	fake := buildT27Binary(t, ctx, artifacts.root, dir, "fakeclaude", "./seams/subagent/claudecode/testdata/fakeclaude")
	hook := buildT27Binary(t, ctx, artifacts.root, dir, "hxapprove", "./seams/subagent/claudecode/hxapprove")
	fixture := filepath.Join(artifacts.root, t27Fixture)
	for _, order := range []string{"native-first", "hook-first"} {
		t.Run(order, func(t *testing.T) {
			repository, digest := buildT27FakeClaudeImage(t, ctx, dir, order, fake, hook, fixture)
			runT27ToolUseGate(t, ctx, artifacts, adapter, repository, digest, order)
		})
	}
}

func runT27ToolUseGate(t *testing.T, parent context.Context, artifacts integrationArtifacts, adapter, repository, digest, order string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()
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
	adapterHashBefore := fileSHA256(t, adapter)
	// The approval deadline is min(budget, the bounded host approval wait);
	// 180s keeps it far beyond
	// t27ApprovalWait, so a missing approval_request cannot be a deadline deny.
	budget := gen.Budget{Tokens: 100_000, TimeMs: 180_000, MaxDepth: 2}
	profile := "t27-tool-use-" + order
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: profile, Workspace: lower, FSScope: []string{lower},
		Egress: []string{"example.com"}, Budget: budget, Approval: policy.ApprovalManual,
	})
	instruction := "T27 container tool_use gate"
	image := repository + "@" + digest
	spawnSpec := world.NewSpawnSpec(effective, world.NewImageReference(repository, digest), claudecode.ContainerArgv("claude", instruction), 0, traceID, childSpan, world.AgentIdentity{UID: 1000, GID: 1000}, nil)
	adapterStderr := &t27Buffer{}
	active, err := startProductionWorld(ctx, worldLaunch{
		Backend: backend, SpawnSpec: spawnSpec, Writer: writer, TraceID: traceID, ParentSpan: parentSpan,
		AdapterCommand: []string{adapter}, AdapterName: "claudecode", ControlMode: gen.SubagentSpawnPayloadControlModeToolApproval, AdapterStderr: adapterStderr,
		Instruction: instruction, Workspace: "/workspace", Budget: budget, Depth: 0,
		ProfileID: profile, Approval: subagent.Spec{Approval: policy.ApprovalManual, Decider: policy.DenyAll{}},
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
	approvalAddress := active.Lease.ApprovalEndpoint().Address()
	processAddress := active.Lease.ProcessEndpoint().Address()

	waitRecord(t, store, gen.KindSubagentReady, 60*time.Second)
	if !waitRecordWithin(store, gen.KindSubagentApprovalRequest, t27ApprovalWait) {
		evidence, deadlocked := t27DeadlockEvidence(ctx, store, image)
		stopErr := active.Subagent.Stop(gen.StopPayloadReasonUser)
		waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
		done, waitErr := active.Subagent.Wait(waitCtx)
		waitCancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		closeErr := active.FinalizeCollection(closeCtx)
		closeCancel()
		finalized = true
		// The DEADLOCK label is reserved for the exact circular-wait signature
		// (hook alive and blocked, no tool_call, no done); anything else is a
		// different failure and must not be reported as the known deadlock.
		label := "approval_request missing (not deadlock)"
		explanation := "the stall does not match the circular-wait signature (hxapprove alive && no tool_call && no done)."
		if deadlocked {
			label = "DEADLOCK reproduced"
			explanation = "the hook waits in the broker for an intent the adapter only registers after the native tool_use line, " +
				"which the agent withholds until the hook returns."
		}
		t.Fatalf("VERIFICATION: %s (order=%s): no subagent/approval_request within %s of ready — %s\nevidence: %s\n"+
			// close carries the approval broker's Err() (lease Close joins its
			// Shutdown error); broker Warnings() are not reachable through the
			// lease and are printed by the broker to this test's stderr.
			"cleanup: stop=%v done=%+v wait=%v close(broker Err)=%v residue=%s\nadapter stderr (incl. approval client failure chain, agent stderr frames)=%q",
			label, order, t27ApprovalWait, explanation, evidence, stopErr, done, waitErr, closeErr, t27RuntimeResidue(ctx, image, childSpan), adapterStderr.String())
	}

	// fakeclaude holds after its last native line (result → usage) until
	// SIGUSR1, so the live agent container can be inspected from the host.
	waitRecord(t, store, gen.KindSubagentUsage, 60*time.Second)
	agentCID := findAgentContainer(t, ctx, image)
	assertContainerIsolation(t, ctx, agentCID, adapter, active.Lease)
	assertT27RelayMount(t, ctx, agentCID)
	runCommand(t, ctx, "podman", "kill", "--signal", "USR1", agentCID)

	waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
	done, waitErr := active.Subagent.Wait(waitCtx)
	waitCancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	closeErr := active.FinalizeCollection(closeCtx)
	closeCancel()
	finalized = true
	if waitErr != nil {
		t.Fatalf("VERIFICATION: subagent did not reach done (order=%s): %v\nadapter stderr=%q", order, waitErr, adapterStderr.String())
	}
	if closeErr != nil {
		t.Fatalf("VERIFICATION: lease close/broker reported failure (order=%s): %v", order, closeErr)
	}
	if done.Status != gen.DonePayloadStatusOk {
		// fakeclaude exits 3 when hxapprove fails or the decision it received
		// is not "deny"; that surfaces here as done{error}.
		t.Fatalf("VERIFICATION: done status=%s result=%q (order=%s)\nadapter stderr=%q", done.Status, done.Result, order, adapterStderr.String())
	}
	assertNoRuntimeArtifacts(t, ctx, image, childSpan)
	for _, path := range []string{approvalAddress, filepath.Dir(approvalAddress), processAddress} {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("VERIFICATION: lease 종료 뒤 socket/dir 잔존: %s (err=%v)", path, err)
		}
	}
	if got := fileSHA256(t, adapter); got != adapterHashBefore {
		t.Fatalf("VERIFICATION: host claudecode adapter binary was modified: before=%s after=%s", adapterHashBefore, got)
	}
	// "approval relay fatal" is the broker's directDeny reason sent to the hook,
	// not adapter stderr text: kept as a harmless extra guard, it has no effect
	// on its own. The other two are the adapter's broker/intent error texts.
	for _, fatal := range []string{"world approval broker", "approval relay fatal", "intent 등록"} {
		if strings.Contains(adapterStderr.String(), fatal) {
			t.Fatalf("VERIFICATION: adapter/broker fatal in stderr (%q): %q", fatal, adapterStderr.String())
		}
	}
	assertT27ApprovalChain(t, store.snapshot(), childSpan, order)
}

// assertT27ApprovalChain checks the durable chain. The relative order of
// tool_call and approval_request is asserted only for hook-first: in
// native-first the adapter emits tool_call after its intent registration
// returns while its approval poller may already emit approval_request, so
// that pair is not ordered by contract.
func assertT27ApprovalChain(t *testing.T, records []gen.EventRecord, childSpan, order string) {
	t.Helper()
	var toolCall, request, decision, toolResult, done *gen.EventRecord
	count := map[gen.Kind]int{}
	var requestPayload gen.SubagentApprovalRequestPayload
	var decisionPayload gen.PolicyDecisionPayload
	var lastChild int64
	for i := range records {
		record := &records[i]
		if record.SpanID != childSpan {
			continue
		}
		if strings.HasPrefix(string(record.Kind), "subagent/") {
			lastChild = record.Seq
		}
		count[record.Kind]++
		switch record.Kind {
		case gen.KindSubagentToolCall:
			var payload gen.SubagentToolCallPayload
			if err := json.Unmarshal(record.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.CallID != t27ToolCallID || payload.Name != t27ToolName {
				t.Fatalf("VERIFICATION: unexpected tool_call %+v", payload)
			}
			toolCall = record
		case gen.KindSubagentApprovalRequest:
			if err := json.Unmarshal(record.Payload, &requestPayload); err != nil {
				t.Fatal(err)
			}
			if record.Raw == nil || *record.Raw == "" {
				t.Fatal("VERIFICATION: approval_request lost the exact hook raw")
			}
			request = record
		case gen.KindPolicyDecision:
			if err := json.Unmarshal(record.Payload, &decisionPayload); err != nil {
				t.Fatal(err)
			}
			decision = record
		case gen.KindSubagentToolResult:
			toolResult = record
		case gen.KindSubagentDone:
			done = record
		}
	}
	for _, kind := range []gen.Kind{gen.KindSubagentToolCall, gen.KindSubagentApprovalRequest, gen.KindPolicyDecision, gen.KindSubagentToolResult, gen.KindSubagentDone} {
		if count[kind] != 1 {
			t.Fatalf("VERIFICATION: %s count=%d, want 1 (order=%s) counts=%v", kind, count[kind], order, count)
		}
	}
	// Reason == nil means the request is not a forced relay deny, i.e. the
	// parent Decider decides it (every forced deny carries a reason). The
	// hook-to-native-intent correlation is proven separately by the canonical
	// args equality below.
	if requestPayload.CallID != t27ToolCallID || requestPayload.Name != t27ToolName || requestPayload.Reason != nil {
		t.Fatalf("VERIFICATION: approval_request not an exactly correlated hook: %+v", requestPayload)
	}
	if decisionPayload.Decision != gen.PolicyDecisionPayloadDecisionDeny || decisionPayload.RequestID == nil ||
		*decisionPayload.RequestID != requestPayload.RequestID || decisionPayload.Reason == nil || *decisionPayload.Reason != t27DenyReason ||
		decisionPayload.DecisionSource == nil || *decisionPayload.DecisionSource != gen.PolicyDecisionPayloadDecisionSourceLocal {
		t.Fatalf("VERIFICATION: policy/decision is not the DenyAll deny for this request: %+v", decisionPayload)
	}
	var result gen.SubagentToolResultPayload
	if err := json.Unmarshal(toolResult.Payload, &result); err != nil || result.CallID != t27ToolCallID || result.Status != gen.SubagentToolResultPayloadStatusRejected {
		t.Fatalf("VERIFICATION: tool_result is not the rejected native result: %+v err=%v", result, err)
	}
	if !(request.Seq < decision.Seq && decision.Seq < toolResult.Seq && toolCall.Seq < toolResult.Seq && toolResult.Seq < done.Seq) {
		t.Fatalf("VERIFICATION: approval chain out of order: tool_call=%d request=%d decision=%d tool_result=%d done=%d",
			toolCall.Seq, request.Seq, decision.Seq, toolResult.Seq, done.Seq)
	}
	// Correlation proof: the hook's args (approval_request) and the native
	// tool_use args (tool_call) are the same canonical object. In hook-first
	// the tool_call is recorded only after the decision, so this is the
	// after-the-fact correlation that the T27(e) broker repair performs.
	var callPayload gen.SubagentToolCallPayload
	if err := json.Unmarshal(toolCall.Payload, &callPayload); err != nil {
		t.Fatal(err)
	}
	requestArgs, requestErr := policy.CanonicalArgs(requestPayload.Args)
	callArgs, callErr := policy.CanonicalArgs(callPayload.Args)
	if requestErr != nil || callErr != nil || !bytes.Equal(requestArgs, callArgs) {
		t.Fatalf("VERIFICATION: approval_request.args and tool_call.args differ canonically (order=%s): request=%s call=%s errs=%v/%v",
			order, requestPayload.Args, callPayload.Args, requestErr, callErr)
	}
	if order == "hook-first" && !(decision.Seq < toolCall.Seq) {
		t.Fatalf("VERIFICATION: hook-first tool_call(%d) recorded before the decision(%d)", toolCall.Seq, decision.Seq)
	}
	if done.Seq != lastChild {
		t.Fatalf("VERIFICATION: done(%d) is not the last child subagent event(%d)", done.Seq, lastChild)
	}
}

func assertT27RelayMount(t *testing.T, ctx context.Context, cid string) {
	t.Helper()
	var mounts []struct {
		Source, Destination string
		RW                  bool
	}
	if err := json.Unmarshal(runOutput(t, ctx, "podman", "inspect", "--format", "{{json .Mounts}}", cid), &mounts); err != nil {
		t.Fatal(err)
	}
	for _, mount := range mounts {
		if mount.Destination == "/run/hx" {
			if mount.RW {
				t.Fatalf("VERIFICATION: approval relay mount is writable: %+v", mount)
			}
			return
		}
	}
	t.Fatalf("VERIFICATION: approval relay (/run/hx) not mounted into the agent: %+v", mounts)
}

func waitRecordWithin(store *integrationStore, kind gen.Kind, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		for _, record := range store.snapshot() {
			if record.Kind == kind {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// t27DeadlockEvidence captures, while the run is still stalled, the
// circular-wait signature: hxapprove still alive in the agent container
// (blocked on the relay) rather than failed, and neither tool_call nor done
// recorded. The boolean is true only when that whole signature holds.
func t27DeadlockEvidence(ctx context.Context, store *integrationStore, image string) (string, bool) {
	var kinds []string
	toolCall, done := false, false
	for _, record := range store.snapshot() {
		kinds = append(kinds, string(record.Kind))
		toolCall = toolCall || record.Kind == gen.KindSubagentToolCall
		done = done || record.Kind == gen.KindSubagentDone
	}
	out := fmt.Sprintf("kinds=%v tool_call=%v done=%v", kinds, toolCall, done)
	ids, err := exec.CommandContext(ctx, "podman", "ps", "--filter", "ancestor="+image, "--format", "{{.ID}}").CombinedOutput()
	out += fmt.Sprintf(" running=%q(err=%v)", strings.TrimSpace(string(ids)), err)
	alive := false
	if fields := strings.Fields(string(ids)); err == nil && len(fields) == 1 {
		top, topErr := exec.CommandContext(ctx, "podman", "top", fields[0], "pid", "comm").CombinedOutput()
		alive = topErr == nil && strings.Contains(string(top), "hxapprove")
		out += fmt.Sprintf(" top=%q(err=%v)", strings.TrimSpace(string(top)), topErr)
		// Container state as Podman sees it: "running" while the adapter has
		// already observed process exit (done/Done) separates a premature
		// exit observation from a real agent exit.
		state, stateErr := exec.CommandContext(ctx, "podman", "inspect", "--format", "{{.State.Status}} exit={{.State.ExitCode}} started={{.State.StartedAt}}", fields[0]).CombinedOutput()
		out += fmt.Sprintf(" state=%q(err=%v)", strings.TrimSpace(string(state)), stateErr)
		// fakeclaude stderr (hook exit/decision trace or its exit-3 cause).
		logs, logsErr := exec.CommandContext(ctx, "podman", "logs", fields[0]).CombinedOutput()
		out += fmt.Sprintf(" agent_stderr=%q(err=%v)", t27StderrLines(logs), logsErr)
	}
	out += fmt.Sprintf(" hxapprove_alive=%v", alive)
	for _, record := range store.snapshot() {
		if record.Kind == gen.KindSubagentDone {
			out += fmt.Sprintf(" done_payload=%s", record.Payload)
		}
	}
	return out, alive && !toolCall && !done
}

// t27StderrLines keeps only the agent-side diagnostic lines from `podman
// logs` (which interleaves the large native stdout stream), bounded to 4 KiB.
func t27StderrLines(logs []byte) string {
	var kept []string
	for _, line := range strings.Split(string(logs), "\n") {
		if strings.HasPrefix(line, "fakeclaude:") || strings.HasPrefix(line, "hxapprove:") {
			kept = append(kept, line)
		}
	}
	joined := strings.Join(kept, "\n")
	if len(joined) > 4096 {
		joined = joined[len(joined)-4096:]
	}
	return joined
}

func t27RuntimeResidue(ctx context.Context, image, spanID string) string {
	ids, err := exec.CommandContext(ctx, "podman", "ps", "-a", "--filter", "ancestor="+image, "--format", "{{.ID}}").CombinedOutput()
	out := fmt.Sprintf("containers=%q(err=%v)", strings.TrimSpace(string(ids)), err)
	for _, network := range []string{"hx-" + spanID + "-internal", "hx-" + spanID + "-egress"} {
		if exec.CommandContext(ctx, "podman", "network", "exists", network).Run() == nil {
			out += " network=" + network
		}
	}
	return out
}

func buildT27Binary(t *testing.T, ctx context.Context, root, dir, name, pkg string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", path, pkg)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("VERIFICATION: %s build failed: %v\n%s", pkg, err, out)
	}
	return path
}

// buildT27FakeClaudeImage reuses the T10 FROM-scratch pattern: fakeclaude as
// /usr/local/bin/claude (ContainerArgv's argv[0]), hxapprove beside it, and
// the read-only fixture copy. The hook order is fixed per image through ENV,
// so the agent environment needs no world-config env.
func buildT27FakeClaudeImage(t *testing.T, ctx context.Context, dir, order, fake, hook, fixture string) (string, string) {
	t.Helper()
	contextDir := filepath.Join(dir, "image-"+order)
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		src, name string
		mode      os.FileMode
	}{{fake, "claude", 0o755}, {hook, "hxapprove", 0o755}, {fixture, "fixture.ndjson", 0o644}} {
		data, err := os.ReadFile(file.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(contextDir, file.name), data, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	containerfile := "FROM scratch\n" +
		"COPY claude /usr/local/bin/claude\n" +
		"COPY hxapprove /usr/local/bin/hxapprove\n" +
		"COPY fixture.ndjson /opt/hx-t27/fixture.ndjson\n" +
		"ENV PATH=/usr/local/bin HX_CLAUDE_FIXTURE=/opt/hx-t27/fixture.ndjson HX_CLAUDE_HOOK_ORDER=" + order +
		" HX_CLAUDE_HOOK_EXPECT_DECISION=deny HX_CLAUDE_HOLD_UNTIL_SIGUSR1=1 HX_CLAUDE_NO_SHELL=1\n" +
		"USER 1000:1000\n"
	if err := os.WriteFile(filepath.Join(contextDir, "Containerfile"), []byte(containerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	repository := "localhost/hx-t27-fakeclaude-" + order
	tag := repository + ":integration"
	runCommand(t, ctx, "podman", "build", "--pull=never", "-t", tag, "-f", filepath.Join(contextDir, "Containerfile"), contextDir)
	digest := strings.TrimSpace(string(runOutput(t, ctx, "podman", "image", "inspect", "--format", "{{.Digest}}", tag)))
	if len(digest) != 71 || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("VERIFICATION: invalid fakeclaude image digest %q", digest)
	}
	runCommand(t, ctx, "podman", "image", "inspect", repository+"@"+digest)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanup, "podman", "image", "rm", "--force", repository+"@"+digest).Run()
		_ = exec.CommandContext(cleanup, "podman", "image", "rm", "--force", tag).Run()
	})
	return repository, digest
}

// t27Buffer lets the deadlock branch read adapter stderr while the adapter
// may still be writing it.
type t27Buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *t27Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *t27Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
