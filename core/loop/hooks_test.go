package loop

import (
	"encoding/json"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// T5 완료 기준: 훅 판정 조합 테이블 테스트 (FR-LOOP-04 —
// reject > rewrite > continue, 복수 rewrite는 등록 순서대로 적용).
func TestResolveDecisionsTable(t *testing.T) {
	orig := json.RawMessage(`{"v":"원본"}`)
	r1 := json.RawMessage(`{"v":"r1"}`)
	r2 := json.RawMessage(`{"v":"r2"}`)

	cases := []struct {
		name        string
		decisions   []Decision
		wantReject  string // "" = reject 없음
		wantFinal   string
		wantRewrote bool
	}{
		{"훅 없음", nil, "", `{"v":"원본"}`, false},
		{"continue 단독", []Decision{Continue()}, "", `{"v":"원본"}`, false},
		{"rewrite 단독", []Decision{Rewrite(r1, "")}, "", `{"v":"r1"}`, true},
		{"reject 단독", []Decision{Reject("사유A")}, "사유A", "", false},
		{"continue+rewrite", []Decision{Continue(), Rewrite(r1, "")}, "", `{"v":"r1"}`, true},
		{"rewrite+continue", []Decision{Rewrite(r1, ""), Continue()}, "", `{"v":"r1"}`, true},
		{"rewrite+reject — reject 우선", []Decision{Rewrite(r1, ""), Reject("사유B")}, "사유B", "", false},
		{"reject+rewrite — reject 우선", []Decision{Reject("사유C"), Rewrite(r1, "")}, "사유C", "", false},
		{"continue+reject", []Decision{Continue(), Reject("사유D")}, "사유D", "", false},
		{"복수 reject — 등록 순서상 첫 reject", []Decision{Continue(), Reject("첫째"), Reject("둘째")}, "첫째", "", false},
		{"복수 rewrite — 등록 순서 적용(마지막이 최종)", []Decision{Rewrite(r1, ""), Rewrite(r2, "")}, "", `{"v":"r2"}`, true},
		{"3종 혼합 — reject 우선", []Decision{Rewrite(r1, ""), Continue(), Reject("사유E")}, "사유E", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rejected, final, rewrote := resolveDecisions(c.decisions, orig)
			if c.wantReject != "" {
				if rejected == nil || rejected.Reason != c.wantReject {
					t.Fatalf("rejected = %+v (%q 기대)", rejected, c.wantReject)
				}
				return
			}
			if rejected != nil {
				t.Fatalf("예상 밖 reject: %+v", rejected)
			}
			if string(final) != c.wantFinal || rewrote != c.wantRewrote {
				t.Fatalf("final=%s rewrote=%v (%s/%v 기대)", final, rewrote, c.wantFinal, c.wantRewrote)
			}
		})
	}
}

func TestValidateDecision(t *testing.T) {
	bad := []struct {
		name string
		d    Decision
	}{
		{"빈 사유 reject", Decision{Verdict: gen.HookVerdictPayloadVerdictReject}},
		{"대체값 없는 rewrite", Decision{Verdict: gen.HookVerdictPayloadVerdictRewrite}},
		{"대체값 있는 continue", Decision{Verdict: gen.HookVerdictPayloadVerdictContinue, Rewrite: json.RawMessage(`{}`)}},
		{"미지 판정", Decision{Verdict: "pause"}},
	}
	for _, c := range bad {
		if err := validateDecision(c.d); err == nil {
			t.Errorf("%s: 위반 판정이 통과함", c.name)
		}
	}
	good := []Decision{Continue(), Rewrite(json.RawMessage(`{}`), "이유"), Reject("이유")}
	for i, d := range good {
		if err := validateDecision(d); err != nil {
			t.Errorf("유효 판정 %d 거부: %v", i, err)
		}
	}
}
