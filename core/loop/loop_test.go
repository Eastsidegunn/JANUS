package loop

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
)

// ---- 테스트 전용 Fake 구현 ----

// FakeStore는 logd.Store의 인메모리 fake다.
type FakeStore struct {
	mu      sync.Mutex
	events  []gen.EventRecord
	lastSeq int64
}

func (s *FakeStore) LastSeq(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq, nil
}

func (s *FakeStore) Append(ctx context.Context, rec gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, rec)
	s.lastSeq = rec.Seq
	return nil
}

func (s *FakeStore) AppendBatch(ctx context.Context, recs []gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range recs {
		s.events = append(s.events, rec)
		s.lastSeq = rec.Seq
	}
	return nil
}

func (s *FakeStore) ReadFrom(ctx context.Context, fromSeq int64) ([]gen.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []gen.EventRecord
	for _, e := range s.events {
		if e.Seq >= fromSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *FakeStore) Close() error { return nil }

// FakeModel은 각본대로 응답하는 모델 fake다.
type FakeModel struct {
	script   []ModelResponse
	i        int
	requests []ModelRequest
}

func (m *FakeModel) Complete(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	m.requests = append(m.requests, req)
	if m.i >= len(m.script) {
		return ModelResponse{Text: "각본 소진"}, nil
	}
	r := m.script[m.i]
	m.i++
	return r, nil
}

// FakeTools는 받은 콜을 기록하고 고정 출력을 돌려준다.
type FakeTools struct {
	calls []ToolCall
}

func (t *FakeTools) Invoke(ctx context.Context, call ToolCall) (ToolResult, error) {
	t.calls = append(t.calls, call)
	return ToolResult{Output: json.RawMessage(`"ok"`)}, nil
}

func newLoop(t *testing.T, model Model, tools Tools) (*Loop, *FakeStore) {
	t.Helper()
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	var tick int64
	l := New(w, store, model, tools,
		strings.Repeat("a", 32), strings.Repeat("b", 16),
		WithClock(func() int64 { tick++; return tick }))
	return l, store
}

func kinds(events []gen.EventRecord) []gen.Kind {
	out := make([]gen.Kind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func assertKinds(t *testing.T, got []gen.Kind, want ...gen.Kind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("이벤트 %d건 (%d건 기대):\n got=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("이벤트 %d: %s (%s 기대)\n전체=%v", i, got[i], want[i], got)
		}
	}
}

// ---- T5 완료 기준 테스트 ----

// FR-LOOP-05: reject된 첫 step은 step 없는 durable turn으로 로그에 남는다 —
// 시도 자체가 기록 대상이다.
func TestRejectedFirstStepLeavesDurableTurnWithoutSteps(t *testing.T) {
	model := &FakeModel{}
	l, store := newLoop(t, model, &FakeTools{})
	if err := l.RegisterHook(gen.HookPointPreStep, func(ctx context.Context, hc HookContext) Decision {
		return Reject("정책상 실행 불가")
	}); err != nil {
		t.Fatal(err)
	}

	if err := l.RunTurn(context.Background(), "시도"); err != nil {
		t.Fatal(err)
	}
	events, _ := store.ReadFrom(context.Background(), 1)
	assertKinds(t, kinds(events),
		gen.KindTurnStart, gen.KindUserMessage, gen.KindHookVerdict, gen.KindTurnEnd)

	// step은 시작조차 안 됐고, 모델도 호출되지 않았다
	for _, e := range events {
		if e.Kind == gen.KindStepStart || e.Kind == gen.KindStepEnd {
			t.Fatal("reject된 turn에 step 경계가 있음")
		}
	}
	if len(model.requests) != 0 {
		t.Fatal("reject됐는데 모델이 호출됨")
	}
	// 판정 사유가 이벤트로 남았다 (FR-LOOP-06)
	var p gen.HookVerdictPayload
	if err := json.Unmarshal(events[2].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Verdict != gen.HookVerdictPayloadVerdictReject || p.Reason == nil || *p.Reason != "정책상 실행 불가" {
		t.Fatalf("판정 기록 이상: %+v", p)
	}
	// durable: 파생 상태 재계산 가능 (writer 경유이므로 T3의 내구 경로)
	if _, err := logd.Replay(events); err != nil {
		t.Fatalf("reject turn 재생 불가: %v", err)
	}
}

// 정상 흐름: 툴 스텝 1회 + 종결 스텝 1회 — turn/step 경계가 전부 기록된다
// (FR-LOOP-01/06). 모델 요청은 로그 프로젝션에서 재구성된다(FR-LOG-03).
func TestTurnFlowWithTools(t *testing.T) {
	model := &FakeModel{script: []ModelResponse{
		{Text: "툴을 쓰겠다", ToolCalls: []ToolCall{{Name: "bash", Args: json.RawMessage(`{"cmd":"ls"}`)}}, UsageIn: 10, UsageOut: 5},
		{Text: "끝", UsageIn: 20, UsageOut: 7},
	}}
	tools := &FakeTools{}
	l, store := newLoop(t, model, tools)

	if err := l.RunTurn(context.Background(), "파일 보여줘"); err != nil {
		t.Fatal(err)
	}
	events, _ := store.ReadFrom(context.Background(), 1)
	assertKinds(t, kinds(events),
		gen.KindTurnStart, gen.KindUserMessage,
		gen.KindStepStart, gen.KindAssistantMessage, gen.KindToolCall, gen.KindToolResult, gen.KindStepEnd,
		gen.KindStepStart, gen.KindAssistantMessage, gen.KindStepEnd,
		gen.KindTurnEnd)

	// 2번째 모델 요청은 로그 프로젝션 그대로다: user, assistant, tool_call, tool_result
	if len(model.requests) != 2 {
		t.Fatalf("모델 호출 %d회 (2회 기대)", len(model.requests))
	}
	roles := []logd.Role{}
	for _, m := range model.requests[1].Messages {
		roles = append(roles, m.Role)
	}
	want := []logd.Role{logd.RoleUser, logd.RoleAssistant, logd.RoleToolCall, logd.RoleToolResult}
	if len(roles) != len(want) {
		t.Fatalf("2번째 요청 히스토리 %v (%v 기대)", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("2번째 요청 히스토리 %v (%v 기대)", roles, want)
		}
	}
	if len(tools.calls) != 1 || tools.calls[0].Name != "bash" {
		t.Fatalf("툴 호출 이상: %+v", tools.calls)
	}
	// usage가 assistant/message에 기록됐다
	var sawUsage bool
	for _, e := range events {
		if e.Kind == gen.KindAssistantMessage && e.UsageIn != nil && *e.UsageIn == 10 {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Fatal("usage 미기록")
	}
}

// pre_tool rewrite: 툴은 대체된 인자로 실행되고, 원래 시도(tool/call)와
// 대체값(hook/verdict.rewrite)이 모두 로그에 남아 재구성 가능하다.
func TestPreToolRewriteApplied(t *testing.T) {
	model := &FakeModel{script: []ModelResponse{
		{ToolCalls: []ToolCall{{Name: "bash", Args: json.RawMessage(`{"cmd":"rm -rf /"}`)}}},
		{Text: "끝"},
	}}
	tools := &FakeTools{}
	l, store := newLoop(t, model, tools)
	rewritten := json.RawMessage(`{"name":"bash","args":{"cmd":"ls"}}`)
	l.RegisterHook(gen.HookPointPreTool, func(ctx context.Context, hc HookContext) Decision {
		return Rewrite(rewritten, "위험 명령 교정")
	})

	if err := l.RunTurn(context.Background(), "정리해줘"); err != nil {
		t.Fatal(err)
	}
	if len(tools.calls) != 1 || string(tools.calls[0].Args) != `{"cmd":"ls"}` {
		t.Fatalf("툴이 대체 인자로 실행되지 않음: %+v", tools.calls)
	}
	events, _ := store.ReadFrom(context.Background(), 1)
	var sawOriginal, sawRewriteVerdict bool
	for _, e := range events {
		if e.Kind == gen.KindToolCall && strings.Contains(string(e.Payload), "rm -rf") {
			sawOriginal = true
		}
		if e.Kind == gen.KindHookVerdict {
			var p gen.HookVerdictPayload
			json.Unmarshal(e.Payload, &p)
			if p.Point == gen.HookPointPreTool && p.Verdict == gen.HookVerdictPayloadVerdictRewrite &&
				strings.Contains(string(p.Rewrite), `"ls"`) {
				sawRewriteVerdict = true
			}
		}
	}
	if !sawOriginal || !sawRewriteVerdict {
		t.Fatalf("재구성 근거 누락: 원래 시도=%v, rewrite 판정=%v", sawOriginal, sawRewriteVerdict)
	}
}

// pre_tool reject: 툴은 실행되지 않고 거부 사실이 모델 가시 결과로 남는다.
func TestPreToolRejectSkipsExecution(t *testing.T) {
	model := &FakeModel{script: []ModelResponse{
		{ToolCalls: []ToolCall{{Name: "bash", Args: json.RawMessage(`{"cmd":"x"}`)}}},
		{Text: "끝"},
	}}
	tools := &FakeTools{}
	l, store := newLoop(t, model, tools)
	l.RegisterHook(gen.HookPointPreTool, func(ctx context.Context, hc HookContext) Decision {
		return Reject("egress 미허용")
	})
	if err := l.RunTurn(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if len(tools.calls) != 0 {
		t.Fatal("reject된 툴이 실행됨")
	}
	events, _ := store.ReadFrom(context.Background(), 1)
	var sawRejectedResult bool
	for _, e := range events {
		if e.Kind == gen.KindToolResult && strings.Contains(string(e.Payload), "egress 미허용") {
			sawRejectedResult = true
		}
	}
	if !sawRejectedResult {
		t.Fatal("거부 사실이 모델 가시 결과로 남지 않음")
	}
}

// 훅 독립성 (FR-LOOP-03): 모든 훅은 원본 payload를 받는다 — 앞선 훅의
// rewrite가 뒤 훅의 입력이 되지 않는다(체이닝 없음). 적용은 등록 순서.
func TestHooksAreIndependent(t *testing.T) {
	model := &FakeModel{script: []ModelResponse{
		{ToolCalls: []ToolCall{{Name: "t", Args: json.RawMessage(`{"v":"원본"}`)}}},
		{Text: "끝"},
	}}
	tools := &FakeTools{}
	l, _ := newLoop(t, model, tools)
	var firstSaw, secondSaw string
	l.RegisterHook(gen.HookPointPreTool, func(ctx context.Context, hc HookContext) Decision {
		firstSaw = string(hc.Payload)
		return Rewrite(json.RawMessage(`{"name":"t","args":{"v":"r1"}}`), "")
	})
	l.RegisterHook(gen.HookPointPreTool, func(ctx context.Context, hc HookContext) Decision {
		secondSaw = string(hc.Payload)
		return Rewrite(json.RawMessage(`{"name":"t","args":{"v":"r2"}}`), "")
	})
	if err := l.RunTurn(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if firstSaw != secondSaw || !strings.Contains(secondSaw, "원본") {
		t.Fatalf("훅이 원본을 받지 않음: 첫=%s 둘=%s", firstSaw, secondSaw)
	}
	// 등록 순서 적용 — 대체값은 전체 교체이므로 마지막 rewrite가 최종
	if string(tools.calls[0].Args) != `{"v":"r2"}` {
		t.Fatalf("최종 인자 = %s (r2 기대)", tools.calls[0].Args)
	}
}

// turn_stopping reject는 turn 정지를 저지해 step을 하나 더 돌린다.
func TestTurnStoppingRejectForcesAnotherStep(t *testing.T) {
	model := &FakeModel{script: []ModelResponse{
		{Text: "1차 응답"},
		{Text: "2차 응답"},
	}}
	l, store := newLoop(t, model, &FakeTools{})
	rejectedOnce := false
	l.RegisterHook(gen.HookPointTurnStopping, func(ctx context.Context, hc HookContext) Decision {
		if !rejectedOnce {
			rejectedOnce = true
			return Reject("검증 단계가 남았다")
		}
		return Continue()
	})
	if err := l.RunTurn(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("모델 호출 %d회 (2회 기대 — turn_stopping reject가 step을 추가해야 함)", len(model.requests))
	}
	events, _ := store.ReadFrom(context.Background(), 1)
	stepStarts := 0
	for _, e := range events {
		if e.Kind == gen.KindStepStart {
			stepStarts++
		}
	}
	if stepStarts != 2 {
		t.Fatalf("step %d회 (2회 기대)", stepStarts)
	}
}

// 무한 저지에 대한 안전 상한.
func TestMaxStepsGuard(t *testing.T) {
	model := &FakeModel{script: []ModelResponse{{Text: "a"}, {Text: "b"}, {Text: "c"}}}
	store := &FakeStore{}
	w, _ := logd.NewWriter(context.Background(), store)
	defer w.Close()
	var tick int64
	l := New(w, store, model, &FakeTools{},
		strings.Repeat("a", 32), strings.Repeat("b", 16),
		WithClock(func() int64 { tick++; return tick }), WithMaxSteps(2))
	l.RegisterHook(gen.HookPointTurnStopping, func(ctx context.Context, hc HookContext) Decision {
		return Reject("영원히")
	})
	if err := l.RunTurn(context.Background(), "x"); err == nil {
		t.Fatal("step 상한 없이 무한 진행")
	}
	// 상한 도달로 끝나도 turn 경계는 durable하게 닫힌다
	events, _ := store.ReadFrom(context.Background(), 1)
	if events[len(events)-1].Kind != gen.KindTurnEnd {
		t.Fatal("오류 종료에서 turn/end 누락")
	}
}

// 잘못된 훅 판정(빈 사유 reject)은 조용히 삼켜지지 않고 실패한다.
func TestInvalidHookDecisionFails(t *testing.T) {
	model := &FakeModel{script: []ModelResponse{{Text: "x"}}}
	l, _ := newLoop(t, model, &FakeTools{})
	l.RegisterHook(gen.HookPointPreStep, func(ctx context.Context, hc HookContext) Decision {
		return Decision{Verdict: gen.HookVerdictPayloadVerdictReject} // 사유 없음
	})
	if err := l.RunTurn(context.Background(), "x"); err == nil {
		t.Fatal("위반 판정이 통과함")
	}
}

// 훅 지점은 4개로 고정된다 (FR-LOOP-02).
func TestRegisterHookRejectsUnknownPoint(t *testing.T) {
	l, _ := newLoop(t, &FakeModel{}, &FakeTools{})
	if err := l.RegisterHook(gen.HookPoint("post_step"), func(ctx context.Context, hc HookContext) Decision {
		return Continue()
	}); err == nil {
		t.Fatal("미지의 훅 지점 등록이 성공함")
	}
}
