package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/world"
)

const (
	bypassFixtureCallID = "toolu_01Nn8KS7Rke53sMr4xJMyFCc"
	bypassResult        = "approval gate bypassed: tool_result Glob/" + bypassFixtureCallID + " without approval_request"
)

func TestApprovalGateLedgerRequiresPrecedingApprovalRequest(t *testing.T) {
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	w := &wireWriter{out: &output, vals: vals, gate: newApprovalGateLedger()}
	call := mustPayload(t, gen.AgentToolCallPayload{CallID: bypassFixtureCallID, Name: "Glob", Args: json.RawMessage(`{"pattern":"*"}`)})
	result := mustPayload(t, gen.AgentToolResultPayload{CallID: bypassFixtureCallID, Status: gen.AgentToolResultPayloadStatusOk, Output: json.RawMessage(`{"text":"a.txt"}`)})
	if err := w.emit(gen.EventKindSubagentToolCall, call, nil); err != nil {
		t.Fatal(err)
	}
	err = w.emit(gen.EventKindSubagentToolResult, result, nil)
	if !strings.Contains(errString(err), bypassResult) {
		t.Fatalf("missing approval err=%v", err)
	}
	if lines := bytes.Count(output.Bytes(), []byte{'\n'}); lines != 2 {
		t.Fatalf("tool_result was not written before fatal: lines=%d output=%s", lines, output.String())
	}
}

func TestApprovalGateLedgerRequiresDecisionSend(t *testing.T) {
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	w := &wireWriter{out: &output, vals: vals, gate: newApprovalGateLedger()}
	call := mustPayload(t, gen.AgentToolCallPayload{CallID: "call-timeout", Name: "Bash", Args: json.RawMessage(`{"command":"true"}`)})
	request := mustPayload(t, gen.ApprovalRequestPayload{
		RequestID: "11111111-1111-4111-8111-111111111111", CallID: "call-timeout", Name: "Bash", Args: json.RawMessage(`{"command":"true"}`),
	})
	for _, event := range []struct {
		kind    gen.EventKind
		payload json.RawMessage
	}{{gen.EventKindSubagentToolCall, call}, {gen.EventKindSubagentApprovalRequest, request}} {
		if err := w.emit(event.kind, event.payload, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Model Claude's non-blocking 600s hook timeout: the request is durable,
	// but the pending hook never receives a decision.
	err = w.emit(gen.EventKindSubagentToolResult, mustPayload(t, gen.AgentToolResultPayload{
		CallID: "call-timeout", Status: gen.AgentToolResultPayloadStatusOk, Output: json.RawMessage(`{"text":"ran"}`),
	}), nil)
	if !errors.Is(err, errApprovalGateBypass) {
		t.Fatalf("undelivered decision did not trip gate: %v", err)
	}
	if lines := bytes.Count(output.Bytes(), []byte{'\n'}); lines != 3 {
		t.Fatalf("request/result wire evidence missing: lines=%d output=%s", lines, output.String())
	}
}

func TestApprovalGateLedgerAcceptsHookFirstAndNativeFirst(t *testing.T) {
	for _, order := range []string{"hook-first", "native-first"} {
		t.Run(order, func(t *testing.T) {
			vals, err := validate.New()
			if err != nil {
				t.Fatal(err)
			}
			w := &wireWriter{out: io.Discard, vals: vals, gate: newApprovalGateLedger()}
			call := mustPayload(t, gen.AgentToolCallPayload{CallID: "call-1", Name: "Read", Args: json.RawMessage(`{}`)})
			request := mustPayload(t, gen.ApprovalRequestPayload{
				RequestID: "11111111-1111-4111-8111-111111111111", CallID: "call-1", Name: "Read", Args: json.RawMessage(`{}`),
			})
			if order == "hook-first" {
				err = w.emit(gen.EventKindSubagentApprovalRequest, request, nil)
				if err == nil {
					err = w.emit(gen.EventKindSubagentToolCall, call, nil)
				}
			} else {
				err = w.emit(gen.EventKindSubagentToolCall, call, nil)
				if err == nil {
					err = w.emit(gen.EventKindSubagentApprovalRequest, request, nil)
				}
			}
			// Both orders become approved immediately before the transport sends
			// its decision to the hook.
			if err == nil {
				w.markApprovalDecisionSent("call-1", "Read")
			}
			if err == nil {
				err = w.emit(gen.EventKindSubagentToolResult, mustPayload(t, gen.AgentToolResultPayload{
					CallID: "call-1", Status: gen.AgentToolResultPayloadStatusOk, Output: json.RawMessage(`{"text":"ok"}`),
				}), nil)
			}
			if err != nil {
				t.Fatalf("valid %s order failed: %v", order, err)
			}
		})
	}
}

func TestApprovalGateAllowsOrderedFixtureEndToEnd(t *testing.T) {
	bins := buildAdapterBinaries(t)
	fixture, err := filepath.Abs(filepath.Join(fixtureDir, "02-single-tool.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"host", "world"} {
		for _, order := range []string{"native-first", "hook-first"} {
			t.Run(transport+"/"+order, func(t *testing.T) {
				var events []gen.Event
				var stderr string
				var runErr error
				if transport == "host" {
					events, _, stderr, runErr = runOrderedFixture(
						t, bins, fixture, order, gen.ApprovalResponsePayloadDecisionAllow,
					)
				} else {
					events, stderr, runErr = runWorldOrderedFixture(t, bins, fixture, order)
				}
				if runErr != nil {
					t.Fatalf("allow 경로 실패: %v\nstderr=%s", runErr, stderr)
				}
				if strings.Contains(stderr, "approval gate bypassed") {
					t.Fatalf("allow 경로가 bypass로 진단됨: %s", stderr)
				}
				assertApprovalGateAllow(t, events)
			})
		}
	}
}

func assertApprovalGateAllow(t *testing.T, events []gen.Event) {
	t.Helper()
	resultCount, doneCount := 0, 0
	for _, event := range events {
		if event.Kind == gen.EventKindSubagentToolResult {
			resultCount++
		}
		if event.Kind != gen.EventKindSubagentDone {
			continue
		}
		doneCount++
		var done gen.DonePayload
		if err := json.Unmarshal(event.Payload, &done); err != nil {
			t.Fatal(err)
		}
		if done.Status != gen.DonePayloadStatusOk || strings.Contains(done.Result, "approval gate bypassed") {
			t.Fatalf("done=%+v", done)
		}
	}
	if resultCount != 1 || doneCount != 1 || len(events) == 0 || events[len(events)-1].Kind != gen.EventKindSubagentDone {
		t.Fatalf("tool_result=%d done=%d terminal=%v events=%+v", resultCount, doneCount, len(events) > 0 && events[len(events)-1].Kind == gen.EventKindSubagentDone, events)
	}
}

func runWorldOrderedFixture(t *testing.T, bins adapterBinaries, fixture, order string) ([]gen.Event, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	releaseProcess := make(chan struct{})
	approvals := newFakeWorldApprovalBroker(t, withFakeWorldApprovalRelay(20*time.Millisecond, func() {
		close(releaseProcess)
	}))
	defer approvals.Close()
	argv := ContainerArgv(bins.fake, "fixture replay")
	process := startMultiturnBroker(t, ctx, argv, []string{
		"HX_CLAUDE_FIXTURE=" + fixture,
		"HX_CLAUDE_HOOK_ORDER=" + order,
		"HX_CLAUDE_HOOK_EXPECT_DECISION=allow",
		"HX_CLAUDE_HOLD_UNTIL_SIGUSR1=1",
		approvalSocketEnv + "=" + approvals.RelayAddress(),
		"PATH=" + filepath.Dir(bins.approve) + ":" + os.Getenv("PATH"),
	})
	go func() {
		select {
		case <-releaseProcess:
			_ = process.signalProcess(syscall.SIGUSR1)
		case <-ctx.Done():
		}
	}()

	input, commands := io.Pipe()
	output, adapterOutput := io.Pipe()
	var stderr bytes.Buffer
	runDone := make(chan error, 1)
	go func() {
		err := Run(ctx, input, adapterOutput, &stderr, Config{
			ClaudeBin: "not-a-host-executable",
			ProcessEndpoint: world.NewProcessEndpoint(
				"unix", process.listener.Addr().String(), "lease", "control", "output",
			),
			ApprovalEndpoint: world.NewApprovalEndpoint(
				"unix", approvals.listener.Addr().String(), "capability",
			),
			WorldSpanID: "2222222222222222",
		})
		_ = adapterOutput.CloseWithError(err)
		runDone <- err
	}()
	if _, err := commands.Write(taskCommandLine(t, t.TempDir())); err != nil {
		t.Fatal(err)
	}

	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	var events []gen.Event
	scanner := bufio.NewScanner(output)
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
		if _, err := commands.Write(approvalResponseLine(t, request.RequestID, gen.ApprovalResponsePayloadDecisionAllow)); err != nil {
			t.Fatal(err)
		}
	}
	_ = commands.Close()
	runErr := <-runDone
	if scanErr := scanner.Err(); scanErr != nil && runErr == nil {
		runErr = scanErr
	}
	select {
	case relayErr := <-approvals.relayFinished:
		if runErr == nil && relayErr != nil {
			runErr = relayErr
		}
	case <-time.After(2 * time.Second):
		if runErr == nil {
			runErr = errors.New("world approval relay completion timeout")
		}
	}
	select {
	case brokerErr := <-process.finished:
		if runErr == nil && brokerErr != nil {
			runErr = brokerErr
		}
	case <-time.After(2 * time.Second):
		if runErr == nil {
			runErr = errors.New("world process broker completion timeout")
		}
	}
	select {
	case intent := <-approvals.intents:
		if runErr == nil && (intent.CallID != bypassFixtureCallID || intent.Name != "Glob") {
			runErr = errors.New("world tool intent mismatch")
		}
	default:
		if runErr == nil {
			runErr = errors.New("world tool intent missing")
		}
	}
	return events, stderr.String(), runErr
}

func mustPayload(t *testing.T, value any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestApprovalGateBypassHostStopsAndEndsError(t *testing.T) {
	bins := buildAdapterBinaries(t)
	fixture, err := filepath.Abs(filepath.Join(fixtureDir, "02-single-tool.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"exit-1", "not-run"} {
		t.Run(failure, func(t *testing.T) {
			started := time.Now()
			run := runFixtureProcess(t, bins, fixture, []string{
				"HX_CLAUDE_HOOK_ORDER=native-first",
				"HX_CLAUDE_HOOK_FAILURE=" + failure,
				"HX_CLAUDE_HOLD_UNTIL_SIGUSR1=1",
			}, nil)
			if run.err == nil {
				t.Fatalf("approval bypass exited successfully; stderr=%s", run.stderr)
			}
			if time.Since(started) > 5*time.Second {
				t.Fatalf("native process was not stopped promptly: %v", time.Since(started))
			}
			assertBypassEvents(t, run.events)
			if !strings.Contains(run.stderr, bypassResult) {
				t.Fatalf("stderr lacks bypass diagnostic: %q", run.stderr)
			}
		})
	}
}

func TestApprovalGateBypassWorldRequestsStopAndEndsError(t *testing.T) {
	bins := buildAdapterBinaries(t)
	fixture, err := filepath.Abs(filepath.Join(fixtureDir, "02-single-tool.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	argv := ContainerArgv(bins.fake, "fixture replay")
	broker := startMultiturnBroker(t, ctx, argv, []string{
		"HX_CLAUDE_FIXTURE=" + fixture,
		"HX_CLAUDE_HOLD=1",
	})
	input, commands := io.Pipe()
	defer commands.Close()
	task := taskCommandLine(t, t.TempDir())
	go func() { _, _ = commands.Write(task) }()
	var output, stderr bytes.Buffer
	err = Run(ctx, input, &output, &stderr, Config{
		ClaudeBin: "not-a-host-executable",
		ProcessEndpoint: world.NewProcessEndpoint(
			"unix", broker.listener.Addr().String(), "lease", "control", "output",
		),
		WorldSpanID: "2222222222222222",
	})
	if err == nil || !strings.Contains(err.Error(), bypassResult) {
		t.Fatalf("Run error=%v stderr=%s", err, stderr.String())
	}
	select {
	case <-broker.stops:
	case <-time.After(2 * time.Second):
		t.Fatal("world process did not receive Stop after approval bypass")
	}
	var events []gen.Event
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		var event gen.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("event decode: %v\n%s", err, line)
		}
		events = append(events, event)
	}
	assertBypassEvents(t, events)
	if !strings.Contains(stderr.String(), bypassResult) {
		t.Fatalf("stderr lacks bypass diagnostic: %q", stderr.String())
	}
	if err := <-broker.finished; err != nil {
		t.Fatalf("broker: %v", err)
	}
}

func TestApprovalGateParserRejectedResultIsNotFatal(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join(fixtureDir, "05-approval-denied.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	w := &wireWriter{out: io.Discard, vals: vals, gate: newApprovalGateLedger()}
	parser := NewParser()
	var approvals, rejected int
	for _, line := range bytes.Split(bytes.TrimSpace(fixture), []byte{'\n'}) {
		events, err := parser.ParseLine(line)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			switch event.Kind {
			case gen.EventKindSubagentApprovalRequest:
				approvals++
			case gen.EventKindSubagentToolResult:
				var result gen.AgentToolResultPayload
				if err := json.Unmarshal(event.Payload, &result); err != nil {
					t.Fatal(err)
				}
				if result.Status == gen.AgentToolResultPayloadStatusRejected {
					rejected++
				}
			}
			if err := w.emitEvent(event); err != nil {
				t.Fatalf("parser-synthesized rejection became fatal: %v", err)
			}
		}
	}
	if approvals != 0 || rejected != 1 {
		t.Fatalf("approval_requests=%d rejected_results=%d", approvals, rejected)
	}
}

func assertBypassEvents(t *testing.T, events []gen.Event) {
	t.Helper()
	toolResult, doneCount := -1, 0
	for i, event := range events {
		if event.Kind == gen.EventKindSubagentToolResult {
			toolResult = i
		}
		if event.Kind != gen.EventKindSubagentDone {
			continue
		}
		doneCount++
		var done gen.DonePayload
		if err := json.Unmarshal(event.Payload, &done); err != nil {
			t.Fatal(err)
		}
		if done.Status != gen.DonePayloadStatusError || done.Result != bypassResult {
			t.Fatalf("done=%+v", done)
		}
		if toolResult < 0 || toolResult >= i {
			t.Fatalf("tool_result was not emitted before done: %+v", events)
		}
	}
	if doneCount != 1 {
		t.Fatalf("done count=%d events=%+v", doneCount, events)
	}
}
