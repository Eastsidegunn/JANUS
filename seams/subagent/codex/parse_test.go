package codex

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

func TestRecordedMixedToolLifecycle(t *testing.T) {
	p := NewParser(policy.ApprovalAuto)
	lines := [][]byte{
		[]byte(`{"type":"thread.started","thread_id":"01a11509-test"}`),
		[]byte(`{"type":"item.started","item":{"id":"item_cmd_ok","type":"command_execution","command":"cat hello.txt","aggregated_output":"","exit_code":null,"status":"in_progress"}}`),
		[]byte(`{"type":"item.completed","item":{"id":"item_cmd_ok","type":"command_execution","command":"cat hello.txt","aggregated_output":"hello","exit_code":0,"status":"completed"}}`),
		[]byte(`{"type":"item.started","item":{"id":"item_file","type":"file_change","changes":[{"path":"/workspace/hello.txt","kind":"update"}],"status":"in_progress"}}`),
		[]byte(`{"type":"item.completed","item":{"id":"item_file","type":"file_change","changes":[{"path":"/workspace/hello.txt","kind":"update"}],"status":"completed"}}`),
		[]byte(`{"type":"item.started","item":{"id":"item_cmd_fail","type":"command_execution","command":"sh -c 'exit 128'","aggregated_output":"","exit_code":null,"status":"in_progress"}}`),
		[]byte(`{"type":"item.completed","item":{"id":"item_cmd_fail","type":"command_execution","command":"sh -c 'exit 128'","aggregated_output":"","exit_code":128,"status":"failed"}}`),
		[]byte(`{"type":"item.completed","item":{"id":"item_msg","type":"agent_message","text":"done"}}`),
		[]byte(`{"type":"turn.completed"}`),
	}
	var events []Event
	for _, line := range lines {
		got, err := p.ParseLine(line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		events = append(events, got...)
	}
	if len(events) != 9 {
		t.Fatalf("events=%d, want ready + 3 call/result pairs + message + done: %+v", len(events), events)
	}
	if events[0].Kind != gen.EventKindSubagentReady || events[8].Kind != gen.EventKindSubagentDone {
		t.Fatalf("lifecycle kinds=%v,%v", events[0].Kind, events[8].Kind)
	}
	wantKinds := []gen.EventKind{
		gen.EventKindSubagentToolCall, gen.EventKindSubagentToolResult,
		gen.EventKindSubagentToolCall, gen.EventKindSubagentToolResult,
		gen.EventKindSubagentToolCall, gen.EventKindSubagentToolResult,
	}
	for i, want := range wantKinds {
		if events[i+1].Kind != want {
			t.Fatalf("events[%d].Kind=%q, want %q", i+1, events[i+1].Kind, want)
		}
	}
	var results []gen.SubagentToolResultPayload
	for _, index := range []int{2, 4, 6} {
		var result gen.SubagentToolResultPayload
		if err := json.Unmarshal(events[index].Payload, &result); err != nil {
			t.Fatalf("result payload %d: %v", index, err)
		}
		results = append(results, result)
	}
	if results[0].Status != gen.SubagentToolResultPayloadStatusOk || results[1].Status != gen.SubagentToolResultPayloadStatusOk || results[2].Status != gen.SubagentToolResultPayloadStatusError {
		t.Fatalf("result statuses=%q,%q,%q, want ok,ok,error", results[0].Status, results[1].Status, results[2].Status)
	}
	var done gen.DonePayload
	if err := json.Unmarshal(events[8].Payload, &done); err != nil {
		t.Fatal(err)
	}
	if done.Status != gen.DonePayloadStatusOk {
		t.Fatalf("done status=%q, want ok", done.Status)
	}
}

func TestManualApprovalIsFailClosed(t *testing.T) {
	p := NewParser()
	if _, err := p.ParseLine([]byte(`{"type":"thread.started","thread_id":"t1"}`)); err != nil {
		t.Fatal(err)
	}
	_, err := p.ParseLine([]byte(`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"touch denied","status":"in_progress"}}`))
	if !errors.Is(err, ErrApprovalUnavailable) {
		t.Fatalf("manual parser error=%v, want ErrApprovalUnavailable", err)
	}
	if p.Disposition() != "" || len(p.pending) != 0 {
		t.Fatalf("manual approval failure must not emit or register an effect: disposition=%q pending=%v", p.Disposition(), p.pending)
	}
	events, finishErr := p.Finish()
	if !errors.Is(finishErr, ErrApprovalUnavailable) {
		t.Fatalf("Finish error=%v, want approval error", finishErr)
	}
	if len(events) != 1 || events[0].Kind != gen.EventKindSubagentDone || len(events[0].Raw) != 0 {
		t.Fatalf("manual failure done=%+v, want one synthetic done", events)
	}
	if !strings.Contains(string(events[0].Payload), "승인 경로 미관측") {
		t.Fatalf("done does not preserve deterministic approval reason: %s", events[0].Payload)
	}
	if !p.Done() {
		t.Fatal("manual failure must terminate the parser")
	}
}

func TestExplicitAutoAllowsRecordedEffect(t *testing.T) {
	p := NewParser(policy.ApprovalAuto)
	if _, err := p.ParseLine([]byte(`{"type":"thread.started","thread_id":"t1"}`)); err != nil {
		t.Fatal(err)
	}
	events, err := p.ParseLine([]byte(`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"echo ok","status":"in_progress"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != gen.EventKindSubagentToolCall {
		t.Fatalf("auto parser events=%+v, want tool_call", events)
	}
	if !strings.Contains(string(events[0].Payload), `"call_id":"i1"`) {
		t.Fatalf("tool call lacks native id: %s", events[0].Payload)
	}
}

func TestUnknownNativeEventFailsClosed(t *testing.T) {
	p := NewParser(policy.ApprovalAuto)
	if _, err := p.ParseLine([]byte(`{"type":"thread.started","thread_id":"t1"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ParseLine([]byte(`{"type":"new_future_event"}`)); err == nil {
		t.Fatal("unknown native event was silently accepted")
	}
	if _, err := p.Finish(); err == nil {
		t.Fatal("post-ready unknown event must produce terminal error")
	}
}

func TestUnknownItemTypeFailsClosedInsideKnownEvent(t *testing.T) {
	p := NewParser(policy.ApprovalAuto)
	if _, err := p.ParseLine([]byte(`{"type":"thread.started","thread_id":"t1"}`)); err != nil {
		t.Fatal(err)
	}
	_, err := p.ParseLine([]byte(`{"type":"item.completed","item":{"id":"i1","type":"future_item_type","text":"must not disappear"}}`))
	if err == nil {
		t.Fatal("unknown item.type inside known event.type was silently accepted")
	}
	if !strings.Contains(err.Error(), "미지의 item type") {
		t.Fatalf("error=%v, want explicit unknown item type diagnosis", err)
	}
}

func TestCodexRequiresThreadStartedFirst(t *testing.T) {
	p := NewParser(policy.ApprovalAuto)
	if _, err := p.ParseLine([]byte(`{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"early"}}`)); err == nil {
		t.Fatal("native item before thread.started was accepted")
	}
	if p.Ready() || p.Done() {
		t.Fatalf("pre-ready contract failure emitted lifecycle event: ready=%v done=%v", p.Ready(), p.Done())
	}
}

func TestInterruptedCodexStreamIsStopped(t *testing.T) {
	p := NewParser(policy.ApprovalAuto)
	for _, line := range [][]byte{
		[]byte(`{"type":"thread.started","thread_id":"t1"}`),
		[]byte(`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"tail -f /dev/null","status":"in_progress"}}`),
	} {
		if _, err := p.ParseLine(line); err != nil {
			t.Fatal(err)
		}
	}
	events, err := p.Finish()
	if err != nil || len(events) != 1 {
		t.Fatalf("Finish events=%+v err=%v", events, err)
	}
	if !strings.Contains(string(events[0].Payload), `"status":"stopped"`) {
		t.Fatalf("interrupted stream status=%s", events[0].Payload)
	}
}
