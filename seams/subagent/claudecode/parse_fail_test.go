package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustParse(t *testing.T, p *Parser, line string) {
	t.Helper()
	if _, err := p.ParseLine([]byte(line)); err != nil {
		t.Fatalf("정상 입력이 거부됨: %v", err)
	}
}

const initLine = `{"type":"system","subtype":"init","session_id":"s1","model":"m","tools":["Bash"]}`

// 제안서 §8.4 fail-closed 회귀 목록.
func TestFailClosedCases(t *testing.T) {
	cases := []struct {
		name    string
		lines   []string
		wantErr string
	}{
		{"빈 줄", []string{""}, "빈 줄"},
		{"잘못된 JSON", []string{`{"type":`}, "JSON 파싱"},
		{"init 없이 assistant", []string{`{"type":"assistant","message":{"content":[{"type":"text","text":"too early"}]}}`}, "첫 native 줄은 system/init"},
		{"미지 native type", []string{initLine, `{"type":"telemetry"}`}, "미지의 네이티브 이벤트"},
		{"미지 system subtype", []string{initLine, `{"type":"system","subtype":"mystery"}`}, "미지의 system subtype"},
		{"미지 assistant block", []string{initLine,
			`{"type":"assistant","message":{"content":[{"type":"image"}]}}`}, "미지의 assistant content block"},
		{"plugin_install 격리 위반", []string{initLine, `{"type":"system","subtype":"plugin_install"}`}, "격리 계약 위반"},
		{"hook_started 격리 위반", []string{initLine, `{"type":"hook_started"}`}, "격리 계약 위반"},
		{"init 중복", []string{initLine, initLine}, "system/init 중복"},
		{"permission_denied에 id 없음", []string{initLine,
			`{"type":"system","subtype":"permission_denied","decision_reason":"x"}`}, "tool_use_id 없음"},
		{"tool_use에 id 없음", []string{initLine,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`}, "id 또는 name 없음"},
		{"usage 핵심값 누락", []string{initLine,
			`{"type":"result","subtype":"success","result":"r","usage":{"input_tokens":1}}`}, "usage 핵심값 누락"},
		{"usage 음수", []string{initLine,
			`{"type":"result","subtype":"success","result":"r","usage":{"input_tokens":-1,"output_tokens":1}}`}, "음수 usage"},
		{"usage overflow", []string{initLine,
			`{"type":"result","subtype":"success","result":"r","usage":{"input_tokens":9223372036854775807,"output_tokens":1,"cache_read_input_tokens":1}}`}, "overflow"},
		{"거부 미해소 상태로 result 도달", []string{initLine,
			`{"type":"system","subtype":"permission_denied","tool_use_id":"t1","decision_reason":"r"}`,
			`{"type":"result","subtype":"success","result":"r"}`}, "거부 확인 통보 없이 result"},
		{"거부 뒤 다른 결과", []string{initLine,
			`{"type":"system","subtype":"permission_denied","tool_use_id":"t1","decision_reason":"r"}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`},
			"user-rejected가 아닌 결과"},
		{"done 이후 출력", []string{initLine,
			`{"type":"result","subtype":"success","result":"r"}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"유령"}]}}`}, "done 이후 출력"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewParser()
			var lastErr error
			for _, l := range c.lines {
				if _, err := p.ParseLine([]byte(l)); err != nil {
					lastErr = err
					break
				}
			}
			if lastErr == nil {
				t.Fatal("위반 입력이 통과함")
			}
			if !strings.Contains(lastErr.Error(), c.wantErr) {
				t.Fatalf("오류 %q에 %q 없음", lastErr, c.wantErr)
			}
		})
	}
}

// 크기 계약 (제안서 §8.2): 64KiB 초과 유효 줄은 정상 처리, 상한 초과만 거부.
func TestLineSizeContract(t *testing.T) {
	p := NewParser()
	mustParse(t, p, initLine)

	big := strings.Repeat("a", 200*1024) // 200KiB > 64KiB
	line := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + big + `"}]}}`
	evs, err := p.ParseLine([]byte(line))
	if err != nil {
		t.Fatalf("64KiB 초과 유효 줄이 거부됨(%d bytes): %v", len(line), err)
	}
	if len(evs) != 1 || evs[0].Kind != "subagent/tool_result" {
		t.Fatalf("이벤트 이상: %+v", evs)
	}

	huge := make([]byte, MaxLineBytes+1)
	if _, err := p.ParseLine(huge); err == nil || !strings.Contains(err.Error(), "상한 초과") {
		t.Fatalf("상한 초과 줄이 거부되지 않음: %v", err)
	}
}

// usage 객체 전체 부재 → 이벤트 생략 + 폴백 (FR-ADP-07, 제안서 §5.2).
func TestUsageAbsentIsOmittedNotError(t *testing.T) {
	p := NewParser()
	mustParse(t, p, initLine)
	evs, err := p.ParseLine([]byte(`{"type":"result","subtype":"success","result":"끝"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "subagent/done" {
		t.Fatalf("usage 부재인데 %d개 이벤트: %+v", len(evs), evs)
	}
}

// 캐시 보조값 누락은 0으로 간주 (제안서 §5.2).
func TestCacheFieldsDefaultToZero(t *testing.T) {
	p := NewParser()
	mustParse(t, p, initLine)
	evs, err := p.ParseLine([]byte(`{"type":"result","subtype":"success","result":"끝","usage":{"input_tokens":5,"output_tokens":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	var u struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	json.Unmarshal(evs[0].Payload, &u)
	if u.InputTokens != 5 || u.OutputTokens != 7 {
		t.Fatalf("usage = %+v (5/7 기대)", u)
	}
}

// api_retry는 무시된다 — 픽스처에 없으므로 합성 입력으로 고정 (제안서 §3.4).
func TestApiRetryIgnoredSynthetic(t *testing.T) {
	p := NewParser()
	mustParse(t, p, initLine)
	evs, err := p.ParseLine([]byte(`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("api_retry가 이벤트를 만듦: %+v", evs)
	}
	if p.Disposition() != "ignored:system/api_retry" {
		t.Fatalf("disposition = %q", p.Disposition())
	}
}

// result → done 매핑 (제안서 §8.3).
func TestDoneStatusMapping(t *testing.T) {
	cases := []struct {
		name, line string
		stop       bool
		want       string
		wantResult string
	}{
		{"success", `{"type":"result","subtype":"success","terminal_reason":"completed","result":"끝"}`, false, "ok", "끝"},
		{"stop 명령 뒤 success", `{"type":"result","subtype":"success","terminal_reason":"completed","result":"끝"}`, true, "stopped", "끝"},
		{"중단", `{"type":"result","subtype":"error_during_execution","terminal_reason":"aborted_streaming"}`, false, "stopped",
			"(결과 없음: subtype=error_during_execution, terminal_reason=aborted_streaming)"},
		{"stop 명령 선행", `{"type":"result","subtype":"error_during_execution","terminal_reason":"other"}`, true, "stopped",
			"(결과 없음: subtype=error_during_execution, terminal_reason=other)"},
		{"기타 오류", `{"type":"result","subtype":"error_max_turns","terminal_reason":"completed"}`, false, "error",
			"(결과 없음: subtype=error_max_turns, terminal_reason=completed)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewParser()
			mustParse(t, p, initLine)
			if c.stop {
				p.NoteStop()
			}
			evs, err := p.ParseLine([]byte(c.line))
			if err != nil {
				t.Fatal(err)
			}
			var d struct {
				Status string `json:"status"`
				Result string `json:"result"`
			}
			json.Unmarshal(evs[len(evs)-1].Payload, &d)
			if d.Status != c.want || d.Result != c.wantResult {
				t.Fatalf("done = %+v (status=%s result=%q 기대)", d, c.want, c.wantResult)
			}
		})
	}
}

// 1원본 → N정규화면 동일 raw가 각각 붙는다 (제안서 §4).
func TestSameRawAttachedToEachEvent(t *testing.T) {
	p := NewParser()
	mustParse(t, p, initLine)
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"하겠다"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`
	evs, err := p.ParseLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("이벤트 %d개 (2개 기대)", len(evs))
	}
	if string(evs[0].Raw) != line || string(evs[1].Raw) != line {
		t.Fatal("두 이벤트의 raw가 원본 줄과 동일하지 않음")
	}
}
