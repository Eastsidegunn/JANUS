package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
)

// Model은 모델 seam의 계약이다 (구현은 seams/model).
type Model interface {
	// Complete는 모델 요청 1회다. 요청은 로그 프로젝션에서 재구성된
	// 모델 가시 히스토리다 — 로그에 없는 내용은 모델에 들어갈 수 없다
	// (FR-LOG-03의 구조적 강제).
	Complete(ctx context.Context, req ModelRequest) (ModelResponse, error)
}

// ModelRequest는 모델 요청이다. Messages는 logd의 프로젝션 그대로다.
type ModelRequest struct {
	Messages []logd.Message `json:"messages"`
}

// ModelResponse는 모델 응답 1회다.
type ModelResponse struct {
	Text      string     `json:"text"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	UsageIn   int64      `json:"usage_in"`
	UsageOut  int64      `json:"usage_out"`
}

// ToolCall은 모델이 요청한 툴 실행이다.
type ToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ToolResult는 툴 실행 결과다.
type ToolResult struct {
	Output json.RawMessage `json:"output"`
}

// Tools는 툴 seam의 계약이다 (구현은 seams/tools).
type Tools interface {
	Invoke(ctx context.Context, call ToolCall) (ToolResult, error)
}

// DefaultMaxSteps는 turn당 step 안전 상한이다. 예산 기반 중단(FR-POL-06)은
// T6의 몫이고, 이것은 turn_stopping 훅이 영원히 reject하는 류의 무한 루프
// 방지 장치다.
const DefaultMaxSteps = 32

// Loop는 한 세션의 turn/step 상태 머신이다 (FR-LOOP-01 — 교체 불가).
// 모든 경계·판정·대화는 writer를 경유해 세션 이벤트로 기록된다(FR-LOOP-06).
type Loop struct {
	writer   *logd.Writer
	model    Model
	tools    Tools
	hooks    map[gen.HookPoint][]Hook
	traceID  string
	rootSpan string
	now      func() int64
	maxSteps int
}

// LoopOption은 Loop 설정이다.
type LoopOption func(*Loop)

// WithClock은 이벤트 ts의 시간원을 주입한다(테스트 결정성).
func WithClock(now func() int64) LoopOption {
	return func(l *Loop) { l.now = now }
}

// WithMaxSteps는 turn당 step 안전 상한을 바꾼다.
func WithMaxSteps(n int) LoopOption {
	return func(l *Loop) { l.maxSteps = n }
}

// New는 세션 하나의 루프를 만든다. 모델 히스토리는 이 writer에 결속된
// 프로젝션(Writer.Replay)에서만 재구성된다 — 다른 로그를 읽는 혼합은
// 표현 불가능하다(FR-LOG-03).
func New(w *logd.Writer, model Model, tools Tools, traceID, rootSpan string, opts ...LoopOption) *Loop {
	l := &Loop{
		writer: w, model: model, tools: tools,
		hooks:   map[gen.HookPoint][]Hook{},
		traceID: traceID, rootSpan: rootSpan,
		now:      func() int64 { return time.Now().UnixMilli() },
		maxSteps: DefaultMaxSteps,
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// RegisterHook은 고정 4지점 중 하나에 훅을 등록한다 (FR-LOOP-02).
func (l *Loop) RegisterHook(point gen.HookPoint, h Hook) error {
	if !validHookPoints[point] {
		return fmt.Errorf("loop: 알 수 없는 훅 지점 %q (허용: pre_step|pre_tool|post_tool|turn_stopping)", point)
	}
	l.hooks[point] = append(l.hooks[point], h)
	return nil
}

// RunTurn은 입력 하나를 소진할 때까지의 실행 단위다.
//
// 상태 머신 (FR-LOOP-01):
//
//	turn/start → user/message → { pre_step → step/start → model →
//	  [tool 루프: tool/call → pre_tool → 실행 → post_tool → tool/result] →
//	  step/end }* → turn_stopping → turn/end
//
// pre_step reject: 새 step 없이 turn 종료 — 첫 step이 reject되면 step 없는
// durable turn이 로그에 남는다(FR-LOOP-05, 시도 자체가 기록 대상).
// turn_stopping: 전원 continue면 종료, reject가 있으면 한 step 더,
// rewrite(x)는 x를 추가 user/message로 주입하고 한 step 더 진행한다.
func (l *Loop) RunTurn(ctx context.Context, input string) (err error) {
	if err := l.emit(ctx, gen.KindTurnStart, struct{}{}, nil, nil); err != nil {
		return err
	}
	// turn 경계는 오류·취소를 포함한 어떤 경로로 끝나도 durable하게 닫힌다
	// (FR-LOOP-05/06). 종료 기록은 취소된 ctx로 유실되면 안 되므로
	// durable admission context를 쓰고, 원래 오류와 함께 보존한다.
	defer func() {
		err = errors.Join(err, l.emitBoundary(ctx, gen.KindTurnEnd))
	}()
	if err := l.emit(ctx, gen.KindUserMessage, gen.UserMessagePayload{Text: input}, nil, nil); err != nil {
		return err
	}

	for steps := 0; ; steps++ {
		if steps >= l.maxSteps {
			return fmt.Errorf("loop: turn당 step 상한 %d 도달", l.maxSteps)
		}

		// pre_step: 모델 요청(로그 프로젝션에서 재구성)이 판정 대상
		req, err := l.buildRequest(ctx)
		if err != nil {
			return err
		}
		reqPayload, err := json.Marshal(req)
		if err != nil {
			return err
		}
		rejected, finalReq, _, err := l.runHooks(ctx, gen.HookPointPreStep, reqPayload)
		if err != nil {
			return err
		}
		if rejected != nil {
			return nil // step 없이 turn 종료 (FR-LOOP-05)
		}
		// rewrite는 전체 교체다 — zero-value에 strict decode 후 교체
		// (형태는 runHooks의 지점별 검증이 이미 보장).
		var nextReq ModelRequest
		if err := strictDecode(finalReq, &nextReq); err != nil {
			return fmt.Errorf("loop: pre_step rewrite 결과가 모델 요청 형태가 아님: %w", err)
		}
		req = nextReq

		toolCalls, err := l.runStep(ctx, req)
		if err != nil {
			return err
		}
		if toolCalls > 0 {
			continue // 툴 결과를 소화할 다음 step
		}

		// turn_stopping: 정지 저지 판정
		rejected, injected, rewrote, err := l.runHooks(ctx, gen.HookPointTurnStopping, json.RawMessage(`{}`))
		if err != nil {
			return err
		}
		if rejected == nil && !rewrote {
			return nil // turn 종료
		}
		if rewrote {
			// rewrite(x): x(userMessagePayload)를 추가 입력으로 주입하고 계속.
			// 형태는 runHooks의 지점별 검증이 이미 보장했다.
			var msg gen.UserMessagePayload
			if err := strictDecode(injected, &msg); err != nil {
				return fmt.Errorf("loop: turn_stopping rewrite 결과가 userMessagePayload가 아님: %w", err)
			}
			if err := l.emit(ctx, gen.KindUserMessage, msg, nil, nil); err != nil {
				return err
			}
		}
		// reject: 사유는 이미 hook/verdict로 기록됨 — 한 step 더
	}
}

// buildRequest는 모델 요청을 로그 프로젝션에서 재구성한다.
// "모델이 본 것은 로그에 있다"(불변식 3)를 구조로 강제하는 지점이다 —
// 로그를 거치지 않은 내용은 여기로 들어올 경로가 없다.
func (l *Loop) buildRequest(ctx context.Context) (ModelRequest, error) {
	state, err := l.writer.Replay(ctx)
	if err != nil {
		return ModelRequest{}, fmt.Errorf("loop: 모델 요청 재구성: %w", err)
	}
	return ModelRequest{Messages: state.Messages}, nil
}

// runStep은 step 하나의 실행 scope다. step/start가 성공하면 step/end는
// 오류·취소를 포함한 모든 경로에서 defer로 보장된다.
func (l *Loop) runStep(ctx context.Context, req ModelRequest) (toolCalls int, err error) {
	if err := l.emit(ctx, gen.KindStepStart, struct{}{}, nil, nil); err != nil {
		return 0, err
	}
	defer func() {
		err = errors.Join(err, l.emitBoundary(ctx, gen.KindStepEnd))
	}()

	resp, err := l.model.Complete(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("loop: 모델 요청: %w", err)
	}
	if err := l.emit(ctx, gen.KindAssistantMessage, gen.AssistantMessagePayload{Text: resp.Text},
		&resp.UsageIn, &resp.UsageOut); err != nil {
		return 0, err
	}
	for _, tc := range resp.ToolCalls {
		if err := l.runTool(ctx, tc); err != nil {
			return 0, err
		}
	}
	return len(resp.ToolCalls), nil
}

func (l *Loop) runTool(ctx context.Context, tc ToolCall) error {
	// 모델이 args를 생략한 경우에만 {}로 정규화한다 ([H] 승인 조건) —
	// toolCallPayload는 args를 필수 객체로 요구한다.
	args := tc.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	callPayload := gen.ToolCallPayload{Name: tc.Name, Args: args}
	// 모델의 원래 시도를 먼저 기록한다 — rewrite 체인은 hook/verdict의
	// 대체값과 함께 로그에서 완전 재구성된다.
	if err := l.emit(ctx, gen.KindToolCall, callPayload, nil, nil); err != nil {
		return err
	}
	tcPayload, err := json.Marshal(callPayload)
	if err != nil {
		return err
	}
	rejected, finalTC, _, err := l.runHooks(ctx, gen.HookPointPreTool, tcPayload)
	if err != nil {
		return err
	}
	if rejected != nil {
		// 툴은 실행되지 않는다. 모델 가시 결과로 거부 사실을 남긴다.
		return l.emit(ctx, gen.KindToolResult, gen.ToolResultPayload{
			Status: gen.ToolResultPayloadStatusRejected,
			Reason: &rejected.Reason,
		}, nil, nil)
	}
	// 전체 교체 의미론: zero-value에 strict decode(형태는 runHooks의 지점별
	// 검증이 보장) — 누락 필드가 원래 콜의 값으로 살아남으면 로그의
	// 대체값과 실제 실행이 어긋난다.
	var nextCall gen.ToolCallPayload
	if err := strictDecode(finalTC, &nextCall); err != nil {
		return fmt.Errorf("loop: pre_tool rewrite 결과가 toolCallPayload가 아님: %w", err)
	}
	tc = ToolCall{Name: nextCall.Name, Args: nextCall.Args}

	result, invokeErr := l.tools.Invoke(ctx, tc)
	if invokeErr != nil {
		msg := invokeErr.Error()
		return l.emit(ctx, gen.KindToolResult, gen.ToolResultPayload{
			Status: gen.ToolResultPayloadStatusError,
			Error:  &msg,
		}, nil, nil)
	}
	okResult := gen.ToolResultPayload{
		Status: gen.ToolResultPayloadStatusOk,
		Output: normalizeOutput(result.Output),
	}
	resPayload, err := json.Marshal(okResult)
	if err != nil {
		return err
	}
	rejectedPost, finalRes, _, err := l.runHooks(ctx, gen.HookPointPostTool, resPayload)
	if err != nil {
		return err
	}
	if rejectedPost != nil {
		// 결과가 모델에 노출되지 않는다 — 거부 사실만 모델 가시로 남긴다.
		return l.emit(ctx, gen.KindToolResult, gen.ToolResultPayload{
			Status: gen.ToolResultPayloadStatusRejected,
			Reason: &rejectedPost.Reason,
		}, nil, nil)
	}
	var finalResult gen.ToolResultPayload
	if err := strictDecode(finalRes, &finalResult); err != nil {
		return fmt.Errorf("loop: post_tool rewrite 결과가 toolResultPayload가 아님: %w", err)
	}
	return l.emit(ctx, gen.KindToolResult, finalResult, nil, nil)
}

// normalizeOutput은 툴 출력의 객체 정규화다 ([H] 승인 조건): 객체는 그대로,
// 스칼라·배열·null은 {"value": <원본 JSON>}으로 감싼다.
func normalizeOutput(out json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return trimmed
	}
	if len(trimmed) == 0 {
		trimmed = json.RawMessage(`null`)
	}
	wrapped, _ := json.Marshal(map[string]json.RawMessage{"value": trimmed})
	return wrapped
}

// runHooks는 지점의 훅 전부를 독립 호출하고(전원 원본 payload 수신, 체이닝
// 없음), 모든 판정을 형태·지점별로 먼저 검증한 뒤에만 각각 hook/verdict
// 이벤트로 기록한다(FR-LOOP-06) — 뒤쪽의 잘못된 판정 때문에 앞쪽 verdict만
// 남는 부분 기록이 없다. 기록은 durable admission context를 쓴다: 훅이 ctx를
// 취소해도 생성된 판정은 유실되지 않는다. 이후 FR-LOOP-04 규칙으로 해소한다.
func (l *Loop) runHooks(ctx context.Context, point gen.HookPoint, payload json.RawMessage) (rejected *Decision, final json.RawMessage, rewrote bool, err error) {
	hooks := l.hooks[point]
	decisions := make([]Decision, len(hooks))
	for i, h := range hooks {
		decisions[i] = h(ctx, HookContext{Point: point, Payload: payload})
	}
	// 1단계: 전건 검증 — 형태 계약 + rewrite 대체값의 지점별 형태
	for _, d := range decisions {
		if err := validateDecision(d); err != nil {
			return nil, nil, false, err
		}
		if d.Verdict == gen.HookVerdictPayloadVerdictRewrite {
			if err := validateRewriteTarget(point, d.Rewrite); err != nil {
				return nil, nil, false, fmt.Errorf("loop: %s rewrite 대체값 검증: %w", point, err)
			}
		}
	}
	// 2단계: 전건 durable 기록
	durableCtx := context.WithoutCancel(ctx)
	for _, d := range decisions {
		p := gen.HookVerdictPayload{Point: point, Verdict: d.Verdict}
		if d.Reason != "" {
			p.Reason = &d.Reason
		}
		if d.Verdict == gen.HookVerdictPayloadVerdictRewrite {
			p.Rewrite = d.Rewrite
		}
		if err := l.emit(durableCtx, gen.KindHookVerdict, p, nil, nil); err != nil {
			return nil, nil, false, err
		}
	}
	rejected, final, rewrote = resolveDecisions(decisions, payload)
	return rejected, final, rewrote, nil
}

// validateRewriteTarget은 지점별 rewrite 대체값의 형태 계약이다
// (2026-08-17 [H] 승인). 생성 payload 타입의 strict decode는 미지 필드만
// 잡을 뿐 스키마의 required·컨테이너 제약(누락 vs 빈 문자열, 객체 vs
// 배열/null)을 강제하지 못하므로, raw JSON 수준의 구조 검사를 함께 한다 —
// 이 검증 전체가 verdict 기록보다 먼저 수행된다.
func validateRewriteTarget(point gen.HookPoint, x json.RawMessage) error {
	fields, err := objectFields(x)
	if err != nil {
		return err
	}
	switch point {
	case gen.HookPointPreStep:
		var req ModelRequest // 모델 요청 형태는 코어 소유
		if err := strictDecode(x, &req); err != nil {
			return err
		}
		return requireField(fields, "messages", '[', "배열")
	case gen.HookPointPreTool:
		var call gen.ToolCallPayload
		if err := strictDecode(x, &call); err != nil {
			return err
		}
		if call.Name == "" {
			return fmt.Errorf("툴 이름이 없음 (빈/부분 대체는 실행 불가)")
		}
		return requireField(fields, "args", '{', "객체")
	case gen.HookPointPostTool:
		var res gen.ToolResultPayload
		if err := strictDecode(x, &res); err != nil {
			return err
		}
		if res.Status != gen.ToolResultPayloadStatusOk {
			return fmt.Errorf("post_tool rewrite는 status:ok 분기만 허용 (%q)", res.Status)
		}
		if res.Reason != nil || res.Error != nil {
			return fmt.Errorf("ok 분기에 reason/error가 있음")
		}
		return requireField(fields, "output", '{', "객체")
	case gen.HookPointTurnStopping:
		var msg gen.UserMessagePayload
		if err := strictDecode(x, &msg); err != nil {
			return err
		}
		return requireField(fields, "text", '"', "문자열")
	default:
		return fmt.Errorf("알 수 없는 지점 %q", point)
	}
}

// objectFields는 rewrite 대체값을 raw 필드 표로 파싱한다 — 필드의
// 존재 여부와 값의 컨테이너 종류를 구분하기 위한 원본 보존 뷰다.
func objectFields(x json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(x, &fields); err != nil {
		return nil, fmt.Errorf("객체가 아님: %w", err)
	}
	return fields, nil
}

// requireField는 필드가 존재하고 값이 요구된 JSON 컨테이너 종류
// (첫 바이트 '{'=객체, '['=배열, '"'=문자열)임을 검사한다 — 스키마의
// required·type 제약을 생성 타입 decode가 놓치는 부분의 보강이다.
func requireField(fields map[string]json.RawMessage, key string, kind byte, kindName string) error {
	v, ok := fields[key]
	if !ok {
		return fmt.Errorf("%s 키가 없음 (불완전한 전체 교체)", key)
	}
	trimmed := bytes.TrimSpace(v)
	if len(trimmed) == 0 || trimmed[0] != kind {
		return fmt.Errorf("%s는 null이 아닌 %s여야 함 (현재: %s)", key, kindName, trimmed)
	}
	return nil
}

// strictDecode는 rewrite 대체값의 전체 교체 디코드다: zero-value 대상,
// 미지 필드 거부, 후행 데이터 거부.
func strictDecode(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return fmt.Errorf("후행 데이터가 있음")
	}
	return nil
}

// emitBoundary는 turn/step 종료 경계의 durable 기록이다 — 취소된 ctx로
// 경계 이벤트가 유실되면 로그의 구조 재구성이 깨지므로, admission에
// 취소가 전파되지 않는 context를 쓴다.
func (l *Loop) emitBoundary(ctx context.Context, kind gen.Kind) error {
	return l.emit(context.WithoutCancel(ctx), kind, struct{}{}, nil, nil)
}

func (l *Loop) emit(ctx context.Context, kind gen.Kind, payload any, usageIn, usageOut *int64) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := l.writer.Submit(ctx, gen.EventRecord{
		Ts: l.now(), TraceID: l.traceID, SpanID: l.rootSpan,
		Kind: kind, Actor: "parent", Payload: b,
		UsageIn: usageIn, UsageOut: usageOut,
	}); err != nil {
		return fmt.Errorf("loop: %s 기록: %w", kind, err)
	}
	return nil
}
