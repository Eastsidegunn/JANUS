package claudecode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
)

func TestApprovalGatePreHookValidationExemptionIsExact(t *testing.T) {
	wrapper := "<tool_use_error>String to replace not found in file.\nString: zzz-not-present</tool_use_error>"
	cases := []struct {
		name       string
		isError    bool
		content    any
		wantExempt bool
	}{
		{"string exact", true, "  \n" + wrapper + "\t", true},
		{"block array one text", true, []map[string]any{{"type": "text", "text": wrapper}}, true},
		{"partial prefix", true, "before " + wrapper, false},
		{"partial suffix", true, wrapper + " after", false},
		{"plain error", true, "File does not exist.", false},
		{"ok wrapped", false, wrapper, false},
		{"block array two text blocks", true, []map[string]any{
			{"type": "text", "text": wrapper},
			{"type": "text", "text": "extra"},
		}, false},
		{"block array mixed", true, []map[string]any{
			{"type": "text", "text": wrapper},
			{"type": "image", "source": map[string]any{"type": "base64", "data": "AA=="}},
		}, false},
		{"block array plain text", true, []map[string]any{{"type": "text", "text": "Exit code 42"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resultEvent, raw := parseUnapprovedToolResult(t, tc.isError, tc.content)
			vals, err := validate.New()
			if err != nil {
				t.Fatal(err)
			}
			var output, diagnostics bytes.Buffer
			w := &wireWriter{out: &output, diagnostics: &diagnostics, vals: vals, gate: newApprovalGateLedger()}
			if err := w.emit(gen.EventKindSubagentToolCall, mustPayload(t, gen.AgentToolCallPayload{
				CallID: "call-validation", Name: "Edit", Args: json.RawMessage(`{"file_path":"sample.txt"}`),
			}), nil); err != nil {
				t.Fatal(err)
			}
			err = w.emitEvent(resultEvent)
			if tc.wantExempt {
				if err != nil {
					t.Fatalf("exact pre-hook validation result became fatal: %v", err)
				}
				if got := diagnostics.String(); got != approvalGateInputValidationDiagnostic+"call-validation\n" {
					t.Fatalf("exemption diagnostic=%q", got)
				}
				assertLastWireRaw(t, output.Bytes(), raw)
				return
			}
			if !errors.Is(err, errApprovalGateBypass) {
				t.Fatalf("unapproved result err=%v, want approval gate bypass", err)
			}
			if diagnostics.Len() != 0 {
				t.Fatalf("non-exempt result emitted exemption diagnostic: %q", diagnostics.String())
			}
		})
	}
}

func TestParserDuplicateToolResultAfterValidationWrapperIsFatal(t *testing.T) {
	p := NewParser()
	mustParse(t, p, initLine)
	mustParse(t, p, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-validation","name":"Edit","input":{}}]}}`)
	first := toolResultNativeLine(t, true, "<tool_use_error>String to replace not found</tool_use_error>")
	if _, err := p.ParseLine(first); err != nil {
		t.Fatal(err)
	}
	second := toolResultNativeLine(t, false, "edited")
	if _, err := p.ParseLine(second); err == nil || !strings.Contains(err.Error(), "tool_result 중복") {
		t.Fatalf("duplicate tool_result err=%v", err)
	}
}

func parseUnapprovedToolResult(t *testing.T, isError bool, content any) (Event, []byte) {
	t.Helper()
	p := NewParser()
	mustParse(t, p, initLine)
	mustParse(t, p, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-validation","name":"Edit","input":{}}]}}`)
	raw := toolResultNativeLine(t, isError, content)
	events, err := p.ParseLine(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != gen.EventKindSubagentToolResult {
		t.Fatalf("tool_result events=%+v", events)
	}
	return events[0], raw
}

func toolResultNativeLine(t *testing.T, isError bool, content any) []byte {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []map[string]any{{
				"type": "tool_result", "tool_use_id": "call-validation",
				"is_error": isError, "content": content,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func assertLastWireRaw(t *testing.T, output, want []byte) {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(output), []byte{'\n'})
	if len(lines) == 0 {
		t.Fatal("wire output missing")
	}
	var event gen.Event
	if err := json.Unmarshal(lines[len(lines)-1], &event); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(event.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("wire raw=%s want=%s", raw, want)
	}
}
