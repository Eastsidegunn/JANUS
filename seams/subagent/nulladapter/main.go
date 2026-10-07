// NullAdapter는 §5.2 와이어 프로토콜을 말하는 가짜 어댑터다 (T7 워킹
// 스켈레톤·테스트 전용 — Null 접두사 규칙). 실제 에이전트를 실행하지 않고
// 결정적인 이벤트 시퀀스를 방출한다: ready → message → tool_call →
// tool_result → usage → done(ok). stop 명령에는 done(stopped)으로 응답한다.
//
// FR-ADP-01: stdin/stdout NDJSON을 말하는 독립 실행 파일.
// FR-ADP-03: subagent/ready와 subagent/done은 MUST — 항상 방출한다.
// FR-ADP-04: 모든 이벤트에 raw(base64) 첨부 — 합성 이벤트는 "".
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

type command struct {
	V       int64           `json:"v"`
	Cmd     string          `json:"cmd"`
	Payload json.RawMessage `json:"payload"`
}

type taskPayload struct {
	Instruction string `json:"instruction"`
}

func emit(kind string, payload any, raw string) {
	p, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nulladapter: payload 직렬화:", err)
		os.Exit(2)
	}
	line, err := json.Marshal(map[string]any{
		"v": 1, "kind": kind, "payload": json.RawMessage(p), "raw": raw,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "nulladapter:", err)
		os.Exit(2)
	}
	os.Stdout.Write(append(line, '\n'))
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var cmd command
		if err := json.Unmarshal(scanner.Bytes(), &cmd); err != nil {
			fmt.Fprintln(os.Stderr, "nulladapter: 명령 파싱:", err)
			os.Exit(2)
		}
		switch cmd.Cmd {
		case "task":
			var task taskPayload
			if err := json.Unmarshal(cmd.Payload, &task); err != nil {
				fmt.Fprintln(os.Stderr, "nulladapter: task 파싱:", err)
				os.Exit(2)
			}
			emit("subagent/ready", map[string]any{"grade": "observable"}, "")
			emit("subagent/message", map[string]any{"text": "작업 시작: " + task.Instruction}, "")
			emit("subagent/tool_call", map[string]any{
				"call_id": "null-call-1", "name": "echo",
				"args": map[string]any{"text": task.Instruction},
			}, "")
			fakeRaw := base64.StdEncoding.EncodeToString([]byte(`{"native":"echo-output"}`))
			emit("subagent/tool_result", map[string]any{
				"call_id": "null-call-1", "status": "ok",
				"output": map[string]any{"stdout": task.Instruction},
			}, fakeRaw)
			emit("subagent/usage", map[string]any{"input_tokens": 12, "output_tokens": 34}, "")
			emit("subagent/done", map[string]any{"status": "ok", "result": "null 어댑터 완료: " + task.Instruction}, "")
			return
		case "message":
			emit("subagent/message", map[string]any{"text": "추가 입력 수신"}, "")
		case "stop":
			emit("subagent/done", map[string]any{"status": "stopped", "result": "중단됨"}, "")
			return
		default:
			fmt.Fprintf(os.Stderr, "nulladapter: 미지의 cmd %q\n", cmd.Cmd)
			os.Exit(2)
		}
	}
}
