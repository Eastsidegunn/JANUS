package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

func TestRunEmitsValidatedReadyAndDone(t *testing.T) {
	input := `{"v":1,"cmd":"task","payload":{}}
{"type":"thread.started","thread_id":"t1"}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}
`
	var out, diag bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(input), &out, &diag, policy.ApprovalAuto); err != nil {
		t.Fatal(err)
	}
	vals, _ := validate.New()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("events=%d", len(lines))
	}
	for _, line := range lines {
		if err := vals.ValidateEvent([]byte(line)); err != nil {
			t.Fatalf("invalid event: %v (%s)", err, line)
		}
	}
	var first, last gen.Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	if first.Kind != gen.EventKind(gen.KindSubagentReady) || last.Kind != gen.EventKind(gen.KindSubagentDone) {
		t.Fatalf("order=%s,%s", first.Kind, last.Kind)
	}
}

func TestRunManualCommandFailsClosed(t *testing.T) {
	input := `{"v":1,"cmd":"task","payload":{}}
{"type":"thread.started","thread_id":"t1"}
{"type":"item.started","item":{"id":"c1","type":"command_execution","status":"in_progress","command":"touch x"}}
`
	var out, diag bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(input), &out, &diag, policy.ApprovalManual); err == nil {
		t.Fatal("manual command unexpectedly allowed")
	}
	if !strings.Contains(out.String(), `"subagent/done"`) {
		t.Fatalf("missing done: %s", out.String())
	}
}

func TestRunUnknownNativeEventFails(t *testing.T) {
	input := `{"v":1,"cmd":"task","payload":{}}
{"type":"thread.started","thread_id":"t1"}
{"type":"future.event"}
`
	var out, diag bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(input), &out, &diag, policy.ApprovalAuto); err == nil {
		t.Fatal("unknown event accepted")
	}
}
