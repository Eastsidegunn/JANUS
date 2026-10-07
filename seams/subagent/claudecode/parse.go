// Package claudecode는 Claude Code의 stream-json 출력을 §5.2 정규화 이벤트로
// 변환한다 (T9, FR-ADP-03/04/05/06/07).
//
// 이 파일은 순수 변환기다 — 프로세스·소켓·네트워크를 모르며, 입력은 NDJSON
// 한 줄들이고 출력은 정규화 이벤트 목록이다. 계약은
// docs/t9-adapter-contract-proposal.md(2026-08-18 명세 소유자 승인)에서 왔다.
package claudecode

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
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
	// terminalMu는 stop 접수(NoteStop)와 terminal done 상태 봉인(SealTerminal)을
	// 직렬화한다 (T27(d) stop 경합 결정성).
	terminalMu     sync.Mutex
	terminalSealed bool
	done           bool
	// disposition은 직전 ParseLine이 이벤트를 만들지 않은 경우의 사유다.
	// 골든에 기록해 "의도적 무시"와 "조용한 누락"을 구분한다(제안서 §8.1).
	disposition string

	// multiturn(SCP-T25-001)에서 native result는 세션 종료가 아니라 턴
	// 종료다. 턴 result는 usage만 방출하고 마지막 것을 보관했다가, 프로세스
	// 종료 시 TurnDone이 단 하나의 subagent/done으로 사영한다 — 신규 kind 없음.
	multiturn  bool
	sessionID  string
	turns      int
	lastResult *nativeLine
	lastRaw    []byte

	// apiErrorText는 마지막 합성 API 오류 메시지의 텍스트다. 이후 모델 가시
	// assistant 출력이 오면 비터미널(재시도 성공)로 보고 지운다. result나
	// 세션 종료 시점에 남아 있으면 터미널 오류로서 done 사유에 담긴다 (T27(a)).
	apiErrorText string
	// lastAPIError는 multiturn에서 마지막 턴 result에 귀속된 합성 오류다.
	lastAPIError string
}

// NewParser는 빈 상태의 변환기를 만든다.
func NewParser() *Parser {
	return &Parser{rejectedEmitted: map[string]bool{}}
}

// NewMultiturnParser는 다중 턴 세션용 변환기를 만든다. oneshot 매핑(골든)은
// NewParser가 그대로 소유한다.
func NewMultiturnParser() *Parser {
	p := NewParser()
	p.multiturn = true
	return p
}

// Turns는 multiturn에서 완료된(result를 받은) 턴 수다.
func (p *Parser) Turns() int { return p.turns }

// TurnDone은 multiturn 세션의 terminal done을 만든다: 마지막 턴 result를
// 현재 stop 상태로 매핑한다(doneStatus 동일 규칙). 완료된 턴이 없으면 nil —
// 호출자는 finishNative의 missing_result 합성으로 넘어간다.
func (p *Parser) TurnDone() (*Event, error) {
	if !p.multiturn || p.lastResult == nil || p.done {
		return nil, nil
	}
	done := terminalDone(*p.lastResult, p.lastAPIError, p.stopRequested.Load())
	ev, err := p.emit(gen.EventKindSubagentDone, done, p.lastRaw)
	if err != nil {
		return nil, err
	}
	p.done = true
	return &ev[0], nil
}

// NoteStop은 코어가 stop 명령을 보냈음을 기록한다. terminal done 상태가 이미
// 봉인됐으면(SealTerminal) 접수하지 않고 false를 돌려준다 — 봉인 이후의 stop은
// 이미 결정된 done을 바꿀 수 없기 때문이다(T19 판정표, T27(d)).
func (p *Parser) NoteStop() bool {
	p.terminalMu.Lock()
	defer p.terminalMu.Unlock()
	if p.terminalSealed {
		return false
	}
	p.stopRequested.Store(true)
	return true
}

// SealTerminal은 terminal done의 stop 판정을 단 한 번 고정하고 그 값을 돌려준다.
// NoteStop과 같은 잠금 아래에서 실행되므로 "stop 접수"와 "done 상태 결정"은
// 전순서를 가진다: 봉인 전에 접수된 stop은 반드시 stopped로 반영되고, 봉인
// 후의 stop은 접수되지 않는다. 어댑터는 done을 방출하기 직전에 이 값만 쓴다.
func (p *Parser) SealTerminal() bool {
	p.terminalMu.Lock()
	defer p.terminalMu.Unlock()
	p.terminalSealed = true
	return p.stopRequested.Load()
}

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

	// assistant 합성 API 오류 표식 (T27(a)). claude-code가 상류 오류를
	// assistant 메시지 형태로 합성할 때 붙인다.
	IsAPIErrorMessage bool `json:"is_api_error_message"`

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
	Model             string        `json:"model"`
	IsAPIErrorMessage bool          `json:"is_api_error_message"`
	Content           []nativeBlock `json:"content"`
}

// syntheticModel은 claude-code가 상류(API) 오류를 assistant 메시지로 합성할 때
// 쓰는 모델 표식이다. 이 메시지의 텍스트는 모델 응답이 아니다 (T27(a)).
const syntheticModel = "<synthetic>"

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
	if n.Type == "system" && ignoredSystemSubtypes[n.Subtype] {
		p.disposition = "ignored:system/" + n.Subtype
		return nil, nil
	}
	// init은 매핑 대상 이벤트보다 먼저 와야 한다 — 그래야 ready가 §5.2의 첫
	// 이벤트가 된다. 무시 대상(rate_limit_event, thinking_tokens, api_retry)은
	// 실제 세션에서 init보다 앞설 수 있으므로(smoke 실측: claude 2.1.235의 첫
	// 줄은 rate_limit_event) 이 검사 앞에서 걸러진다.
	if !p.sawInit && !(n.Type == "system" && n.Subtype == "init") {
		return nil, fmt.Errorf("claudecode: system/init보다 먼저 매핑 대상 이벤트가 옴 (type=%q subtype=%q)", n.Type, n.Subtype)
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
			// multiturn: 턴 경계 뒤의 init은 같은 native 세션의 재통보일 때만
			// 소비한다(ready는 세션당 1회). 턴 도중·다른 세션 id는 여전히 위반.
			if p.multiturn && p.turns > 0 && p.lastResult != nil && n.SessionID != "" && n.SessionID == p.sessionID {
				p.disposition = "consumed:turn-init"
				return nil, nil
			}
			return nil, fmt.Errorf("claudecode: system/init 중복 — 세션당 1회여야 함")
		}
		p.sawInit = true
		p.sessionID = n.SessionID
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
	}
	return nil, fmt.Errorf("claudecode: 미지의 system subtype=%q", n.Subtype)
}

func (p *Parser) parseAssistant(n nativeLine, line []byte) ([]Event, error) {
	if synthetic, text, err := syntheticAPIError(n); err != nil {
		return nil, err
	} else if synthetic {
		return p.parseSyntheticAPIError(text, line)
	}
	blocks, err := decodeBlocks(n.Message)
	if err != nil {
		return nil, err
	}
	// 모델 가시 출력이 이어졌다 — 앞선 합성 오류는 비터미널(재시도 성공)이었다.
	p.apiErrorText = ""
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

// syntheticAPIError는 assistant 줄이 claude-code의 합성 API 오류 메시지인지
// 판정하고, 그렇다면 text 블록을 이어붙인 오류 텍스트를 돌려준다. 표식은
// message.model=="<synthetic>" 또는 is_api_error_message:true(줄 또는 message
// 수준)다. 합성 메시지에 text 이외의 블록이 있으면 fail-closed다 — 합성
// 메시지가 툴 의도를 실어 나르는 경로를 조용히 만들지 않는다.
func syntheticAPIError(n nativeLine) (bool, string, error) {
	if len(n.Message) == 0 {
		return false, "", nil
	}
	var m nativeMessage
	if err := json.Unmarshal(n.Message, &m); err != nil {
		return false, "", fmt.Errorf("claudecode: message 파싱: %w", err)
	}
	if !n.IsAPIErrorMessage && !m.IsAPIErrorMessage && m.Model != syntheticModel {
		return false, "", nil
	}
	var parts []string
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		default:
			return false, "", fmt.Errorf("claudecode: 합성 API 오류 메시지에 text 외 블록 type=%q", b.Type)
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" {
		text = "(내용 없음)"
	}
	return true, text, nil
}

// parseSyntheticAPIError는 합성 API 오류를 모델 텍스트(subagent/message)로
// 방출하지 않는다. 원본 줄은 FR-LOG-07대로 보존돼야 하므로 기존 kind 중 텍스트를
// 싣지 않는 subagent/usage{0,0}에 raw로 실어 로그에 남긴다 — 합성 메시지는
// 모델 호출이 성립하지 않았다는 통보이므로 토큰 0은 사실 그대로다. 오류
// 텍스트는 보관했다가 터미널이면 done 사유로 사영한다(terminalDone,
// WithAPIErrorDone).
func (p *Parser) parseSyntheticAPIError(text string, line []byte) ([]Event, error) {
	p.apiErrorText = text
	return p.emit(gen.EventKindSubagentUsage, gen.UsagePayload{InputTokens: 0, OutputTokens: 0}, line)
}

// APIErrorText는 아직 해소되지 않은(이후 모델 가시 출력도 result도 없는) 합성
// API 오류 텍스트다. 세션이 result 없이 끝날 때 어댑터가 done 사유에 덧붙인다.
func (p *Parser) APIErrorText() string { return p.apiErrorText }

// withAPIError는 터미널 합성 오류를 done 사유 텍스트에 담는다. result 텍스트가
// 이미 그 내용을 포함하면(claude-code는 보통 같은 문구를 result로 반복한다)
// 그대로 둔다.
func withAPIError(result, apiErr string) string {
	if apiErr == "" || strings.Contains(result, apiErr) {
		return result
	}
	return result + " (합성 API 오류: " + apiErr + ")"
}

// WithAPIErrorDone은 result 없이 합성된 terminal done(missing_result)에 미해소
// 합성 API 오류를 사유로 덧붙인다. stopped는 그대로 두고, ok는 있을 수 없지만
// 방어적으로 error로 내린다.
func WithAPIErrorDone(ev Event, apiErr string) (Event, error) {
	if apiErr == "" || ev.Kind != gen.EventKindSubagentDone {
		return ev, nil
	}
	var payload gen.DonePayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return Event{}, err
	}
	if payload.Status == gen.DonePayloadStatusOk {
		payload.Status = gen.DonePayloadStatusError
	}
	payload.Result = withAPIError(payload.Result, apiErr)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	ev.Payload = encoded
	return ev, nil
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
	apiErr := p.apiErrorText
	p.apiErrorText = ""
	if p.multiturn {
		last := n
		p.lastResult = &last
		p.lastRaw = append([]byte(nil), line...)
		p.lastAPIError = apiErr
		p.turns++
		if len(out) == 0 {
			p.disposition = "consumed:turn-result"
		}
		return out, nil
	}
	done := terminalDone(n, apiErr, p.stopRequested.Load())
	ev, err := p.emit(gen.EventKindSubagentDone, done, line)
	if err != nil {
		return nil, err
	}
	p.done = true
	return append(out, ev...), nil
}

// terminalDone은 result(와 그 앞의 미해소 합성 API 오류)를 done으로 사영한다.
// 합성 오류 뒤에 모델 가시 출력 없이 result가 왔다면 그 턴의 결과는 모델
// 응답이 아니라 상류 오류다: stop이 아니면 status=error이고 사유에 오류
// 텍스트를 담는다 (T27(a)).
func terminalDone(n nativeLine, apiErr string, stopRequested bool) gen.DonePayload {
	status := doneStatus(n, stopRequested)
	if apiErr != "" && status == gen.DonePayloadStatusOk {
		status = gen.DonePayloadStatusError
	}
	return gen.DonePayload{Status: status, Result: withAPIError(resultText(n), apiErr)}
}

// doneStatus는 result → done 매핑이다 (제안서 §8.3).
func doneStatus(n nativeLine, stopRequested bool) gen.DonePayloadStatus {
	if stopRequested {
		return gen.DonePayloadStatusStopped
	}
	if n.Subtype == "success" {
		if n.IsError || authenticationFailure(resultText(n)) {
			return gen.DonePayloadStatusError
		}
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
