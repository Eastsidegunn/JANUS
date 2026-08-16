// Package loop는 turn/step 상태 머신과 훅 4지점을 구현한다 (FR-LOOP).
// 상태 머신은 코어에 고정되며 교체 불가하다(FR-LOOP-01) — seam이 아니다.
package loop

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// 훅 지점은 4개로 고정한다 (FR-LOOP-02). contracts의 HookPoint 어휘를 쓴다.
var validHookPoints = map[gen.HookPoint]bool{
	gen.HookPointPreStep:      true,
	gen.HookPointPreTool:      true,
	gen.HookPointPostTool:     true,
	gen.HookPointTurnStopping: true,
}

// Decision은 훅 판정이다: continue | rewrite(x) | reject(reason) (FR-LOOP-03).
type Decision struct {
	Verdict gen.HookVerdictPayloadVerdict
	// Rewrite는 rewrite 판정의 대체값 x다 — 이 값이 hook/verdict 이벤트에
	// 기록되어야 로그에서 모델 입력을 재구성할 수 있다(FR-LOG-03).
	Rewrite json.RawMessage
	Reason  string
}

// Continue는 무개입 판정이다.
func Continue() Decision {
	return Decision{Verdict: gen.HookVerdictPayloadVerdictContinue}
}

// Rewrite는 대상을 x로 교체하는 판정이다.
func Rewrite(x json.RawMessage, reason string) Decision {
	return Decision{Verdict: gen.HookVerdictPayloadVerdictRewrite, Rewrite: x, Reason: reason}
}

// Reject는 대상 실행을 거부하는 판정이다. reason은 비어 있으면 안 된다.
func Reject(reason string) Decision {
	return Decision{Verdict: gen.HookVerdictPayloadVerdictReject, Reason: reason}
}

// Hook은 지점별 대상(payload)을 받아 독립적으로 판정한다.
// 미들웨어식 next() 체이닝은 없다(FR-LOOP-03) — 모든 훅은 원본 payload를
// 받고, rewrite의 적용은 판정 수집 후 resolveDecisions가 등록 순서대로 한다.
type Hook func(ctx context.Context, hc HookContext) Decision

// HookContext는 훅 호출 문맥이다.
type HookContext struct {
	Point gen.HookPoint
	// Payload는 지점별 판정 대상의 정규화 JSON이다:
	// pre_step → 모델 요청, pre_tool → 툴 콜, post_tool → 툴 결과,
	// turn_stopping → 빈 객체.
	Payload json.RawMessage
}

// validateDecision은 판정의 형태 계약이다 — 위반 판정은 이벤트로 기록될 수
// 없으므로(hook/verdict 스키마) 훅 버그를 조용히 삼키지 않고 실패시킨다.
func validateDecision(d Decision) error {
	switch d.Verdict {
	case gen.HookVerdictPayloadVerdictContinue:
		if d.Rewrite != nil {
			return fmt.Errorf("loop: continue 판정에 대체값이 있음")
		}
	case gen.HookVerdictPayloadVerdictRewrite:
		if len(d.Rewrite) == 0 {
			return fmt.Errorf("loop: rewrite 판정에 대체값이 없음")
		}
	case gen.HookVerdictPayloadVerdictReject:
		if d.Reason == "" {
			return fmt.Errorf("loop: reject 판정에 사유가 없음")
		}
	default:
		return fmt.Errorf("loop: 알 수 없는 판정 %q", d.Verdict)
	}
	return nil
}

// resolveDecisions는 다중 훅 판정의 고정 충돌 해소 규칙이다 (FR-LOOP-04):
// reject > rewrite > continue. reject가 하나라도 있으면 등록 순서상 첫
// reject가 이긴다. reject가 없으면 rewrite들을 등록 순서대로 원본에
// 적용한다 — 대체값은 전체 교체이므로 순서 적용의 결과는 마지막 rewrite다.
func resolveDecisions(decisions []Decision, original json.RawMessage) (rejected *Decision, final json.RawMessage, rewrote bool) {
	for i := range decisions {
		if decisions[i].Verdict == gen.HookVerdictPayloadVerdictReject {
			return &decisions[i], nil, false
		}
	}
	final = original
	for _, d := range decisions {
		if d.Verdict == gen.HookVerdictPayloadVerdictRewrite {
			final = d.Rewrite
			rewrote = true
		}
	}
	return nil, final, rewrote
}
