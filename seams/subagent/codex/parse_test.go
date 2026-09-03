package codex

import (
	"errors"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

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
