package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/seams/subagent/internal/procgroup"
)

type adapterBinaries struct {
	adapter string
	fake    string
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
	}
}

type processRun struct {
	events []gen.Event
	stderr string
	err    error
}

func runFixtureProcess(t *testing.T, bins adapterBinaries, fixture string, env []string, stopAfterReady bool) processRun {
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
	stopSent := false
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
		if stopAfterReady && !stopSent && event.Kind == gen.EventKindSubagentReady {
			if _, err := stdin.Write(stopCommandLine(t)); err != nil {
				t.Fatal(err)
			}
			stopSent = true
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
			run := runFixtureProcess(t, bins, path, nil, false)
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

func TestAdapterPassesApprovedFlagsToClaudeProcess(t *testing.T) {
	bins := buildAdapterBinaries(t)
	argsPath := filepath.Join(t.TempDir(), "args.json")
	run := runFixtureProcess(t, bins, filepath.Join(fixtureDir, "01-simple-text.ndjson"), []string{
		"HX_CLAUDE_ARGS_OUT=" + argsPath,
	}, false)
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
			run := runFixtureProcess(t, bins, fixture, env, false)
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
	}, true)
	if run.err != nil {
		t.Fatalf("adapter exit: %v\n%s", run.err, run.stderr)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("stop did not terminate Claude group promptly: %v", time.Since(start))
	}
	assertLastDone(t, run.events, gen.DonePayloadStatusStopped,
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
