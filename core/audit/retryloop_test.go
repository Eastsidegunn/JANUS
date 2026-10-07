package audit

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// runBDenyAtMs is an observed real-run shape: 8 CONNECT denies to the same
// domain with non-decreasing gaps 0.5s → 24.5s.
var runBDenyAtMs = []int64{0, 500, 1500, 3500, 8000, 16000, 28500, 53000}

const runBWarning = "경고: 모델 재시도 루프 의심 — api.anthropic.com CONNECT deny 8건(간격 0.5s→24.5s), 모델 가시 이벤트 0"

type retrySession struct {
	events []gen.EventRecord
}

func (s *retrySession) add(kind gen.Kind, actor string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	s.events = append(s.events, gen.EventRecord{
		Seq: int64(len(s.events) + 1), TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("c", 16),
		Kind: kind, Actor: actor, Payload: b,
	})
}

func (s *retrySession) egress(base int64, at []int64, domain, method string, decision gen.EgressPayloadDecision) {
	for _, offset := range at {
		s.add(gen.KindCollectorEgress, "collector", gen.EgressPayload{
			AtMs: base + offset, Decision: decision, Domain: domain, Method: method,
		})
	}
}

func newRetrySession(withModelEvents bool, denyAt []int64) []gen.EventRecord {
	s := &retrySession{}
	s.add(gen.KindSessionStart, "parent", map[string]any{})
	s.add(gen.KindSubagentReady, "subagent:claude-code:1", gen.ReadyPayload{Grade: gen.ReadyPayloadGradeObservable})
	if withModelEvents {
		s.add(gen.KindSubagentMessage, "subagent:claude-code:1", gen.AgentMessagePayload{Text: "작업 시작"})
		s.add(gen.KindSubagentToolCall, "subagent:claude-code:1", gen.AgentToolCallPayload{CallID: "c1", Name: "Bash", Args: json.RawMessage(`{}`)})
	}
	s.egress(1_790_000_000_000, denyAt, "api.anthropic.com", "CONNECT", gen.EgressPayloadDecisionDeny)
	s.add(gen.KindSubagentDone, "subagent:claude-code:1", gen.DonePayload{Status: gen.DonePayloadStatusStopped, Result: "stopped"})
	return s.events
}

// T27(c) 완료 기준: Run B 패턴 → 경고 정확히 1줄.
func TestRetryLoopWarningRunBPattern(t *testing.T) {
	lines, err := RetryLoopWarnings(newRetrySession(false, runBDenyAtMs), DefaultRetryLoopDenyThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != runBWarning {
		t.Fatalf("warnings=%q\nwant=%q", lines, runBWarning)
	}
}

// 정상 세션(Run A3 형태: ready 이후 message/tool_call 존재) → 무경고.
func TestRetryLoopWarningNormalSessionSilent(t *testing.T) {
	lines, err := RetryLoopWarnings(newRetrySession(true, runBDenyAtMs), DefaultRetryLoopDenyThreshold)
	if err != nil || len(lines) != 0 {
		t.Fatalf("정상 세션 경고=%q err=%v", lines, err)
	}
}

// deny는 많으나 간격이 백오프(비감소) 패턴이 아님 → 무경고.
func TestRetryLoopWarningNonBackoffDeniesSilent(t *testing.T) {
	for name, at := range map[string][]int64{
		"irregular":        {0, 4000, 4500, 9000, 9100, 15000, 15200, 30000, 30100, 31000},
		"shrinking":        {0, 8000, 12000, 14000, 15000, 15500},
		"out of order ts":  {0, 500, 1500, 1000, 8000, 16000},
		"one late shorter": {0, 500, 1500, 3500, 8000, 16000, 28500, 28600},
		// F1: 비감소이지만 엄격 증가가 한 번도 없는 패턴은 백오프가 아니다.
		"fixed interval x4": {0, 1000, 2000, 3000},
		"zero gap burst x4": {0, 0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			lines, err := RetryLoopWarnings(newRetrySession(false, at), DefaultRetryLoopDenyThreshold)
			if err != nil || len(lines) != 0 {
				t.Fatalf("비백오프 deny 경고=%q err=%v", lines, err)
			}
		})
	}
}

// 임계 K 경계·ready 이전 deny·allow·도메인 분리.
func TestRetryLoopWarningThresholdAndScope(t *testing.T) {
	below := runBDenyAtMs[:DefaultRetryLoopDenyThreshold-1]
	if lines, _ := RetryLoopWarnings(newRetrySession(false, below), 0); len(lines) != 0 {
		t.Fatalf("K 미만에서 경고: %q", lines)
	}
	exact := runBDenyAtMs[:DefaultRetryLoopDenyThreshold]
	if lines, _ := RetryLoopWarnings(newRetrySession(false, exact), 0); len(lines) != 1 ||
		!strings.Contains(lines[0], "deny 4건(간격 0.5s→2s)") {
		t.Fatalf("K 정확히에서 경고 불일치: %q", lines)
	}

	// ready 이전 deny, allow, 다른 도메인의 소수 deny는 세지 않는다.
	s := &retrySession{}
	s.add(gen.KindSessionStart, "parent", map[string]any{})
	s.egress(0, runBDenyAtMs, "api.anthropic.com", "CONNECT", gen.EgressPayloadDecisionDeny) // ready 이전
	s.add(gen.KindSubagentReady, "subagent:claude-code:1", gen.ReadyPayload{Grade: gen.ReadyPayloadGradeObservable})
	s.egress(100_000, runBDenyAtMs, "api.anthropic.com", "CONNECT", gen.EgressPayloadDecisionAllow)
	s.egress(200_000, []int64{0, 10, 20}, "statsig.anthropic.com", "CONNECT", gen.EgressPayloadDecisionDeny)
	if lines, _ := RetryLoopWarnings(s.events, 0); len(lines) != 0 {
		t.Fatalf("범위 밖 이벤트로 경고: %q", lines)
	}

	// ready 이후가 없다 → 판정 대상 아님.
	s = &retrySession{}
	s.add(gen.KindSessionStart, "parent", map[string]any{})
	s.egress(0, runBDenyAtMs, "api.anthropic.com", "CONNECT", gen.EgressPayloadDecisionDeny)
	if lines, _ := RetryLoopWarnings(s.events, 0); len(lines) != 0 {
		t.Fatalf("ready 없는 세션 경고: %q", lines)
	}
}

// 결정성: 같은 로그의 재계산은 같은 판정을 낸다 — 입력 슬라이스 순서와 무관하게
// seq 순서로 판정하며, 입력을 변경하지 않는다.
func TestRetryLoopWarningDeterministicOverLogRecomputation(t *testing.T) {
	events := newRetrySession(false, runBDenyAtMs)
	snapshot := append([]gen.EventRecord(nil), events...)
	want, err := DetectRetryLoops(events, DefaultRetryLoopDenyThreshold)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(27))
	for i := 0; i < 200; i++ {
		shuffled := append([]gen.EventRecord(nil), events...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		got, err := DetectRetryLoops(shuffled, DefaultRetryLoopDenyThreshold)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("iter %d 재계산 불일치: got=%+v want=%+v err=%v", i, got, want, err)
		}
	}
	if !reflect.DeepEqual(events, snapshot) {
		t.Fatal("판정 함수가 입력 로그를 변경함")
	}
}

func TestRetryLoopWarningRejectsMalformedEgress(t *testing.T) {
	events := newRetrySession(false, runBDenyAtMs)
	events[len(events)-2].Payload = json.RawMessage(`{"at_ms":"x"}`)
	if _, err := DetectRetryLoops(events, 0); err == nil {
		t.Fatal("손상된 egress payload가 조용히 무시됨")
	}
}
