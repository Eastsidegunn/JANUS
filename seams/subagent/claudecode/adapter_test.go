package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/subagent/internal/procgroup"
)

type adapterBinaries struct {
	adapter string
	fake    string
	approve string
}

func buildAdapterBinaries(t *testing.T) adapterBinaries {
	t.Helper()
	dir := t.TempDir()
	build := func(name, pkg string) string {
		t.Helper()
		bin := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", bin, pkg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s build: %v\n%s", name, err, out)
		}
		return bin
	}
	return adapterBinaries{
		adapter: build("claudecode", "./cmd/claudecode"),
		fake:    build("fakeclaude", "./testdata/fakeclaude"),
		approve: build("hxapprove", "./hxapprove"),
	}
}

type processRun struct {
	events []gen.Event
	stderr string
	err    error
}

func runFixtureProcess(t *testing.T, bins adapterBinaries, fixture string, env []string, afterReady []byte) processRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.adapter)
	absFixture, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(),
		"HX_CLAUDE_BIN="+bins.fake,
		"HX_CLAUDE_FIXTURE="+absFixture,
	)
	cmd.Env = append(cmd.Env, env...)
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
	workspace := t.TempDir() // C안 smoke와 같은 pristine 작업공간
	if _, err := stdin.Write(taskCommandLine(t, workspace)); err != nil {
		t.Fatal(err)
	}

	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	var events []gen.Event
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	commandSent := false
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
		if len(afterReady) > 0 && !commandSent && event.Kind == gen.EventKindSubagentReady {
			if _, err := stdin.Write(afterReady); err != nil {
				t.Fatal(err)
			}
			commandSent = true
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
	return processRun{events: events, stderr: stderr.String(), err: waitErr}
}

func taskCommandLine(t *testing.T, workspace string) []byte {
	t.Helper()
	payload, err := json.Marshal(gen.TaskPayload{
		Instruction: "fixture replay", Workspace: workspace,
		Budget: gen.Budget{Tokens: 1000, TimeMs: 1000, MaxDepth: 2}, Depth: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(gen.Command{V: 1, Cmd: gen.CommandCmdTask, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

func stopCommandLine(t *testing.T) []byte {
	t.Helper()
	payload, err := json.Marshal(gen.StopPayload{Reason: gen.StopPayloadReasonUser})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(gen.Command{V: 1, Cmd: gen.CommandCmdStop, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

func messageCommandLine(t *testing.T) []byte {
	t.Helper()
	payload, err := json.Marshal(gen.MessagePayload{Text: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(gen.Command{V: 1, Cmd: gen.CommandCmdMessage, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

func approvalResponseLine(t *testing.T, requestID string, decision gen.ApprovalResponsePayloadDecision) []byte {
	t.Helper()
	payload := gen.ApprovalResponsePayload{RequestID: requestID, Decision: decision}
	if decision == gen.ApprovalResponsePayloadDecisionDeny {
		reason := "테스트 거부"
		payload.Reason = &reason
	}
	p, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(gen.Command{V: 1, Cmd: gen.CommandCmdApprovalResponse, Payload: p})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

// FR-ADP-01/03/04/05: the built adapter consumes only real T8 Claude output,
// and its complete §5.2 stream matches the already-reviewed parser goldens.
func TestAdapterExecutableReplaysAllClaudeFixtures(t *testing.T) {
	bins := buildAdapterBinaries(t)
	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.ndjson"))
	if err != nil || len(paths) != 8 {
		t.Fatalf("fixtures=%d err=%v", len(paths), err)
	}
	for _, path := range paths {
		path := path
		name := strings.TrimSuffix(filepath.Base(path), ".ndjson")
		t.Run(name, func(t *testing.T) {
			run := runFixtureProcess(t, bins, path, nil, nil)
			if run.err != nil {
				t.Fatalf("adapter exit: %v\n%s", run.err, run.stderr)
			}
			goldenBytes, err := os.ReadFile(filepath.Join("testdata", "golden", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var golden goldenFile
			if err := json.Unmarshal(goldenBytes, &golden); err != nil {
				t.Fatal(err)
			}
			if len(run.events) != len(golden.Events) {
				t.Fatalf("events=%d want=%d", len(run.events), len(golden.Events))
			}
			for i, got := range run.events {
				want := golden.Events[i]
				if got.Kind != want.Kind || got.Raw != want.RawB64 || !jsonEqual(got.Payload, want.Payload) {
					t.Fatalf("event[%d]\ngot=%+v\nwant=%+v", i, got, want)
				}
			}
		})
	}
}

func jsonEqual(a, b []byte) bool {
	var av, bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && reflect.DeepEqual(av, bv)
}

func TestClaudeCommandUsesApprovedIsolationFlags(t *testing.T) {
	task := gen.TaskPayload{Instruction: "x", Workspace: "/workspace"}
	got := claudeCommand(Config{ClaudeBin: "/bin/claude"}, task)
	if got[0] != "/bin/claude" {
		t.Fatalf("executable=%q", got[0])
	}
	joined := strings.Join(got[1:], "\x00")
	for _, forbidden := range []string{"--bare", "--safe-mode"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("forbidden flag %s in %v", forbidden, got)
		}
	}
	for _, required := range []string{
		"--output-format\x00stream-json",
		"--no-session-persistence",
		"--permission-mode\x00manual",
		"--setting-sources\x00" + claudeSettingSources,
		"--settings\x00" + claudeApprovalHookSettings,
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("required flag pair %q missing from %v", required, got)
		}
	}
	if !json.Valid([]byte(claudeApprovalHookSettings)) {
		t.Fatal("inline hook settings is not JSON")
	}
}

func TestLocalPodmanConfigSelectsProcessEndpointBeforeHostProcgroup(t *testing.T) {
	var out, stderr bytes.Buffer
	line := taskCommandLine(t, t.TempDir())
	err := Run(context.Background(), io.NopCloser(bytes.NewReader(line)), &out, &stderr, Config{
		ClaudeBin:       "/does/not/execute",
		ProcessEndpoint: world.NewProcessEndpoint("tcp", "invalid", "lease", "control", "output"),
	})
	if err == nil || !strings.Contains(err.Error(), "process endpoint") {
		t.Fatalf("world endpoint branch error=%v", err)
	}
	if strings.Contains(err.Error(), "Claude 실행") {
		t.Fatalf("world branch fell back to host procgroup: %v", err)
	}
}

func TestAdapterPassesApprovedFlagsToClaudeProcess(t *testing.T) {
	bins := buildAdapterBinaries(t)
	argsPath := filepath.Join(t.TempDir(), "args.json")
	run := runFixtureProcess(t, bins, filepath.Join(fixtureDir, "01-simple-text.ndjson"), []string{
		"HX_CLAUDE_ARGS_OUT=" + argsPath,
	}, nil)
	if run.err != nil {
		t.Fatalf("adapter exit: %v\n%s", run.err, run.stderr)
	}
	b, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := claudeCommand(Config{ClaudeBin: bins.fake}, gen.TaskPayload{Instruction: "fixture replay"})[1:]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Claude args\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFinishNativeErrorPrecedence(t *testing.T) {
	handlerErr := errors.New("handler")
	scanErr := errors.New("scan")
	exitErr := errors.New("exit")
	cases := []struct {
		name    string
		drain   procgroup.DrainResult
		stop    bool
		wantErr error
		status  gen.DonePayloadStatus
		result  string
	}{
		{"handler before scan and exit", procgroup.DrainResult{HandlerErr: handlerErr, ScanErr: scanErr, ExitErr: exitErr}, false, handlerErr, "", ""},
		{"scan before exit", procgroup.DrainResult{ScanErr: scanErr, ExitErr: exitErr}, false, scanErr, "", ""},
		{"exit maps error", procgroup.DrainResult{ExitErr: exitErr}, false, nil, gen.DonePayloadStatusError, "(결과 없음: subtype=missing_result, terminal_reason=abnormal_exit)"},
		{"exit after stop maps stopped", procgroup.DrainResult{ExitErr: exitErr}, true, nil, gen.DonePayloadStatusStopped, "(결과 없음: subtype=missing_result, terminal_reason=abnormal_exit)"},
		{"clean exit without result maps error", procgroup.DrainResult{}, false, nil, gen.DonePayloadStatusError, "(결과 없음: subtype=missing_result, terminal_reason=process_exit)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, err := finishNative(tc.drain, nil, tc.stop)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err=%v want=%v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var payload gen.DonePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Status != tc.status || payload.Result != tc.result || len(event.Raw) != 0 {
				t.Fatalf("done=%+v raw=%q", payload, event.Raw)
			}
		})
	}
}

func TestFinishNativeExitMapsPendingDone(t *testing.T) {
	payload, err := json.Marshal(gen.DonePayload{Status: gen.DonePayloadStatusOk, Result: "native result"})
	if err != nil {
		t.Fatal(err)
	}
	pending := &Event{Kind: gen.EventKindSubagentDone, Payload: payload, Raw: []byte(`{"type":"result"}`)}
	for _, tc := range []struct {
		name   string
		stop   bool
		status gen.DonePayloadStatus
	}{
		{"abnormal exit", false, gen.DonePayloadStatusError},
		{"abnormal exit after stop", true, gen.DonePayloadStatusStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event, err := finishNative(procgroup.DrainResult{ExitErr: errors.New("exit")}, pending, tc.stop)
			if err != nil {
				t.Fatal(err)
			}
			var got gen.DonePayload
			if err := json.Unmarshal(event.Payload, &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.status || got.Result != "native result" || string(event.Raw) != `{"type":"result"}` {
				t.Fatalf("event=%+v payload=%+v", event, got)
			}
		})
	}
}

func TestAdapterSynthesizesDoneWithoutNativeResult(t *testing.T) {
	bins := buildAdapterBinaries(t)
	fixture := filepath.Join(fixtureDir, "01-simple-text.ndjson")
	cases := []struct {
		name, exitCode, result string
	}{
		{"clean exit", "", "(결과 없음: subtype=missing_result, terminal_reason=process_exit)"},
		{"abnormal exit", "7", "(결과 없음: subtype=missing_result, terminal_reason=abnormal_exit)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{"HX_CLAUDE_DROP_RESULT=1"}
			if tc.exitCode != "" {
				env = append(env, "HX_CLAUDE_EXIT_CODE="+tc.exitCode)
			}
			run := runFixtureProcess(t, bins, fixture, env, nil)
			if run.err != nil {
				t.Fatalf("adapter exit: %v\n%s", run.err, run.stderr)
			}
			assertLastDone(t, run.events, gen.DonePayloadStatusError, tc.result, "")
		})
	}
}

func TestAdapterStopKillsClaudeAndSynthesizesStopped(t *testing.T) {
	bins := buildAdapterBinaries(t)
	start := time.Now()
	run := runFixtureProcess(t, bins, filepath.Join(fixtureDir, "01-simple-text.ndjson"), []string{
		"HX_CLAUDE_DROP_RESULT=1", "HX_CLAUDE_HOLD=1",
	}, stopCommandLine(t))
	if run.err != nil {
		t.Fatalf("adapter exit: %v\n%s", run.err, run.stderr)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("stop did not terminate Claude group promptly: %v", time.Since(start))
	}
	assertLastDone(t, run.events, gen.DonePayloadStatusStopped,
		"(결과 없음: subtype=missing_result, terminal_reason=abnormal_exit)", "")
}

func TestAdapterEmitsDistinctDoneForFailuresAfterReady(t *testing.T) {
	bins := buildAdapterBinaries(t)
	fixture := filepath.Join(fixtureDir, "01-simple-text.ndjson")
	cases := []struct {
		name       string
		env        []string
		afterReady []byte
		result     string
	}{
		{
			name:       "message unsupported",
			env:        []string{"HX_CLAUDE_DROP_RESULT=1", "HX_CLAUDE_HOLD=1"},
			afterReady: messageCommandLine(t),
			result:     "(어댑터 오류: message 미지원)",
		},
		{
			name:       "inbound command contract violation",
			env:        []string{"HX_CLAUDE_DROP_RESULT=1", "HX_CLAUDE_HOLD=1"},
			afterReady: []byte(`{"v":1,"cmd":"message","payload":{}}` + "\n"),
			result:     "(어댑터 오류: 수신 command 계약 위반)",
		},
		{
			name:   "native stream contract violation",
			env:    []string{"HX_CLAUDE_DUPLICATE_FIRST=1"},
			result: "(어댑터 오류: native stream 계약 위반)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := runFixtureProcess(t, bins, fixture, tc.env, tc.afterReady)
			if run.err == nil {
				t.Fatalf("adapter unexpectedly succeeded; stderr=%s", run.stderr)
			}
			assertLastDone(t, run.events, gen.DonePayloadStatusError, tc.result, "")
		})
	}
}

func TestAdapterRejectsNativeEventBeforeInitWithoutBrokenOutput(t *testing.T) {
	bins := buildAdapterBinaries(t)
	run := runFixtureProcess(t, bins, filepath.Join(fixtureDir, "01-simple-text.ndjson"), []string{
		"HX_CLAUDE_SKIP_FIRST=1",
	}, nil)
	if run.err == nil {
		t.Fatalf("adapter unexpectedly succeeded; stderr=%s", run.stderr)
	}
	if len(run.events) != 0 {
		t.Fatalf("pre-ready failure emitted out-of-order events: %+v", run.events)
	}
	if !strings.Contains(run.stderr, "system/init보다 먼저 매핑 대상 이벤트") {
		t.Fatalf("stderr lost native cause: %q", run.stderr)
	}
}

type approvalProcessRun struct {
	processRun
	hookRaw    []byte
	hookOutput map[string]any
	requestID  string
	trace      []string
}

func runApprovalFixtureProcess(t *testing.T, bins adapterBinaries, decision gen.ApprovalResponsePayloadDecision, duplicate, stop bool) approvalProcessRun {
	return runApprovalFixtureProcessWithOptions(t, bins, decision, duplicate, stop, nil, nil)
}

// runApprovalFixtureProcessWithOptions keeps the approval request pending while
// a test-controlled native-output gate establishes one side of the stop race.
// The fixture itself remains the sole source of native stream-json lines.
func runApprovalFixtureProcessWithOptions(t *testing.T, bins adapterBinaries, decision gen.ApprovalResponsePayloadDecision, duplicate, stop bool, extraEnv []string, beforeStop func() error) approvalProcessRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.adapter)
	fixture, err := filepath.Abs(filepath.Join(fixtureDir, "01-simple-text.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	hookRaw := []byte("{\n  \"hook_event_name\": \"PreToolUse\", \"tool_use_id\": \"call-1\",\n  \"tool_name\": \"Bash\", \"tool_input\": {\"command\":\"true\"}\n}\n")
	hookOut := filepath.Join(t.TempDir(), "hook-output.json")
	cmd.Env = append(os.Environ(),
		"HX_CLAUDE_BIN="+bins.fake,
		"HX_CLAUDE_FIXTURE="+fixture,
		"HX_CLAUDE_RUN_HOOK=1",
		"HX_CLAUDE_HOOK_INPUT="+string(hookRaw),
		"HX_CLAUDE_HOOK_OUT="+hookOut,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
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
	var trace []string
	trace = append(trace, "task-sent")
	var requestID string
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
		trace = append(trace, "event:"+string(event.Kind))
		if event.Kind == gen.EventKindSubagentDone {
			var done gen.DonePayload
			if err := json.Unmarshal(event.Payload, &done); err == nil {
				trace = append(trace, fmt.Sprintf("done:%s:%q", done.Status, done.Result))
			}
		}
		if event.Kind != gen.EventKindSubagentApprovalRequest {
			continue
		}
		var request gen.ApprovalRequestPayload
		if err := json.Unmarshal(event.Payload, &request); err != nil {
			t.Fatal(err)
		}
		requestID = request.RequestID
		decodedRaw, err := base64.StdEncoding.DecodeString(event.Raw)
		if err != nil || !bytes.Equal(decodedRaw, hookRaw) {
			t.Fatalf("approval raw mismatch err=%v\ngot=%q\nwant=%q", err, decodedRaw, hookRaw)
		}
		if stop {
			if beforeStop != nil {
				if err := beforeStop(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := stdin.Write(stopCommandLine(t)); err != nil {
				t.Fatal(err)
			}
			trace = append(trace, "stop-sent")
			continue
		}
		response := approvalResponseLine(t, requestID, decision)
		if duplicate {
			response = append(response, response...)
		}
		if _, err := stdin.Write(response); err != nil {
			t.Fatal(err)
		}
		trace = append(trace, "approval-response-sent:"+string(decision))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	stdin.Close()
	if ctx.Err() != nil {
		t.Fatalf("adapter timeout: %v\n%s", ctx.Err(), stderr.String())
	}
	var output map[string]any
	if b, err := os.ReadFile(hookOut); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &output); err != nil {
			t.Fatal(err)
		}
	}
	return approvalProcessRun{
		processRun: processRun{events: events, stderr: stderr.String(), err: waitErr},
		hookRaw:    hookRaw, hookOutput: output, requestID: requestID, trace: trace,
	}
}

func TestApprovalHandshakeAllowAndDenyPreservesRaw(t *testing.T) {
	bins := buildAdapterBinaries(t)
	for _, decision := range []gen.ApprovalResponsePayloadDecision{
		gen.ApprovalResponsePayloadDecisionAllow,
		gen.ApprovalResponsePayloadDecisionDeny,
	} {
		t.Run(string(decision), func(t *testing.T) {
			run := runApprovalFixtureProcess(t, bins, decision, false, false)
			if run.err != nil {
				t.Fatalf("adapter exit: %v\n%s", run.err, run.stderr)
			}
			if run.requestID == "" || run.hookOutput == nil {
				t.Fatalf("request/hook output missing: %+v", run)
			}
			specific, ok := run.hookOutput["hookSpecificOutput"].(map[string]any)
			if !ok || specific["hookEventName"] != "PreToolUse" || specific["permissionDecision"] != string(decision) {
				t.Fatalf("hook output=%v", run.hookOutput)
			}
		})
	}
}

func TestApprovalResponseCorrelationViolations(t *testing.T) {
	bins := buildAdapterBinaries(t)
	fixture := filepath.Join(fixtureDir, "01-simple-text.ndjson")
	t.Run("unmatched", func(t *testing.T) {
		line := approvalResponseLine(t, "22222222-2222-4222-8222-222222222222", gen.ApprovalResponsePayloadDecisionAllow)
		run := runFixtureProcess(t, bins, fixture, []string{"HX_CLAUDE_DROP_RESULT=1", "HX_CLAUDE_HOLD=1"}, line)
		if run.err == nil {
			t.Fatal("unmatched approval_response succeeded")
		}
		assertLastDone(t, run.events, gen.DonePayloadStatusError, "(어댑터 오류: 미상관 approval_response)", "")
	})
	t.Run("duplicate", func(t *testing.T) {
		run := runApprovalFixtureProcess(t, bins, gen.ApprovalResponsePayloadDecisionAllow, true, false)
		if run.err == nil {
			t.Fatal("duplicate approval_response succeeded")
		}
		assertLastDone(t, run.events, gen.DonePayloadStatusError, "(어댑터 오류: 중복 approval_response)", "")
	})
}

func assertStoppedApprovalRun(t *testing.T, run approvalProcessRun, result string, nativeRaw bool) {
	t.Helper()
	if run.err != nil {
		t.Fatalf("stop exit: %v\n%s", run.err, run.stderr)
	}
	specific, ok := run.hookOutput["hookSpecificOutput"].(map[string]any)
	if !ok || specific["permissionDecision"] != "deny" {
		t.Fatalf("pending hook did not receive deny: %v", run.hookOutput)
	}
	approvalIndex, stopIndex, doneIndex := -1, -1, -1
	for i, item := range run.trace {
		switch {
		case item == "event:subagent/approval_request":
			approvalIndex = i
		case item == "stop-sent":
			stopIndex = i
		case item == "event:subagent/done":
			doneIndex = i
		}
	}
	if approvalIndex < 0 || stopIndex <= approvalIndex || doneIndex <= stopIndex {
		t.Fatalf("stop/deny/native order invalid: %s", strings.Join(run.trace, " -> "))
	}
	if doneIndex != len(run.trace)-2 { // the following trace item is done:<status>:<result>
		t.Fatalf("done was not terminal in trace: %s", strings.Join(run.trace, " -> "))
	}
	hasRaw := len(run.events) > 0 && run.events[len(run.events)-1].Raw != ""
	if hasRaw != nativeRaw {
		t.Fatalf("done raw presence=%v want native=%v", hasRaw, nativeRaw)
	}
	var last gen.Event
	if len(run.events) > 0 {
		last = run.events[len(run.events)-1]
	}
	assertLastDone(t, []gen.Event{last}, gen.DonePayloadStatusStopped, result, last.Raw)
}

func waitForFile(t *testing.T, path string) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("marker %q 대기 timeout", path)
}

func TestStopDeniesPendingHookBeforeNativeTerminationKillFirst(t *testing.T) {
	bins := buildAdapterBinaries(t)
	// The synchronous hook remains pending; after deny, fakeclaude is held
	// before replaying the remaining fixture lines, so native kill wins.
	gate := filepath.Join(t.TempDir(), "release-after-hook")
	run := runApprovalFixtureProcessWithOptions(t, bins, gen.ApprovalResponsePayloadDecisionDeny, false, true,
		[]string{"HX_CLAUDE_HOLD_AFTER_HOOK=" + gate}, nil)
	t.Logf("stop trace: %s", strings.Join(run.trace, " -> "))
	assertStoppedApprovalRun(t, run,
		"(결과 없음: subtype=missing_result, terminal_reason=abnormal_exit)", false)
}

func TestStopDeniesPendingHookBeforeNativeTerminationOutputFirst(t *testing.T) {
	bins := buildAdapterBinaries(t)
	marker := filepath.Join(t.TempDir(), "result-ready")
	// The hook is asynchronous and remains pending while the fixture's result
	// is replayed. The marker lets the test send stop only after native output
	// has won the race, yielding the other contract-valid result.
	run := runApprovalFixtureProcessWithOptions(t, bins, gen.ApprovalResponsePayloadDecisionDeny, false, true,
		[]string{"HX_CLAUDE_HOOK_ASYNC=1", "HX_CLAUDE_RESULT_READY=" + marker},
		func() error { return waitForFile(t, marker) })
	t.Logf("stop trace: %s", strings.Join(run.trace, " -> "))
	assertStoppedApprovalRun(t, run, "2", true)
}

func TestContextCancellationCleansPendingHookViaProcessDone(t *testing.T) {
	bins := buildAdapterBinaries(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	fixture, err := filepath.Abs(filepath.Join(fixtureDir, "01-simple-text.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"HX_CLAUDE_FIXTURE="+fixture,
		"HX_CLAUDE_RUN_HOOK=1",
	)
	env = replaceEnv(env, "PATH", filepath.Dir(bins.approve)+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stderr bytes.Buffer
	result := make(chan error, 1)
	go func() {
		err := Run(ctx, inReader, outWriter, &stderr, Config{ClaudeBin: bins.fake, Env: env})
		outWriter.Close()
		result <- err
	}()
	if _, err := inWriter.Write(taskCommandLine(t, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(outReader)
	var events []gen.Event
	canceled := false
	for scanner.Scan() {
		var event gen.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
		if event.Kind == gen.EventKindSubagentApprovalRequest && !canceled {
			cancel() // procgroup converts this single signal into native Done.
			canceled = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation left hook/socket/process pending")
	}
	inWriter.Close()
	if !canceled {
		t.Fatalf("approval request not observed; stderr=%s", stderr.String())
	}
	assertLastDone(t, events, gen.DonePayloadStatusError,
		"(결과 없음: subtype=missing_result, terminal_reason=abnormal_exit)", "")
}

func assertLastDone(t *testing.T, events []gen.Event, status gen.DonePayloadStatus, result, raw string) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	last := events[len(events)-1]
	if last.Kind != gen.EventKindSubagentDone || last.Raw != raw {
		t.Fatalf("last=%+v", last)
	}
	var payload gen.DonePayload
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Status != status || payload.Result != result {
		t.Fatalf("done=%+v want status=%s result=%q", payload, status, result)
	}
}
