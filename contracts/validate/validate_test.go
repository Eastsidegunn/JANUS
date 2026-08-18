package validate

import (
	"encoding/json"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

func newV(t *testing.T) *Validators {
	t.Helper()
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

const (
	trace = `"11111111111111111111111111111111"`
	span  = `"2222222222222222"`
)

// envelope은 유효한 공통 필드 위에 kind/payload/추가 필드를 얹는다.
func envelope(rest string) string {
	return `{"seq":1,"ts":1700000000000,"trace_id":` + trace + `,"span_id":` + span + `,"actor":"parent",` + rest + `}`
}

func TestValidateRecordValid(t *testing.T) {
	v := newV(t)
	cases := map[string]string{
		"열린 kind":              envelope(`"kind":"session/start","payload":{}`),
		"열린 kind + 자유 payload": envelope(`"kind":"assistant/message","payload":{"text":"hi"}`),
		"subagent actor":       `{"seq":2,"ts":1,"trace_id":` + trace + `,"span_id":` + span + `,"parent_span_id":` + span + `,"actor":"subagent:claude-code:1","kind":"subagent/ready","payload":{"grade":"observable"}}`,
		"raw와 usage":           envelope(`"kind":"tool/result","payload":{"status":"ok","output":{"stdout":"x"}},"raw":"aGVsbG8=","usage_in":10,"usage_out":20`),
		// T5 확정 payload (2026-08-17 [H] 승인)
		"user/message":         envelope(`"kind":"user/message","payload":{"text":"질문"}`),
		"assistant/message":    envelope(`"kind":"assistant/message","payload":{"text":"응답"}`),
		"tool/call":            envelope(`"kind":"tool/call","payload":{"name":"bash","args":{"cmd":"ls"}}`),
		"tool/result rejected": envelope(`"kind":"tool/result","payload":{"status":"rejected","reason":"훅 거부"}`),
		"tool/result error":    envelope(`"kind":"tool/result","payload":{"status":"error","error":"실행 실패"}`),
		"turn 경계 빈 객체":         envelope(`"kind":"turn/start","payload":{}`),
		// T9 확정 subagent payload (2026-08-18 [H] 승인)
		"subagent/ready":                envelope(`"kind":"subagent/ready","payload":{"grade":"observable","model":"claude-opus-5"}`),
		"subagent/message":              envelope(`"kind":"subagent/message","payload":{"text":"진행 중"}`),
		"subagent/tool_call":            envelope(`"kind":"subagent/tool_call","payload":{"call_id":"toolu_1","name":"Bash","args":{"command":"ls"}}`),
		"subagent/tool_result ok":       envelope(`"kind":"subagent/tool_result","payload":{"call_id":"toolu_1","status":"ok","output":{"stdout":"x"}}`),
		"subagent/tool_result rejected": envelope(`"kind":"subagent/tool_result","payload":{"call_id":"toolu_1","status":"rejected","reason":"정책 거부"}`),
		"subagent/approval_request":     envelope(`"kind":"subagent/approval_request","payload":{"request_id":"req-1","call_id":"toolu_1","name":"Write","args":{"path":"/x"}}`),
		"step 경계 빈 객체":                  envelope(`"kind":"step/end","payload":{}`),
		"session/fork":                  envelope(`"kind":"session/fork","payload":{"origin_trace_id":` + trace + `,"origin_seq":42}`),
		"hook continue":                 envelope(`"kind":"hook/verdict","payload":{"point":"pre_step","verdict":"continue"}`),
		"hook rewrite":                  envelope(`"kind":"hook/verdict","payload":{"point":"pre_tool","verdict":"rewrite","rewrite":{"args":{"path":"/x"}},"reason":"경로 교정"}`),
		"hook reject":                   envelope(`"kind":"hook/verdict","payload":{"point":"turn_stopping","verdict":"reject","reason":"예산 초과"}`),
		"subagent/done":                 envelope(`"kind":"subagent/done","payload":{"status":"ok","result":"완료 요약"}`),
		"policy/decision":               envelope(`"kind":"policy/decision","payload":{"decision":"deny","profile_id":"opaque-default","reason":"egress 미허용"}`),
		"collector/fs_changed":          `{"seq":9,"ts":1,"trace_id":` + trace + `,"span_id":` + span + `,"actor":"collector","kind":"collector/fs_changed","payload":{"changes":[{"path":"a/b.txt","hash":"sha256:` + hex64 + `","change_type":"modified"}]}}`,
		"collector/egress":              `{"seq":10,"ts":1,"trace_id":` + trace + `,"span_id":` + span + `,"actor":"collector","kind":"collector/egress","payload":{"domain":"registry.npmjs.org","method":"GET","size_bytes":1024,"at_ms":1700000000001}}`,
		"int64 최대값":                     envelope(`"kind":"session/end","payload":{},"usage_in":9223372036854775807`),
	}
	for name, sample := range cases {
		if err := v.ValidateRecord([]byte(sample)); err != nil {
			t.Errorf("%s: 유효 샘플이 거부됨: %v", name, err)
		}
	}
}

const hex64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestValidateRecordInvalid(t *testing.T) {
	v := newV(t)
	allZeroTrace := `"00000000000000000000000000000000"`
	allZeroSpan := `"0000000000000000"`
	cases := map[string]string{
		// OTel all-zero ID 거부 — [H] 리뷰가 지정한 영구 테스트 대상
		"all-zero trace_id": `{"seq":1,"ts":1,"trace_id":` + allZeroTrace + `,"span_id":` + span + `,"actor":"parent","kind":"session/start","payload":{}}`,
		"all-zero span_id":  `{"seq":1,"ts":1,"trace_id":` + trace + `,"span_id":` + allZeroSpan + `,"actor":"parent","kind":"session/start","payload":{}}`,
		"대문자 trace_id":      `{"seq":1,"ts":1,"trace_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","span_id":` + span + `,"actor":"parent","kind":"session/start","payload":{}}`,
		"짧은 span_id":        `{"seq":1,"ts":1,"trace_id":` + trace + `,"span_id":"22222","actor":"parent","kind":"session/start","payload":{}}`,

		"미지의 kind":    envelope(`"kind":"session/pause","payload":{}`),
		"actor 누락":    `{"seq":1,"ts":1,"trace_id":` + trace + `,"span_id":` + span + `,"kind":"session/start","payload":{}}`,
		"잘못된 actor":   `{"seq":1,"ts":1,"trace_id":` + trace + `,"span_id":` + span + `,"actor":"observer","kind":"session/start","payload":{}}`,
		"미지 필드":       envelope(`"kind":"session/start","payload":{},"extra":true`),
		"seq 0":       `{"seq":0,"ts":1,"trace_id":` + trace + `,"span_id":` + span + `,"actor":"parent","kind":"session/start","payload":{}}`,
		"int64 초과":    envelope(`"kind":"session/end","payload":{},"usage_in":9223372036854775808`),
		"raw 비base64": envelope(`"kind":"tool/result","payload":{"status":"ok","output":{}},"raw":"@@@@"`),
		// T5 확정 payload의 위반 샘플 (2026-08-17 [H] 승인 — 폐쇄화)
		"user/message text 누락":        envelope(`"kind":"user/message","payload":{}`),
		"user/message 여분 필드":          envelope(`"kind":"user/message","payload":{"text":"x","extra":1}`),
		"tool/call args 누락":           envelope(`"kind":"tool/call","payload":{"name":"bash"}`),
		"tool/call 빈 이름":              envelope(`"kind":"tool/call","payload":{"name":"","args":{}}`),
		"tool/result 미지 status":       envelope(`"kind":"tool/result","payload":{"status":"partial","output":{}}`),
		"tool/result ok에 output 누락":   envelope(`"kind":"tool/result","payload":{"status":"ok"}`),
		"tool/result 비객체 output":      envelope(`"kind":"tool/result","payload":{"status":"ok","output":"스칼라"}`),
		"turn/start 비어 있지 않은 payload": envelope(`"kind":"turn/start","payload":{"note":"x"}`),
		// T9 확정 subagent payload의 위반 샘플 (2026-08-18 [H] 승인)
		"ready grade 누락":                  envelope(`"kind":"subagent/ready","payload":{"model":"m"}`),
		"ready 미지 grade":                  envelope(`"kind":"subagent/ready","payload":{"grade":"semi"}`),
		"tool_call call_id 누락":            envelope(`"kind":"subagent/tool_call","payload":{"name":"Bash","args":{}}`),
		"tool_call args 누락":               envelope(`"kind":"subagent/tool_call","payload":{"call_id":"t1","name":"Bash"}`),
		"tool_result status 누락":           envelope(`"kind":"subagent/tool_result","payload":{"call_id":"t1","output":{}}`),
		"tool_result call_id 누락":          envelope(`"kind":"subagent/tool_result","payload":{"status":"ok","output":{}}`),
		"tool_result ok에 output 없음":       envelope(`"kind":"subagent/tool_result","payload":{"call_id":"t1","status":"ok"}`),
		"tool_result ok에 error 동시":        envelope(`"kind":"subagent/tool_result","payload":{"call_id":"t1","status":"ok","output":{},"error":"실패"}`),
		"tool_result ok에 reason 동시":       envelope(`"kind":"subagent/tool_result","payload":{"call_id":"t1","status":"ok","output":{},"reason":"거부"}`),
		"tool_result error에 output 동시":    envelope(`"kind":"subagent/tool_result","payload":{"call_id":"t1","status":"error","error":"실패","output":{}}`),
		"tool_result rejected에 output 동시": envelope(`"kind":"subagent/tool_result","payload":{"call_id":"t1","status":"rejected","reason":"거부","output":{}}`),
		"tool_result rejected 빈 사유":       envelope(`"kind":"subagent/tool_result","payload":{"call_id":"t1","status":"rejected","reason":""}`),
		"approval_request call_id 누락":     envelope(`"kind":"subagent/approval_request","payload":{"request_id":"r1","name":"W","args":{}}`),
		"fork 원본참조 누락":                    envelope(`"kind":"session/fork","payload":{"origin_trace_id":` + trace + `}`),
		"rewrite에 대체값 없음":                 envelope(`"kind":"hook/verdict","payload":{"point":"pre_tool","verdict":"rewrite","reason":"x"}`),
		"reject에 빈 사유":                    envelope(`"kind":"hook/verdict","payload":{"point":"pre_tool","verdict":"reject","reason":""}`),
		"continue에 대체값":                   envelope(`"kind":"hook/verdict","payload":{"point":"pre_step","verdict":"continue","rewrite":{}}`),
		"done result 누락":                  envelope(`"kind":"subagent/done","payload":{"status":"ok"}`),
		"fs_changed 잘못된 해시":               `{"seq":9,"ts":1,"trace_id":` + trace + `,"span_id":` + span + `,"actor":"collector","kind":"collector/fs_changed","payload":{"changes":[{"path":"a","hash":"md5:abc","change_type":"added"}]}}`,
	}
	for name, sample := range cases {
		if err := v.ValidateRecord([]byte(sample)); err == nil {
			t.Errorf("%s: 위반 샘플이 통과함", name)
		}
	}
}

func TestValidateCommand(t *testing.T) {
	v := newV(t)
	valid := map[string]string{
		"task":                    `{"v":1,"cmd":"task","payload":{"instruction":"fix bug","workspace":"/ws","budget":{"tokens":100000,"time_ms":600000,"max_depth":2},"depth":0}}`,
		"task+extensions":         `{"v":1,"cmd":"task","payload":{"instruction":"x","workspace":"/ws","budget":{"tokens":1,"time_ms":1,"max_depth":1},"depth":1,"extensions":[{"name":"mcp-fs","version":"1.2.3","integrity":"sha256:` + hex64 + `","source":"registry.npmjs.org","egress":["api.example.com"]}]}}`,
		"message":                 `{"v":1,"cmd":"message","payload":{"text":"추가 지시"}}`,
		"stop":                    `{"v":1,"cmd":"stop","payload":{"reason":"budget_exceeded"}}`,
		"approval_response allow": `{"v":1,"cmd":"approval_response","payload":{"request_id":"r1","decision":"allow"}}`,
		"approval_response deny":  `{"v":1,"cmd":"approval_response","payload":{"request_id":"r1","decision":"deny","reason":"정책 위반"}}`,
	}
	for name, s := range valid {
		if err := v.ValidateCommand([]byte(s)); err != nil {
			t.Errorf("%s: 유효 샘플이 거부됨: %v", name, err)
		}
	}
	invalid := map[string]string{
		"v 불일치":                           `{"v":2,"cmd":"message","payload":{"text":"x"}}`,
		"미지 cmd":                          `{"v":1,"cmd":"pause","payload":{}}`,
		"budget 축 누락":                     `{"v":1,"cmd":"task","payload":{"instruction":"x","workspace":"/ws","budget":{"tokens":1,"max_depth":1},"depth":0}}`,
		"budget 자체 누락":                    `{"v":1,"cmd":"task","payload":{"instruction":"x","workspace":"/ws","depth":0}}`,
		"extension 해시 누락":                 `{"v":1,"cmd":"task","payload":{"instruction":"x","workspace":"/ws","budget":{"tokens":1,"time_ms":1,"max_depth":1},"depth":0,"extensions":[{"name":"a","version":"1.0.0","source":"r"}]}}`,
		"stop 미지 사유":                      `{"v":1,"cmd":"stop","payload":{"reason":"tired"}}`,
		"deny인데 reason 없음":                `{"v":1,"cmd":"approval_response","payload":{"request_id":"r1","decision":"deny"}}`,
		"deny인데 빈 reason":                 `{"v":1,"cmd":"approval_response","payload":{"request_id":"r1","decision":"deny","reason":""}}`,
		"approval_response 미지 decision":   `{"v":1,"cmd":"approval_response","payload":{"request_id":"r1","decision":"maybe"}}`,
		"approval_response request_id 누락": `{"v":1,"cmd":"approval_response","payload":{"decision":"allow"}}`,
	}
	for name, s := range invalid {
		if err := v.ValidateCommand([]byte(s)); err == nil {
			t.Errorf("%s: 위반 샘플이 통과함", name)
		}
	}
}

func TestValidateEvent(t *testing.T) {
	v := newV(t)
	valid := map[string]string{
		// 합성 이벤트는 빈 base64("")를 명시한다 — [H] 승인 규칙
		"ready 합성 raw": `{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}`,
		"done":         `{"v":1,"kind":"subagent/done","payload":{"status":"error","result":"실패 요약"},"raw":"eyJ4IjoxfQ=="}`,
		"usage":        `{"v":1,"kind":"subagent/usage","payload":{"input_tokens":100,"output_tokens":50},"raw":""}`,
	}
	for name, s := range valid {
		if err := v.ValidateEvent([]byte(s)); err != nil {
			t.Errorf("%s: 유효 샘플이 거부됨: %v", name, err)
		}
	}
	invalid := map[string]string{
		"raw 누락 (FR-ADP-04)": `{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"}}`,
		"usage 한쪽 토큰 누락":     `{"v":1,"kind":"subagent/usage","payload":{"input_tokens":100},"raw":""}`,
		"어댑터가 못 내는 kind":     `{"v":1,"kind":"subagent/spawn","payload":{},"raw":""}`,
		"done status 미지값":    `{"v":1,"kind":"subagent/done","payload":{"status":"partial","result":"x"},"raw":""}`,
		"raw 비base64":        `{"v":1,"kind":"subagent/message","payload":{},"raw":"not base64!"}`,
	}
	for name, s := range invalid {
		if err := v.ValidateEvent([]byte(s)); err == nil {
			t.Errorf("%s: 위반 샘플이 통과함", name)
		}
	}
}

// T1 완료 기준 (a): §5.1 예시 이벤트가 codegen 산출 타입으로 파싱되고,
// 재직렬화 결과도 스키마를 통과한다(타입 ↔ 스키마 정합).
func TestGenTypesRoundTrip(t *testing.T) {
	v := newV(t)
	sample := envelope(`"kind":"subagent/done","payload":{"status":"ok","result":"완료"},"raw":"aGk=","usage_in":5,"usage_out":7`)

	var rec gen.EventRecord
	if err := json.Unmarshal([]byte(sample), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Kind != gen.KindSubagentDone || rec.Seq != 1 {
		t.Fatalf("파싱 결과 이상: %+v", rec)
	}
	var done gen.SubagentDonePayload
	if err := json.Unmarshal(rec.Payload, &done); err != nil {
		t.Fatal(err)
	}
	if done.Status != gen.SubagentDonePayloadStatusOk || done.Result != "완료" {
		t.Fatalf("payload 파싱 결과 이상: %+v", done)
	}

	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ValidateRecord(out); err != nil {
		t.Errorf("생성 타입 재직렬화가 스키마를 위반: %v", err)
	}

	var cmd gen.Command
	cmdLine := `{"v":1,"cmd":"stop","payload":{"reason":"policy"}}`
	if err := json.Unmarshal([]byte(cmdLine), &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Cmd != gen.CommandCmdStop {
		t.Fatalf("cmd 파싱 이상: %+v", cmd)
	}
	out, err = json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ValidateCommand(out); err != nil {
		t.Errorf("생성 타입 재직렬화가 스키마를 위반: %v", err)
	}
}
