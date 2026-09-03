// Package codex converts the recorded Codex exec --json stream into the §5.2
// normalized subagent events. It is a pure parser: process, approval sockets,
// and workspace effects belong to the surrounding seams.
//
// Codex's non-interactive stream has no approval event in the T8 recordings.
// Consequently the zero-value parser is manual and rejects an effect-bearing
// item unless the caller explicitly supplies policy.ApprovalAuto from a
// profile. It never infers approval from a successful native command.
package codex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

// MaxLineBytes is the native line limit. Scanner's initial buffer remains a
// separate concern; valid lines over 64 KiB are accepted up to this bound.
const MaxLineBytes = 4 << 20

// ErrApprovalUnavailable is returned when a manual profile encounters a
// Codex effect but the native stream has no approval path to correlate.
var ErrApprovalUnavailable = errors.New("codex: 승인 경로 미관측 — manual 프로파일에서 실행을 허용하지 않음")

// Event is one normalized §5.2 event. Raw is the exact native line without
// its trailing newline; a synthesized event has nil Raw and therefore an
// empty wire raw field.
type Event struct {
	Kind    gen.EventKind
	Payload json.RawMessage
	Raw     []byte
}

// Parser holds one Codex stream's state. Construct a parser with
// NewParser(policy.ApprovalAuto) only when the effective profile explicitly
// selected auto; an omitted mode is manual and fail-closed.
type Parser struct {
	approval    policy.ApprovalMode
	thread      bool
	done        bool
	failure     error
	lastMsg     string
	pending     map[string]string // item id -> native item type
	disposition string
}

// NewParser creates a parser. At most one mode is meaningful; extra values
// are rejected by treating the first as the profile's effective mode.
func NewParser(mode ...policy.ApprovalMode) *Parser {
	approval := policy.ApprovalManual
	if len(mode) != 0 && mode[0] == policy.ApprovalAuto {
		approval = policy.ApprovalAuto
	}
	return &Parser{approval: approval, pending: map[string]string{}}
}

// Disposition explains a line that intentionally emitted no event.
func (p *Parser) Disposition() string { return p.disposition }

// Ready reports whether thread.started produced subagent/ready.
func (p *Parser) Ready() bool { return p.thread }

// Done reports whether a terminal subagent/done was emitted.
func (p *Parser) Done() bool { return p.done }

type nativeEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     json.RawMessage `json:"item"`
	Usage    *nativeUsage    `json:"usage"`
}

type nativeUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

type nativeItem struct {
	ID          string       `json:"id"`
	Type        string       `json:"type"`
	Text        string       `json:"text"`
	Command     string       `json:"command"`
	Description string       `json:"description"`
	Aggregated  string       `json:"aggregated_output"`
	ExitCode    *int64       `json:"exit_code"`
	Status      string       `json:"status"`
	Changes     []fileChange `json:"changes"`
}

type fileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// ParseLine converts one native Codex JSON line. Once a post-ready failure is
// observed, Finish returns a synthetic done/error event carrying its reason;
// pre-ready failures deliberately produce no event, so startup failure stays
// attributable to the adapter rather than to a violated ready contract.
func (p *Parser) ParseLine(line []byte) ([]Event, error) {
	if len(line) == 0 {
		return p.fail(fmt.Errorf("codex: 빈 줄 — §5.2 위반"))
	}
	if len(line) > MaxLineBytes {
		return p.fail(fmt.Errorf("codex: 줄이 상한 초과 (%d > %d bytes)", len(line), MaxLineBytes))
	}
	if p.done {
		return nil, fmt.Errorf("codex: done 이후 출력 — §5.2 시퀀스 위반")
	}
	if p.failure != nil {
		return nil, p.failure
	}
	p.disposition = ""
	var event nativeEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return p.fail(fmt.Errorf("codex: JSON 파싱: %w", err))
	}
	switch event.Type {
	case "thread.started":
		if p.thread {
			return p.fail(fmt.Errorf("codex: thread.started 중복"))
		}
		if event.ThreadID == "" {
			return p.fail(fmt.Errorf("codex: thread.started에 thread_id 없음"))
		}
		p.thread = true
		return p.emit(gen.EventKindSubagentReady, gen.ReadyPayload{
			Grade:           gen.ReadyPayloadGradeObservable,
			NativeSessionID: strPtr(event.ThreadID),
		}, line)
	case "turn.started":
		if !p.thread {
			return p.fail(fmt.Errorf("codex: thread.started보다 먼저 turn.started가 옴"))
		}
		p.disposition = "ignored:turn.started"
		return nil, nil
	case "item.started", "item.completed":
		if !p.thread {
			return p.fail(fmt.Errorf("codex: thread.started보다 먼저 %s가 옴", event.Type))
		}
		return p.parseItem(event.Type, event.Item, line)
	case "turn.completed":
		if !p.thread {
			return p.fail(fmt.Errorf("codex: thread.started보다 먼저 turn.completed가 옴"))
		}
		return p.parseTurnCompleted(event.Usage, line)
	default:
		return p.fail(fmt.Errorf("codex: 미지의 네이티브 이벤트 type=%q", event.Type))
	}
}

func (p *Parser) parseItem(eventType string, raw json.RawMessage, line []byte) ([]Event, error) {
	var item nativeItem
	if len(raw) == 0 || json.Unmarshal(raw, &item) != nil {
		return p.fail(fmt.Errorf("codex: %s item 파싱 실패", eventType))
	}
	if item.ID == "" {
		return p.fail(fmt.Errorf("codex: %s item id 없음", eventType))
	}
	switch item.Type {
	case "agent_message":
		if eventType != "item.completed" {
			return p.fail(fmt.Errorf("codex: agent_message는 completed만 허용"))
		}
		p.lastMsg = item.Text
		return p.emit(gen.EventKindSubagentMessage, gen.AgentMessagePayload{Text: item.Text}, line)
	case "command_execution":
		return p.parseCommandItem(eventType, item, line)
	case "file_change":
		return p.parseFileChangeItem(eventType, item, line)
	default:
		return p.fail(fmt.Errorf("codex: 미지의 item type=%q", item.Type))
	}
}

func (p *Parser) parseCommandItem(eventType string, item nativeItem, line []byte) ([]Event, error) {
	if p.approval != policy.ApprovalAuto {
		return p.fail(fmt.Errorf("%w: command_execution %s", ErrApprovalUnavailable, item.ID))
	}
	switch eventType {
	case "item.started":
		if item.Status != "in_progress" {
			return p.fail(fmt.Errorf("codex: command_execution %s 시작 상태=%q", item.ID, item.Status))
		}
		if _, exists := p.pending[item.ID]; exists {
			return p.fail(fmt.Errorf("codex: command_execution %s 중복 시작", item.ID))
		}
		p.pending[item.ID] = item.Type
		args := map[string]string{"command": item.Command}
		if item.Description != "" {
			args["description"] = item.Description
		}
		return p.emit(gen.EventKindSubagentToolCall, gen.AgentToolCallPayload{
			CallID: item.ID, Name: "command_execution", Args: mustJSON(args),
		}, line)
	case "item.completed":
		if p.pending[item.ID] != item.Type {
			return p.fail(fmt.Errorf("codex: command_execution %s의 선행 시작 없음", item.ID))
		}
		delete(p.pending, item.ID)
		if item.Status != "completed" && item.Status != "failed" {
			return p.fail(fmt.Errorf("codex: command_execution %s 종료 상태=%q", item.ID, item.Status))
		}
		if item.Status == "completed" && (item.ExitCode == nil || *item.ExitCode != 0) {
			return p.fail(fmt.Errorf("codex: command_execution %s 성공 상태의 exit_code 위반", item.ID))
		}
		if item.Status == "failed" {
			reason := item.Aggregated
			if reason == "" {
				if item.ExitCode != nil {
					reason = fmt.Sprintf("command_execution failed (exit_code=%d)", *item.ExitCode)
				} else {
					reason = "command_execution failed"
				}
			}
			return p.emit(gen.EventKindSubagentToolResult, gen.AgentToolResultPayload{
				CallID: item.ID, Status: gen.AgentToolResultPayloadStatusError, Error: &reason,
			}, line)
		}
		return p.emit(gen.EventKindSubagentToolResult, gen.AgentToolResultPayload{
			CallID: item.ID, Status: gen.AgentToolResultPayloadStatusOk, Output: normalizeOutput(item.Aggregated),
		}, line)
	default:
		return nil, fmt.Errorf("codex: 미지의 item event=%q", eventType)
	}
}

func (p *Parser) parseFileChangeItem(eventType string, item nativeItem, line []byte) ([]Event, error) {
	if p.approval != policy.ApprovalAuto {
		return p.fail(fmt.Errorf("%w: file_change %s", ErrApprovalUnavailable, item.ID))
	}
	if item.Changes == nil {
		return p.fail(fmt.Errorf("codex: file_change %s changes 없음", item.ID))
	}
	switch eventType {
	case "item.started":
		if item.Status != "in_progress" {
			return p.fail(fmt.Errorf("codex: file_change %s 시작 상태=%q", item.ID, item.Status))
		}
		if _, exists := p.pending[item.ID]; exists {
			return p.fail(fmt.Errorf("codex: file_change %s 중복 시작", item.ID))
		}
		p.pending[item.ID] = item.Type
		args := mustJSON(map[string]any{"changes": item.Changes})
		return p.emit(gen.EventKindSubagentToolCall, gen.AgentToolCallPayload{CallID: item.ID, Name: "file_change", Args: args}, line)
	case "item.completed":
		if p.pending[item.ID] != item.Type {
			return p.fail(fmt.Errorf("codex: file_change %s의 선행 시작 없음", item.ID))
		}
		delete(p.pending, item.ID)
		if item.Status != "completed" {
			return p.fail(fmt.Errorf("codex: file_change %s 종료 상태=%q", item.ID, item.Status))
		}
		return p.emit(gen.EventKindSubagentToolResult, gen.AgentToolResultPayload{
			CallID: item.ID, Status: gen.AgentToolResultPayloadStatusOk,
			Output: mustJSON(map[string]any{"changes": item.Changes}),
		}, line)
	default:
		return nil, fmt.Errorf("codex: 미지의 item event=%q", eventType)
	}
}

func (p *Parser) parseTurnCompleted(usage *nativeUsage, line []byte) ([]Event, error) {
	if len(p.pending) != 0 {
		return p.fail(fmt.Errorf("codex: 미완료 item이 있는 turn.completed"))
	}
	var out []Event
	if usage != nil {
		if usage.InputTokens == nil || usage.OutputTokens == nil {
			return p.fail(fmt.Errorf("codex: usage 핵심값 누락 (input_tokens/output_tokens)"))
		}
		if *usage.InputTokens < 0 || *usage.OutputTokens < 0 {
			return p.fail(fmt.Errorf("codex: 음수 usage"))
		}
		events, err := p.emit(gen.EventKindSubagentUsage, gen.UsagePayload{
			InputTokens: *usage.InputTokens, OutputTokens: *usage.OutputTokens,
		}, line)
		if err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	result := p.lastMsg
	if result == "" {
		result = "(결과 없음: turn.completed)"
	}
	done, err := p.emit(gen.EventKindSubagentDone, gen.DonePayload{
		Status: gen.DonePayloadStatusOk, Result: result,
	}, line)
	if err != nil {
		return nil, err
	}
	p.done = true
	return append(out, done...), nil
}

// Finish closes a stream that ended without turn.completed. An in-progress
// command/file item is an interrupted execution and yields stopped; an empty
// or malformed post-ready tail yields a deterministic error done. In both
// cases the synthetic event has empty raw, because no native line produced it.
func (p *Parser) Finish() ([]Event, error) {
	if p.done {
		return nil, nil
	}
	if !p.thread {
		if p.failure != nil {
			return nil, p.failure
		}
		return nil, fmt.Errorf("codex: stream 종료 전 system 준비 이벤트 없음")
	}
	if p.failure != nil {
		event := p.syntheticDone(gen.DonePayloadStatusError, "(결과 없음: "+p.failure.Error()+")")
		p.done = true
		return []Event{event}, p.failure
	}
	if len(p.pending) != 0 {
		event := p.syntheticDone(gen.DonePayloadStatusStopped, "(결과 없음: interrupted command_execution)")
		p.done = true
		return []Event{event}, nil
	}
	event := p.syntheticDone(gen.DonePayloadStatusError, "(결과 없음: stream ended before turn.completed)")
	p.done = true
	return []Event{event}, fmt.Errorf("codex: turn.completed 없이 stream 종료")
}

func (p *Parser) fail(err error) ([]Event, error) {
	if p.thread && p.failure == nil {
		p.failure = err
	}
	return nil, err
}

func (p *Parser) emit(kind gen.EventKind, payload any, line []byte) ([]Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw := append([]byte(nil), line...)
	return []Event{{Kind: kind, Payload: b, Raw: raw}}, nil
}

func (p *Parser) syntheticDone(status gen.DonePayloadStatus, result string) Event {
	p.disposition = ""
	b, _ := json.Marshal(gen.DonePayload{Status: status, Result: result})
	return Event{Kind: gen.EventKindSubagentDone, Payload: b}
}

func normalizeOutput(output string) json.RawMessage {
	return mustJSON(map[string]string{"value": output})
}

func mustJSON(value any) json.RawMessage {
	b, _ := json.Marshal(value)
	return b
}

func strPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// RawB64 converts a native line to the wire raw representation.
func RawB64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }
