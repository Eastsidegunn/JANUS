// Package claudecode는 Claude Code의 stream-json 출력을 §5.2 정규화 이벤트로
// 변환한다 (T9, FR-ADP-03/04/05/06/07).
//
// 이 파일은 순수 변환기다 — 프로세스·소켓·네트워크를 모르며, 입력은 NDJSON
// 한 줄들이고 출력은 정규화 이벤트 목록이다. 계약은
// docs/t9-adapter-contract-proposal.md(2026-08-18 [H] 승인)에서 왔다.
package claudecode

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sync/atomic"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// MaxLineBytes는 한 줄의 상한이다. 64KiB는 bufio.Scanner의 기본값일 뿐
// 계약이 아니므로(유효한 대형 tool result는 흔하다) 그보다 크게 잡고,
// 이 상한을 넘는 줄만 fail-closed로 거부한다(제안서 §8.2).
const MaxLineBytes = 4 << 20

// Event는 어댑터가 방출하는 정규화 이벤트 하나다 (§5.2 event).
type Event struct {
	Kind    gen.EventKind
	Payload json.RawMessage
	// Raw는 이 이벤트를 낳은 원본 NDJSON 한 줄(개행 제외)의 바이트다.
	// 한 원본이 여러 이벤트를 만들면 같은 raw가 각각 붙는다 (제안서 §4).
	Raw []byte
}

// Parser는 스트림 상태를 들고 줄 단위로 변환한다. 한 세션에 하나.
type Parser struct {
	sawInit bool
	// rejectedEmitted는 system/permission_denied로 rejected를 이미 방출한
	// call_id다. 후속 user.tool_result는 확인용 중복으로 소비한다(제안서 §3.3).
	rejectedEmitted map[string]bool
	// stopRequested는 코어가 stop 명령을 보냈음을 뜻한다 — done 매핑의
	// 1순위 근거(제안서 §8.3).
	stopRequested atomic.Bool
	done          bool
	// disposition은 직전 ParseLine이 이벤트를 만들지 않은 경우의 사유다.
	// 골든에 기록해 "의도적 무시"와 "조용한 누락"을 구분한다(제안서 §8.1).
	disposition string
}

// NewParser는 빈 상태의 변환기를 만든다.
func NewParser() *Parser {
	return &Parser{rejectedEmitted: map[string]bool{}}
}

// NoteStop은 코어가 stop 명령을 보냈음을 기록한다.
func (p *Parser) NoteStop() { p.stopRequested.Store(true) }

// StopRequested reports whether the core sent stop. It is safe to call from
// the command goroutine while the native stream is being drained.
func (p *Parser) StopRequested() bool { return p.stopRequested.Load() }

// Ready reports whether system/init produced subagent/ready. It is read after
// native drain completion when a missing result must be synthesized.
func (p *Parser) Ready() bool { return p.sawInit }

// Done은 subagent/done을 이미 방출했는지 여부다.
func (p *Parser) Done() bool { return p.done }

// Disposition은 직전 ParseLine이 이벤트를 만들지 않은 사유다(만들었으면 "").
func (p *Parser) Disposition() string { return p.disposition }

// nativeLine은 stream-json 한 줄의 공통 단면이다.
type nativeLine struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Message json.RawMessage `json:"message"`

	// system/init
	Model string   `json:"model"`
	Tools []string `json:"tools"`

	SessionID string `json:"session_id"`

	// system/permission_denied
	ToolUseID      string `json:"tool_use_id"`
	DecisionReason string `json:"decision_reason"`

	// user
	ToolResultMeta []struct {
		ID               string `json:"id"`
		NonExecutionKind string `json:"non_execution_kind"`
	} `json:"tool_result_meta"`

	// result
	IsError        bool         `json:"is_error"`
	TerminalReason string       `json:"terminal_reason"`
	Result         *string      `json:"result"`
	Usage          *nativeUsage `json:"usage"`
}

type nativeUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
}

type nativeMessage struct {
	Content []nativeBlock `json:"content"`
}

type nativeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ignoredTypes는 우리가 쓰는 플래그에서 도달 가능하며 정규화 어휘에 대응이
// 없는 이벤트다 (제안서 §3.4). 화이트리스트 밖은 전부 오류다.
var ignoredTypes = map[string]bool{
	"rate_limit_event": true,
}

var ignoredSystemSubtypes = map[string]bool{
	"thinking_tokens": true,
	"api_retry":       true,
}

// isolationViolationTypes는 우리 플래그에서 나타나면 안 되는 이벤트다 —
// 조용히 무시하면 격리 실패를 가린다 (제안서 §3.4).
var isolationViolationTypes = map[string]bool{
	"hook_started":  true,
	"hook_progress": true,
	"hook_response": true,
}

var isolationViolationSystemSubtypes = map[string]bool{
	"plugin_install": true,
}

// ParseLine은 원본 한 줄을 0개 이상의 정규화 이벤트로 변환한다.
func (p *Parser) ParseLine(line []byte) ([]Event, error) {
	if len(line) == 0 {
		return nil, fmt.Errorf("claudecode: 빈 줄 — §5.2 위반")
	}
	if len(line) > MaxLineBytes {
		return nil, fmt.Errorf("claudecode: 줄이 상한 초과 (%d > %d bytes)", len(line), MaxLineBytes)
	}
	if p.done {
		return nil, fmt.Errorf("claudecode: done 이후 출력 — §5.2 시퀀스 위반")
	}
	p.disposition = ""
	var n nativeLine
	if err := json.Unmarshal(line, &n); err != nil {
		return nil, fmt.Errorf("claudecode: JSON 파싱: %w", err)
	}
	if !p.sawInit && !(n.Type == "system" && n.Subtype == "init") {
		return nil, fmt.Errorf("claudecode: 첫 native 줄은 system/init이어야 함 (got type=%q subtype=%q)", n.Type, n.Subtype)
	}
	if isolationViolationTypes[n.Type] {
		return nil, fmt.Errorf("claudecode: %s 출현 — 격리 계약 위반(자동 훅 발견이 차단돼야 함)", n.Type)
	}
	if n.Type == "system" && isolationViolationSystemSubtypes[n.Subtype] {
		return nil, fmt.Errorf("claudecode: system/%s 출현 — 격리 계약 위반(플러그인 발견이 차단돼야 함)", n.Subtype)
	}
	if ignoredTypes[n.Type] {
		p.disposition = "ignored:" + n.Type
		return nil, nil
	}

	switch n.Type {
	case "system":
		return p.parseSystem(n, line)
	case "assistant":
		return p.parseAssistant(n, line)
	case "user":
		return p.parseUser(n, line)
	case "result":
		return p.parseResult(n, line)
	}
	return nil, fmt.Errorf("claudecode: 미지의 네이티브 이벤트 type=%q subtype=%q", n.Type, n.Subtype)
}

func (p *Parser) parseSystem(n nativeLine, line []byte) ([]Event, error) {
	switch {
	case n.Subtype == "init":
		if p.sawInit {
			return nil, fmt.Errorf("claudecode: system/init 중복 — 세션당 1회여야 함")
		}
		p.sawInit = true
		return p.emit(gen.EventKindSubagentReady, gen.ReadyPayload{
			Grade:           gen.ReadyPayloadGradeObservable,
			NativeSessionID: strPtr(n.SessionID),
			Model:           strPtr(n.Model),
			Tools:           n.Tools,
		}, line)
	case n.Subtype == "permission_denied":
		if n.ToolUseID == "" {
			return nil, fmt.Errorf("claudecode: permission_denied에 tool_use_id 없음")
		}
		reason := n.DecisionReason
		if reason == "" {
			reason = "권한 거부(사유 미보고)"
		}
		p.rejectedEmitted[n.ToolUseID] = true
		return p.emit(gen.EventKindSubagentToolResult, gen.AgentToolResultPayload{
			CallID: n.ToolUseID,
			Status: gen.AgentToolResultPayloadStatusRejected,
			Reason: &reason,
		}, line)
	case ignoredSystemSubtypes[n.Subtype]:
		p.disposition = "ignored:system/" + n.Subtype
		return nil, nil
	}
	return nil, fmt.Errorf("claudecode: 미지의 system subtype=%q", n.Subtype)
}

func (p *Parser) parseAssistant(n nativeLine, line []byte) ([]Event, error) {
	blocks, err := decodeBlocks(n.Message)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			ev, err := p.emit(gen.EventKindSubagentMessage, gen.AgentMessagePayload{Text: b.Text}, line)
			if err != nil {
				return nil, err
			}
			out = append(out, ev...)
		case "tool_use":
			if b.ID == "" || b.Name == "" {
				return nil, fmt.Errorf("claudecode: tool_use에 id 또는 name 없음")
			}
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			ev, err := p.emit(gen.EventKindSubagentToolCall, gen.AgentToolCallPayload{
				CallID: b.ID, Name: b.Name, Args: args,
			}, line)
			if err != nil {
				return nil, err
			}
			out = append(out, ev...)
		case "thinking":
			p.disposition = "ignored:assistant/thinking"
		default:
			return nil, fmt.Errorf("claudecode: 미지의 assistant content block type=%q", b.Type)
		}
	}
	return out, nil
}

func (p *Parser) parseUser(n nativeLine, line []byte) ([]Event, error) {
	blocks, err := decodeBlocks(n.Message)
	if err != nil {
		return nil, err
	}
	rejectedIDs := map[string]bool{}
	for _, m := range n.ToolResultMeta {
		if m.NonExecutionKind == "user-rejected" {
			rejectedIDs[m.ID] = true
		}
	}
	var out []Event
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			if b.ToolUseID == "" {
				return nil, fmt.Errorf("claudecode: tool_result에 tool_use_id 없음")
			}
			if p.rejectedEmitted[b.ToolUseID] {
				// 이미 permission_denied에서 rejected를 방출했다.
				if !rejectedIDs[b.ToolUseID] {
					return nil, fmt.Errorf("claudecode: call_id %s는 거부됐는데 user-rejected가 아닌 결과가 도착", b.ToolUseID)
				}
				delete(p.rejectedEmitted, b.ToolUseID)
				p.disposition = "consumed:rejection-confirmation"
				continue // 확인용 중복 통보 — 미방출
			}
			payload, err := toolResultPayload(b, rejectedIDs[b.ToolUseID])
			if err != nil {
				return nil, err
			}
			ev, err := p.emit(gen.EventKindSubagentToolResult, payload, line)
			if err != nil {
				return nil, err
			}
			out = append(out, ev...)
		case "text":
			// 중단 통보 등 사용자 측 텍스트 — 모델 응답이 아니므로 무시
			p.disposition = "ignored:user/text"
		default:
			return nil, fmt.Errorf("claudecode: 미지의 user content block type=%q", b.Type)
		}
	}
	return out, nil
}

func toolResultPayload(b nativeBlock, rejected bool) (gen.AgentToolResultPayload, error) {
	p := gen.AgentToolResultPayload{CallID: b.ToolUseID}
	text := contentText(b.Content)
	switch {
	case rejected:
		if text == "" {
			text = "승인 거부"
		}
		p.Status = gen.AgentToolResultPayloadStatusRejected
		p.Reason = &text
	case b.IsError:
		if text == "" {
			text = "툴 오류(내용 없음)"
		}
		p.Status = gen.AgentToolResultPayloadStatusError
		p.Error = &text
	default:
		p.Status = gen.AgentToolResultPayloadStatusOk
		p.Output = normalizeOutput(b.Content)
	}
	return p, nil
}

func (p *Parser) parseResult(n nativeLine, line []byte) ([]Event, error) {
	var out []Event
	for id := range p.rejectedEmitted {
		return nil, fmt.Errorf("claudecode: call_id %s의 거부 확인 통보 없이 result 도달", id)
	}
	if n.Usage != nil {
		usage, err := normalizeUsage(*n.Usage)
		if err != nil {
			return nil, err
		}
		ev, err := p.emit(gen.EventKindSubagentUsage, usage, line)
		if err != nil {
			return nil, err
		}
		out = append(out, ev...)
	}
	done := gen.DonePayload{Status: doneStatus(n, p.stopRequested.Load()), Result: resultText(n)}
	ev, err := p.emit(gen.EventKindSubagentDone, done, line)
	if err != nil {
		return nil, err
	}
	p.done = true
	return append(out, ev...), nil
}

// doneStatus는 result → done 매핑이다 (제안서 §8.3).
func doneStatus(n nativeLine, stopRequested bool) gen.DonePayloadStatus {
	if stopRequested {
		return gen.DonePayloadStatusStopped
	}
	if n.Subtype == "success" {
		return gen.DonePayloadStatusOk
	}
	if n.TerminalReason == "aborted_streaming" || n.TerminalReason == "aborted" {
		return gen.DonePayloadStatusStopped
	}
	return gen.DonePayloadStatusError
}

// resultText는 결과 문자열이 없을 때 결정적 문구를 만든다 (골든 안정성).
func resultText(n nativeLine) string {
	if n.Result != nil {
		return *n.Result
	}
	tr := n.TerminalReason
	if tr == "" {
		tr = "none"
	}
	return fmt.Sprintf("(결과 없음: subtype=%s, terminal_reason=%s)", n.Subtype, tr)
}

// normalizeUsage는 입력 3항을 checked addition으로 합산한다 (제안서 §5.2).
func normalizeUsage(u nativeUsage) (gen.UsagePayload, error) {
	if u.InputTokens == nil || u.OutputTokens == nil {
		return gen.UsagePayload{}, fmt.Errorf("claudecode: usage 핵심값 누락 (input_tokens/output_tokens)")
	}
	in := *u.InputTokens
	total := in
	for _, aux := range []*int64{u.CacheCreationInputTokens, u.CacheReadInputTokens} {
		v := int64(0)
		if aux != nil {
			v = *aux // 캐시 보조값 부재는 0 (제안서 §5.2)
		}
		if v < 0 {
			return gen.UsagePayload{}, fmt.Errorf("claudecode: 음수 usage %d", v)
		}
		if total > math.MaxInt64-v {
			return gen.UsagePayload{}, fmt.Errorf("claudecode: usage 합산 int64 overflow")
		}
		total += v
	}
	out := *u.OutputTokens
	if in < 0 || out < 0 {
		return gen.UsagePayload{}, fmt.Errorf("claudecode: 음수 usage (in=%d out=%d)", in, out)
	}
	return gen.UsagePayload{InputTokens: total, OutputTokens: out}, nil
}

func (p *Parser) emit(kind gen.EventKind, payload any, line []byte) ([]Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, len(line))
	copy(raw, line)
	return []Event{{Kind: kind, Payload: b, Raw: raw}}, nil
}

func decodeBlocks(msg json.RawMessage) ([]nativeBlock, error) {
	if len(msg) == 0 {
		return nil, nil
	}
	var m nativeMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return nil, fmt.Errorf("claudecode: message 파싱: %w", err)
	}
	return m.Content, nil
}

// contentText는 tool_result content(문자열 또는 블록 배열)에서 텍스트를 뽑는다.
func contentText(c json.RawMessage) string {
	if len(c) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s
	}
	var blocks []nativeBlock
	if json.Unmarshal(c, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return string(c)
}

// normalizeOutput은 객체가 아닌 출력을 {"value": …}로 감싼다 (T5와 동일 규칙).
func normalizeOutput(c json.RawMessage) json.RawMessage {
	if len(c) > 0 && c[0] == '{' {
		return c
	}
	v := c
	if len(v) == 0 {
		v = json.RawMessage(`null`)
	}
	wrapped, _ := json.Marshal(map[string]json.RawMessage{"value": v})
	return wrapped
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// RawB64는 raw 바이트를 §5.2의 base64 문자열로 만든다.
func RawB64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }
