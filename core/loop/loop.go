package loop

import (
	"context"
	"encoding/json"
	"fmt"
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
	reader   logd.Reader
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

// New는 세션 하나의 루프를 만든다. writer/reader는 같은 세션 로그여야 한다.
func New(w *logd.Writer, r logd.Reader, model Model, tools Tools, traceID, rootSpan string, opts ...LoopOption) *Loop {
	l := &Loop{
		writer: w, reader: r, model: model, tools: tools,
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
	// turn 경계는 어떤 경로로 끝나도 durable하게 닫힌다 (FR-LOOP-05/06)
	defer func() {
		if endErr := l.emit(ctx, gen.KindTurnEnd, struct{}{}, nil, nil); endErr != nil && err == nil {
			err = endErr
		}
	}()
	if err := l.emit(ctx, gen.KindUserMessage, map[string]string{"text": input}, nil, nil); err != nil {
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
		if err := json.Unmarshal(finalReq, &req); err != nil {
			return fmt.Errorf("loop: pre_step rewrite 결과가 모델 요청 형태가 아님: %w", err)
		}

		if err := l.emit(ctx, gen.KindStepStart, struct{}{}, nil, nil); err != nil {
			return err
		}
		resp, err := l.model.Complete(ctx, req)
		if err != nil {
			l.emit(ctx, gen.KindStepEnd, struct{}{}, nil, nil)
			return fmt.Errorf("loop: 모델 요청: %w", err)
		}
		if err := l.emit(ctx, gen.KindAssistantMessage, map[string]string{"text": resp.Text},
			&resp.UsageIn, &resp.UsageOut); err != nil {
			return err
		}

		for _, tc := range resp.ToolCalls {
			if err := l.runTool(ctx, tc); err != nil {
				return err
			}
		}
		if err := l.emit(ctx, gen.KindStepEnd, struct{}{}, nil, nil); err != nil {
			return err
		}

		if len(resp.ToolCalls) > 0 {
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
			// rewrite(x): x를 추가 입력으로 주입하고 계속
			var msg map[string]any
			if err := json.Unmarshal(injected, &msg); err != nil {
				return fmt.Errorf("loop: turn_stopping rewrite 결과가 객체가 아님: %w", err)
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
	state, err := logd.ReplayReader(ctx, l.reader)
	if err != nil {
		return ModelRequest{}, fmt.Errorf("loop: 모델 요청 재구성: %w", err)
	}
	return ModelRequest{Messages: state.Messages}, nil
}

func (l *Loop) runTool(ctx context.Context, tc ToolCall) error {
	// 모델의 원래 시도를 먼저 기록한다 — rewrite 체인은 hook/verdict의
	// 대체값과 함께 로그에서 완전 재구성된다.
	if err := l.emit(ctx, gen.KindToolCall, tc, nil, nil); err != nil {
		return err
	}
	tcPayload, err := json.Marshal(tc)
	if err != nil {
		return err
	}
	rejected, finalTC, _, err := l.runHooks(ctx, gen.HookPointPreTool, tcPayload)
	if err != nil {
		return err
	}
	if rejected != nil {
		// 툴은 실행되지 않는다. 모델 가시 결과로 거부 사실을 남긴다.
		return l.emit(ctx, gen.KindToolResult,
			map[string]any{"rejected": true, "reason": rejected.Reason}, nil, nil)
	}
	if err := json.Unmarshal(finalTC, &tc); err != nil {
		return fmt.Errorf("loop: pre_tool rewrite 결과가 툴 콜 형태가 아님: %w", err)
	}

	result, invokeErr := l.tools.Invoke(ctx, tc)
	if invokeErr != nil {
		return l.emit(ctx, gen.KindToolResult,
			map[string]any{"error": invokeErr.Error()}, nil, nil)
	}
	resPayload, err := json.Marshal(map[string]any{"output": result.Output})
	if err != nil {
		return err
	}
	rejectedPost, finalRes, _, err := l.runHooks(ctx, gen.HookPointPostTool, resPayload)
	if err != nil {
		return err
	}
	if rejectedPost != nil {
		// 결과가 모델에 노출되지 않는다 — 거부 사실만 모델 가시로 남긴다.
		return l.emit(ctx, gen.KindToolResult,
			map[string]any{"rejected": true, "reason": rejectedPost.Reason}, nil, nil)
	}
	var out map[string]any
	if err := json.Unmarshal(finalRes, &out); err != nil {
		return fmt.Errorf("loop: post_tool rewrite 결과가 객체가 아님: %w", err)
	}
	return l.emit(ctx, gen.KindToolResult, out, nil, nil)
}

// runHooks는 지점의 훅 전부를 독립 호출하고(전원 원본 payload 수신, 체이닝
// 없음), 판정을 각각 hook/verdict 이벤트로 기록한 뒤(FR-LOOP-06),
// FR-LOOP-04 규칙으로 해소한다.
func (l *Loop) runHooks(ctx context.Context, point gen.HookPoint, payload json.RawMessage) (rejected *Decision, final json.RawMessage, rewrote bool, err error) {
	hooks := l.hooks[point]
	decisions := make([]Decision, len(hooks))
	for i, h := range hooks {
		decisions[i] = h(ctx, HookContext{Point: point, Payload: payload})
	}
	for _, d := range decisions {
		if err := validateDecision(d); err != nil {
			return nil, nil, false, err
		}
		p := gen.HookVerdictPayload{Point: point, Verdict: d.Verdict}
		if d.Reason != "" {
			p.Reason = &d.Reason
		}
		if d.Verdict == gen.HookVerdictPayloadVerdictRewrite {
			p.Rewrite = d.Rewrite
		}
		if err := l.emit(ctx, gen.KindHookVerdict, p, nil, nil); err != nil {
			return nil, nil, false, err
		}
	}
	rejected, final, rewrote = resolveDecisions(decisions, payload)
	return rejected, final, rewrote, nil
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
