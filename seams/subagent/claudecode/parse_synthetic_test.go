package claudecode

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/seams/subagent/internal/procgroup"
)

// T27(a) 입력: claude-code가 상류 오류 시 합성하는 assistant 메시지(실 세션
// 2026-10-02 관측 형태). 표식 조합별로 둔다 — model만, 플래그만, 둘 다.
const (
	syntheticAuthBoth         = `{"type":"assistant","message":{"id":"msg_syn1","model":"<synthetic>","role":"assistant","type":"message","stop_reason":"stop_sequence","usage":{"input_tokens":0,"output_tokens":0},"content":[{"type":"text","text":"Failed to authenticate. API Error: 403 egress denied"}]},"parent_tool_use_id":null,"session_id":"s1","is_api_error_message":true}`
	syntheticTimeoutModelOnly = `{"type":"assistant","message":{"id":"msg_syn2","model":"<synthetic>","role":"assistant","type":"message","content":[{"type":"text","text":"Request timed out"}]},"parent_tool_use_id":null,"session_id":"s1"}`
	syntheticTimeoutFlagOnly  = `{"type":"assistant","message":{"id":"msg_syn3","model":"claude-opus-5","role":"assistant","type":"message","content":[{"type":"text","text":"Request timed out"}]},"parent_tool_use_id":null,"session_id":"s1","is_api_error_message":true}`
	syntheticTimeoutMsgFlag   = `{"type":"assistant","message":{"id":"msg_syn4","model":"claude-opus-5","is_api_error_message":true,"role":"assistant","type":"message","content":[{"type":"text","text":"Request timed out"}]},"parent_tool_use_id":null,"session_id":"s1"}`

	modelTextLine      = `{"type":"assistant","message":{"model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"2"}]},"session_id":"s1"}`
	okResultLine       = `{"type":"result","subtype":"success","is_error":false,"result":"2","session_id":"s1","usage":{"input_tokens":3,"output_tokens":1}}`
	authErrResultLine  = `{"type":"result","subtype":"success","is_error":true,"result":"Failed to authenticate. API Error: 403 egress denied","session_id":"s1"}`
	otherErrResultLine = `{"type":"result","subtype":"success","is_error":true,"result":"API Error","session_id":"s1"}`
)

func parseAll(t *testing.T, p *Parser, lines ...string) []Event {
	t.Helper()
	var out []Event
	for _, line := range lines {
		events, err := p.ParseLine([]byte(line))
		if err != nil {
			t.Fatalf("입력 거부: %v\n%s", err, line)
		}
		out = append(out, events...)
	}
	return out
}

func donePayload(t *testing.T, ev Event) gen.DonePayload {
	t.Helper()
	if ev.Kind != gen.EventKindSubagentDone {
		t.Fatalf("done이 아님: %s", ev.Kind)
	}
	var done gen.DonePayload
	if err := json.Unmarshal(ev.Payload, &done); err != nil {
		t.Fatal(err)
	}
	return done
}

func assertNoMessage(t *testing.T, events []Event, forbidden string) {
	t.Helper()
	for _, ev := range events {
		if ev.Kind == gen.EventKindSubagentMessage {
			var m gen.AgentMessagePayload
			_ = json.Unmarshal(ev.Payload, &m)
			if strings.Contains(m.Text, forbidden) || forbidden == "" {
				t.Fatalf("합성 API 오류가 모델 텍스트로 방출됨: %s", ev.Payload)
			}
		}
	}
}

// assertRawCarried checks FR-LOG-07: the synthetic native line survives
// verbatim as the raw of exactly one emitted event, and that event carries no
// model-visible text (usage{0,0} carrier).
func assertRawCarried(t *testing.T, events []Event, line string) {
	t.Helper()
	carriers := 0
	for _, ev := range events {
		if !bytes.Equal(ev.Raw, []byte(line)) {
			continue
		}
		carriers++
		if ev.Kind != gen.EventKindSubagentUsage {
			t.Fatalf("합성 줄 raw의 운반 kind=%s, want subagent/usage", ev.Kind)
		}
		var u gen.UsagePayload
		if err := json.Unmarshal(ev.Payload, &u); err != nil || u.InputTokens != 0 || u.OutputTokens != 0 {
			t.Fatalf("운반 usage가 0/0이 아님: %s err=%v", ev.Payload, err)
		}
	}
	if carriers != 1 {
		t.Fatalf("합성 줄 raw 보존 이벤트 수=%d, want 1", carriers)
	}
}

// T27(a) 완료 기준: 합성 메시지 + is_error result → subagent/message 부재,
// done{error} 사유에 텍스트 포함, raw 보존.
func TestSyntheticAPIErrorTerminalMapsToErrorDone(t *testing.T) {
	cases := []struct {
		name, synthetic, result, text string
	}{
		{"both markers, result repeats text", syntheticAuthBoth, authErrResultLine, "Failed to authenticate. API Error: 403 egress denied"},
		{"both markers, result differs", syntheticAuthBoth, otherErrResultLine, "Failed to authenticate. API Error: 403 egress denied"},
		{"model marker only", syntheticTimeoutModelOnly, otherErrResultLine, "Request timed out"},
		{"line flag only", syntheticTimeoutFlagOnly, otherErrResultLine, "Request timed out"},
		{"message flag only", syntheticTimeoutMsgFlag, otherErrResultLine, "Request timed out"},
		// result가 is_error를 놓쳐도 합성 오류 뒤 모델 출력 없이 끝난 턴은 오류다.
		{"result not flagged", syntheticTimeoutModelOnly, `{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s1"}`, "Request timed out"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, stop := range []bool{false, true} {
				p := NewParser()
				events := parseAll(t, p, initLine)
				if stop {
					p.NoteStop()
				}
				events = append(events, parseAll(t, p, c.synthetic, c.result)...)
				assertNoMessage(t, events, "")
				assertRawCarried(t, events, c.synthetic)
				done := donePayload(t, events[len(events)-1])
				want := gen.DonePayloadStatusError
				if stop {
					want = gen.DonePayloadStatusStopped // T19: stop 1순위 유지
				}
				if done.Status != want || !strings.Contains(done.Result, c.text) {
					t.Fatalf("stop=%v done=%+v want status=%s result∋%q", stop, done, want, c.text)
				}
				if string(events[len(events)-1].Raw) != c.result {
					t.Fatalf("done raw가 result 줄이 아님: %q", events[len(events)-1].Raw)
				}
			}
		})
	}
}

// 비터미널: 합성 오류 뒤 정상 턴이 이어지면 합성 텍스트는 모델 텍스트도
// done 사유도 아니다. raw만 usage{0,0} 운반으로 보존된다.
func TestSyntheticAPIErrorNonTerminalIsNotModelText(t *testing.T) {
	p := NewParser()
	events := parseAll(t, p, initLine, syntheticTimeoutModelOnly, modelTextLine, okResultLine)
	assertNoMessage(t, events, "Request timed out")
	assertRawCarried(t, events, syntheticTimeoutModelOnly)
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, string(ev.Kind))
	}
	want := "subagent/ready subagent/usage subagent/message subagent/usage subagent/done"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("kinds=%v want %s", kinds, want)
	}
	done := donePayload(t, events[len(events)-1])
	if done.Status != gen.DonePayloadStatusOk || done.Result != "2" {
		t.Fatalf("정상 회복 턴의 done 변질: %+v", done)
	}
	if p.APIErrorText() != "" {
		t.Fatalf("해소된 합성 오류가 남음: %q", p.APIErrorText())
	}
}

// 정상 메시지 경로 회귀: 합성 표식이 없는 assistant 텍스트는 그대로 message다.
func TestOrdinaryAssistantTextStillMessage(t *testing.T) {
	p := NewParser()
	events := parseAll(t, p, initLine, modelTextLine, okResultLine)
	if len(events) != 4 || events[1].Kind != gen.EventKindSubagentMessage || string(events[1].Raw) != modelTextLine {
		t.Fatalf("정상 메시지 경로 변질: %+v", events)
	}
	if done := donePayload(t, events[3]); done.Status != gen.DonePayloadStatusOk || done.Result != "2" {
		t.Fatalf("done=%+v", done)
	}
}

// 세션이 result 없이 끝나면(터미널) 미해소 합성 오류가 합성 done 사유에 담긴다.
func TestSyntheticAPIErrorWithoutResultAnnotatesSynthesizedDone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		drain  procgroup.DrainResult
		stop   bool
		status gen.DonePayloadStatus
	}{
		{"clean exit", procgroup.DrainResult{}, false, gen.DonePayloadStatusError},
		{"stopped", procgroup.DrainResult{}, true, gen.DonePayloadStatusStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParser()
			events := parseAll(t, p, initLine, syntheticTimeoutModelOnly)
			assertNoMessage(t, events, "")
			assertRawCarried(t, events, syntheticTimeoutModelOnly)
			ev, err := finishNative(tc.drain, nil, tc.stop)
			if err != nil {
				t.Fatal(err)
			}
			ev, err = WithAPIErrorDone(ev, p.APIErrorText())
			if err != nil {
				t.Fatal(err)
			}
			done := donePayload(t, ev)
			if done.Status != tc.status || !strings.Contains(done.Result, "Request timed out") || !strings.Contains(done.Result, "missing_result") {
				t.Fatalf("done=%+v", done)
			}
		})
	}
	// 해소된 합성 오류는 아무것도 바꾸지 않는다.
	ev, err := finishNative(procgroup.DrainResult{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	same, err := WithAPIErrorDone(ev, "")
	if err != nil || !bytes.Equal(same.Payload, ev.Payload) {
		t.Fatalf("빈 합성 오류가 done을 바꿈: %s", same.Payload)
	}
}

// multiturn: 마지막 턴이 합성 오류로 끝나면 terminal done(TurnDone)이 오류다.
func TestSyntheticAPIErrorMultiturnLastTurn(t *testing.T) {
	p := NewMultiturnParser()
	events := parseAll(t, p, initLine, modelTextLine, okResultLine, syntheticTimeoutModelOnly, otherErrResultLine)
	assertNoMessage(t, events, "Request timed out")
	assertRawCarried(t, events, syntheticTimeoutModelOnly)
	done, err := p.TurnDone()
	if err != nil || done == nil {
		t.Fatalf("TurnDone=%v err=%v", done, err)
	}
	payload := donePayload(t, *done)
	if payload.Status != gen.DonePayloadStatusError || !strings.Contains(payload.Result, "Request timed out") {
		t.Fatalf("done=%+v", payload)
	}

	// 앞 턴의 합성 오류가 뒤 턴에서 회복되면 terminal done은 정상이다.
	p = NewMultiturnParser()
	parseAll(t, p, initLine, syntheticTimeoutModelOnly, otherErrResultLine, modelTextLine, okResultLine)
	done, err = p.TurnDone()
	if err != nil || done == nil {
		t.Fatalf("TurnDone=%v err=%v", done, err)
	}
	if payload := donePayload(t, *done); payload.Status != gen.DonePayloadStatusOk || payload.Result != "2" {
		t.Fatalf("회복된 multiturn done=%+v", payload)
	}
}

// fail-closed: 합성 메시지가 tool_use를 실어 나르면 조용히 통과시키지 않는다.
func TestSyntheticAPIErrorRejectsNonTextBlocks(t *testing.T) {
	p := NewParser()
	mustParse(t, p, initLine)
	line := `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},"session_id":"s1"}`
	if _, err := p.ParseLine([]byte(line)); err == nil || !strings.Contains(err.Error(), "합성 API 오류 메시지에 text 외 블록") {
		t.Fatalf("err=%v", err)
	}
}
