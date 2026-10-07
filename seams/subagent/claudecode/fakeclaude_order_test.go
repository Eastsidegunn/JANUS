package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
)

// TestFakeClaudeHookOrderModes pins the T27(b) fakeclaude ordering modes on
// the host T9 approval path (no world intent gate), so the fake used by the
// Linux container gate is itself verified in macOS `make ci`. hook-first must
// not write the native tool_use line before the hook has returned, which makes
// approval_request strictly precede tool_call; in both modes the lines after
// the anchor wait for the hook, so tool_result follows the decision.
func TestFakeClaudeHookOrderModes(t *testing.T) {
	bins := buildAdapterBinaries(t)
	fixture, err := filepath.Abs(filepath.Join(fixtureDir, "05-approval-denied.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	const callID = "toolu_015RSsY1h8UtCcm35pts1TAc"
	for _, order := range []string{"native-first", "hook-first"} {
		t.Run(order, func(t *testing.T) {
			events, raw, stderr, waitErr := runOrderedFixture(t, bins, fixture, order)
			if waitErr != nil {
				t.Fatalf("adapter exit: %v\n%s", waitErr, stderr)
			}
			index := map[gen.EventKind]int{}
			for i, event := range events {
				if _, seen := index[event.Kind]; !seen {
					index[event.Kind] = i
				}
			}
			for _, kind := range []gen.EventKind{
				gen.EventKindSubagentToolCall, gen.EventKindSubagentApprovalRequest,
				gen.EventKindSubagentToolResult, gen.EventKindSubagentDone,
			} {
				if _, ok := index[kind]; !ok {
					t.Fatalf("%s 누락: %+v", kind, events)
				}
			}
			if index[gen.EventKindSubagentToolResult] < index[gen.EventKindSubagentApprovalRequest] {
				t.Fatalf("hook 결정 전에 anchor 이후 native 줄이 나감: %+v", events)
			}
			if order == "hook-first" && index[gen.EventKindSubagentToolCall] < index[gen.EventKindSubagentApprovalRequest] {
				t.Fatalf("hook-first인데 tool_call이 approval_request보다 먼저: %+v", events)
			}
			var hook struct {
				HookEventName string          `json:"hook_event_name"`
				ToolUseID     string          `json:"tool_use_id"`
				ToolName      string          `json:"tool_name"`
				ToolInput     json.RawMessage `json:"tool_input"`
			}
			if err := json.Unmarshal(raw, &hook); err != nil || hook.HookEventName != "PreToolUse" ||
				hook.ToolUseID != callID || hook.ToolName != "Write" || len(hook.ToolInput) == 0 {
				t.Fatalf("anchor에서 유도한 hook 입력 이상: %q err=%v", raw, err)
			}
			last := events[len(events)-1]
			var done gen.DonePayload
			if err := json.Unmarshal(last.Payload, &done); err != nil || last.Kind != gen.EventKindSubagentDone || done.Status != gen.DonePayloadStatusOk {
				t.Fatalf("마지막 이벤트가 done{ok}가 아님: %+v err=%v", last, err)
			}
		})
	}
	t.Run("rejects-legacy-hook-env", func(t *testing.T) {
		cmd := exec.Command(bins.fake)
		cmd.Env = append(os.Environ(), "HX_CLAUDE_FIXTURE="+fixture, "HX_CLAUDE_HOOK_ORDER=hook-first", "HX_CLAUDE_RUN_HOOK=1")
		cmd.Stdin = bytes.NewReader(nil)
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
			t.Fatalf("legacy hook env와의 조합이 거부되지 않음: err=%v out=%s", err, out)
		}
	})
	for name, env := range map[string][]string{
		"rejects-order-with-multiturn":      {"HX_CLAUDE_HOOK_ORDER=hook-first", "HX_CLAUDE_MULTITURN_FIXTURES=" + fixture},
		"rejects-hold-without-order":        {"HX_CLAUDE_HOLD_UNTIL_SIGUSR1=1"},
		"rejects-expectation-without-order": {"HX_CLAUDE_HOOK_EXPECT_DECISION=deny"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(bins.fake)
			cmd.Env = append(append(os.Environ(), "HX_CLAUDE_FIXTURE="+fixture), env...)
			cmd.Stdin = bytes.NewReader(nil)
			out, err := cmd.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || len(bytes.TrimSpace(out)) == 0 {
				t.Fatalf("fail-closed 가드가 exit 2로 거부하지 않음: err=%v out=%s", err, out)
			}
			if bytes.Contains(out, []byte(`"type"`)) {
				t.Fatalf("거부 전에 native 줄이 출력됨: %s", out)
			}
		})
	}
}

func TestFakeClaudeFailClosedHookFailures(t *testing.T) {
	bins := buildAdapterBinaries(t)
	fixture, err := filepath.Abs(filepath.Join(fixtureDir, "05-approval-denied.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	base := []string{
		"-p", "fixture replay", "--output-format", "stream-json", "--verbose",
		"--no-session-persistence", "--permission-mode", "manual",
		"--setting-sources", "project,local", "--settings", claudeApprovalHookSettings,
	}
	brokenDir := t.TempDir()
	broken := filepath.Join(brokenDir, "hxapprove")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nsleep 0.05\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"relay-absent", []string{"PATH=" + filepath.Dir(bins.approve) + ":/usr/bin:/bin", "HX_APPROVAL_SOCKET="}},
		{"connection-failure", []string{"PATH=" + filepath.Dir(bins.approve) + ":/usr/bin:/bin", "HX_APPROVAL_SOCKET=/tmp/hx-t30-no-relay.sock"}},
		{"hxapprove-missing", []string{"PATH=/usr/bin:/bin", "HX_APPROVAL_SOCKET="}},
		{"hxapprove-exit2", []string{"PATH=" + brokenDir + ":/usr/bin:/bin", "HX_APPROVAL_SOCKET="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bins.fake, base...)
			cmd.Env = append(os.Environ(),
				"HX_CLAUDE_FIXTURE="+fixture,
				"HX_CLAUDE_HOOK_ORDER=hook-first",
			)
			cmd.Env = append(cmd.Env, tc.env...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if err == nil {
				t.Fatalf("hook failure let fakeclaude continue; stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), `"type":"tool_use"`) {
				t.Fatalf("tool executed after hook failure; stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
		})
	}
}

func TestFakeClaudeModelsNonBlockingHookErrors(t *testing.T) {
	bins := buildAdapterBinaries(t)
	fixture, err := filepath.Abs(filepath.Join(fixtureDir, "05-approval-denied.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		command string
		timeout int
	}{
		{"exit-one", "exit 1", 10},
		{"timeout", "sleep 2", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := fmt.Sprintf(`{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":%q,"timeout":%d}]}]}}`, tc.command, tc.timeout)
			cmd := exec.Command(bins.fake,
				"-p", "fixture replay", "--output-format", "stream-json", "--verbose",
				"--no-session-persistence", "--permission-mode", "manual",
				"--setting-sources", "project,local", "--settings", settings)
			cmd.Env = append(os.Environ(),
				"HX_CLAUDE_FIXTURE="+fixture,
				"HX_CLAUDE_HOOK_ORDER=hook-first",
				"PATH=/usr/bin:/bin",
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("non-blocking hook error stopped tool: %v stderr=%s", err, stderr.String())
			}
			if !strings.Contains(stdout.String(), `"type":"tool_use"`) {
				t.Fatalf("tool did not proceed after non-blocking hook error: stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
		})
	}
}

func runOrderedFixture(t *testing.T, bins adapterBinaries, fixture, order string) ([]gen.Event, []byte, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.adapter)
	cmd.Env = append(os.Environ(),
		"HX_CLAUDE_BIN="+bins.fake,
		"HX_CLAUDE_FIXTURE="+fixture,
		"HX_CLAUDE_HOOK_ORDER="+order,
		"HX_CLAUDE_HOOK_EXPECT_DECISION=deny",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write(taskCommandLine(t, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	var events []gen.Event
	var hookRaw []byte
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if err := vals.ValidateEvent(line); err != nil {
			t.Fatalf("adapter event contract: %v\n%s", err, line)
		}
		var event gen.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
		if event.Kind != gen.EventKindSubagentApprovalRequest {
			continue
		}
		var request gen.ApprovalRequestPayload
		if err := json.Unmarshal(event.Payload, &request); err != nil {
			t.Fatal(err)
		}
		if hookRaw, err = base64.StdEncoding.DecodeString(event.Raw); err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.Write(approvalResponseLine(t, request.RequestID, gen.ApprovalResponsePayloadDecisionDeny)); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	stdin.Close()
	if ctx.Err() != nil {
		t.Fatalf("adapter timeout: %v\n%s", ctx.Err(), stderr.String())
	}
	return events, hookRaw, stderr.String(), waitErr
}
