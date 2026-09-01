package core

// T2: FR-LOG-06(리플레이 결정론)·FR-POL-03(병합 협소성) 속성 테스트의
// 임의 입력 생성기. 속성 테스트 본체는 xfail 태그 파일에 있고 구현 전까지
// 실패가 기대 상태다(make test-xfail). 이 파일의 메타테스트는 생성기가
// 실제로 유효·다양한 입력을 만드는지 지금 검증한다(T2 완료 기준).

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

// ---- 이벤트 시퀀스 생성기 (FR-LOG-06 입력) ----

const hexDigitsNonZero = "123456789abcdef"

// randID는 all-zero가 될 수 없는 소문자 hex ID를 만든다(OTel 규격).
func randID(r *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = hexDigitsNonZero[r.Intn(len(hexDigitsNonZero))]
	}
	return string(b)
}

func randSHA256(r *rand.Rand) string {
	return "sha256:" + randID(r, 64)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func ptr[T any](v T) *T { return &v }

// genEventSequence는 스키마 유효한 세션 이벤트 시퀀스를 생성한다:
// 단일 trace, seq 전순서(strict 증가), ts 단조 비감소,
// session/start로 열고 session/end로 닫으며 사이에 임의의 턴/스텝/훅/
// 서브에이전트 블록/수집기/정책 이벤트를 섞는다.
func genEventSequence(r *rand.Rand) []gen.EventRecord {
	traceID := randID(r, 32)
	rootSpan := randID(r, 16)
	seq := int64(0)
	ts := int64(1_700_000_000_000)
	subagentN := 0
	var events []gen.EventRecord

	add := func(kind gen.Kind, actor, spanID string, parent *string, payload any, mut func(*gen.EventRecord)) {
		seq++
		ts += r.Int63n(1000)
		rec := gen.EventRecord{
			Seq: seq, Ts: ts, TraceID: traceID, SpanID: spanID,
			ParentSpanID: parent, Kind: kind, Actor: actor,
			Payload: mustJSON(payload),
		}
		if mut != nil {
			mut(&rec)
		}
		events = append(events, rec)
	}
	empty := map[string]any{}

	add(gen.KindSessionStart, "parent", rootSpan, nil, empty, nil)
	n := 1 + r.Intn(40)
	for i := 0; i < n; i++ {
		switch r.Intn(12) {
		case 0:
			add(gen.KindTurnStart, "parent", rootSpan, nil, empty, nil)
		case 1:
			add(gen.KindTurnEnd, "parent", rootSpan, nil, empty, nil)
		case 2:
			add(gen.KindStepStart, "parent", rootSpan, nil, empty, nil)
			add(gen.KindStepEnd, "parent", rootSpan, nil, empty, nil)
		case 3:
			add(gen.KindUserMessage, "parent", rootSpan, nil, map[string]any{"text": fmt.Sprintf("입력 %d", i)}, nil)
		case 4:
			add(gen.KindAssistantChunk, "parent", rootSpan, nil, map[string]any{"delta": "…"}, nil)
			add(gen.KindAssistantMessage, "parent", rootSpan, nil, map[string]any{"text": "응답"}, func(e *gen.EventRecord) {
				e.UsageIn = ptr(r.Int63n(10_000))
				e.UsageOut = ptr(r.Int63n(10_000))
			})
		case 5:
			add(gen.KindToolCall, "parent", rootSpan, nil, gen.ToolCallPayload{
				Name: "bash", Args: mustJSON(map[string]any{"cmd": "ls"}),
			}, nil)
			add(gen.KindToolResult, "parent", rootSpan, nil, gen.ToolResultPayload{
				Status: gen.ToolResultPayloadStatusOk,
				Output: mustJSON(map[string]any{"stdout": "ok"}),
			}, func(e *gen.EventRecord) {
				e.Raw = ptr("aGVsbG8=")
			})
		case 6:
			add(gen.KindHookVerdict, "parent", rootSpan, nil, genHookVerdict(r), nil)
		case 7:
			// 서브에이전트 블록: spawn → ready → 중간 이벤트 0..3 → done
			subagentN++
			child := randID(r, 16)
			actor := fmt.Sprintf("subagent:null:%d", subagentN)
			add(gen.KindSubagentSpawn, "parent", child, ptr(rootSpan), gen.SubagentSpawnPayload{
				Adapter: "null", Instruction: "속성 생성기", Depth: 0,
				Budget:       gen.SpawnBudget{Tokens: 100_000, TimeMs: 600_000, MaxDepth: 2},
				WorldBackend: gen.SubagentSpawnPayloadWorldBackendNone,
			}, nil)
			add(gen.KindSubagentReady, actor, child, ptr(rootSpan), gen.SubagentReadyPayload{
				Grade: gen.SubagentReadyPayloadGradeObservable,
			}, func(e *gen.EventRecord) { e.Raw = ptr("") })
			for j := r.Intn(4); j > 0; j-- {
				switch r.Intn(4) {
				case 0:
					add(gen.KindSubagentMessage, actor, child, ptr(rootSpan), gen.SubagentMessagePayload{Text: "진행"}, nil)
				case 1:
					add(gen.KindSubagentToolCall, actor, child, ptr(rootSpan), gen.SubagentToolCallPayload{
						CallID: fmt.Sprintf("call-%d-%d", subagentN, j), Name: "edit",
						Args: mustJSON(map[string]any{"path": "a.txt"}),
					}, nil)
					add(gen.KindSubagentToolResult, actor, child, ptr(rootSpan), gen.SubagentToolResultPayload{
						CallID: fmt.Sprintf("call-%d-%d", subagentN, j),
						Status: gen.SubagentToolResultPayloadStatusOk,
						Output: mustJSON(map[string]any{"ok": true}),
					}, nil)
				case 2:
					add(gen.KindSubagentApprovalRequest, actor, child, ptr(rootSpan), gen.SubagentApprovalRequestPayload{
						RequestID: fmt.Sprintf("req-%d-%d", subagentN, j), CallID: fmt.Sprintf("call-%d-%d", subagentN, j),
						Name: "bash", Args: mustJSON(map[string]any{"command": "rm -rf /"}),
					}, nil)
				case 3:
					add(gen.KindSubagentUsage, actor, child, ptr(rootSpan), empty, func(e *gen.EventRecord) {
						e.UsageIn = ptr(r.Int63n(1_000_000))
						e.UsageOut = ptr(r.Int63n(1_000_000))
					})
				}
			}
			add(gen.KindSubagentDone, actor, child, ptr(rootSpan), gen.SubagentDonePayload{
				Status: gen.SubagentDonePayloadStatusOk, Result: "요약",
			}, nil)
		case 8:
			add(gen.KindPolicyDecision, "parent", rootSpan, nil, gen.PolicyDecisionPayload{
				Decision:  gen.PolicyDecisionPayloadDecisionDeny,
				ProfileID: "opaque-default",
				Reason:    ptr("egress 미허용"),
			}, nil)
		case 9:
			add(gen.KindCollectorFsChanged, "collector", rootSpan, nil, gen.FsChangedPayload{
				Changes: []gen.FsChangedPayloadChangesItem{{
					Path:       fmt.Sprintf("src/f%d.go", i),
					Hash:       randSHA256(r),
					ChangeType: gen.FsChangedPayloadChangesItemChangeTypeModified,
				}},
			}, nil)
		case 10:
			add(gen.KindCollectorEgress, "collector", rootSpan, nil, gen.EgressPayload{
				Domain: "registry.npmjs.org", Method: "GET",
				SizeBytes: r.Int63n(1 << 20), AtMs: ts,
				Decision: gen.EgressPayloadDecisionAllow,
			}, nil)
		case 11:
			add(gen.KindSessionFork, "parent", rootSpan, nil, gen.SessionForkPayload{
				OriginTraceID: randID(r, 32), OriginSeq: 1 + r.Int63n(1000),
			}, nil)
		}
	}
	add(gen.KindSessionEnd, "parent", rootSpan, nil, empty, nil)
	return events
}

func genHookVerdict(r *rand.Rand) gen.HookVerdictPayload {
	points := []gen.HookPoint{gen.HookPointPreStep, gen.HookPointPreTool, gen.HookPointPostTool, gen.HookPointTurnStopping}
	p := gen.HookVerdictPayload{Point: points[r.Intn(len(points))]}
	switch r.Intn(3) {
	case 0:
		p.Verdict = gen.HookVerdictPayloadVerdictContinue
	case 1:
		p.Verdict = gen.HookVerdictPayloadVerdictRewrite
		p.Rewrite = mustJSON(map[string]any{"args": map[string]any{"cmd": "ls -la"}})
		p.Reason = ptr("인자 교정")
	case 2:
		p.Verdict = gen.HookVerdictPayloadVerdictReject
		p.Reason = ptr("정책 위반")
	}
	return p
}

// ---- 프로파일 생성기 (FR-POL-03 입력) ----

// Profile은 T6에서 구현된 core/policy의 실제 프로파일 타입이다
// (T2 시점에는 테스트 로컬 형태였고, T6에서 별칭으로 전환).
type Profile = policy.Profile

var (
	egressPool = []string{
		"registry.npmjs.org", "api.anthropic.com", "github.com",
		"proxy.golang.org", "pypi.org", "index.docker.io", "example.com",
	}
	extensionPolicyPool = []string{"mcp-fs", "mcp-git", "lint@registry.example", "fmt@tools.example"}
	registryPolicyPool  = []string{"registry.example", "tools.example", "registry.npmjs.org"}
	fsPool              = []string{"/workspace", "/workspace/src", "/tmp/scratch", "/data", "/workspace/docs"}
)

func genProfile(r *rand.Rand) Profile {
	pick := func(pool []string) []string {
		out := []string{}
		for _, d := range pool {
			if r.Intn(3) == 0 {
				out = append(out, d)
			}
		}
		return out
	}
	budgets := []int64{0, 1, 100, 10_000, 1_000_000, 1 << 40}
	approval := policy.ApprovalManual
	if r.Intn(2) == 0 {
		approval = policy.ApprovalAuto
	}
	return Profile{
		ID:                fmt.Sprintf("p%d", r.Intn(1000)),
		FSScope:           pick(fsPool),
		Egress:            pick(egressPool),
		AllowedExtensions: pick(extensionPolicyPool),
		AllowedRegistries: pick(registryPolicyPool),
		Budget: gen.Budget{
			Tokens:   budgets[r.Intn(len(budgets))],
			TimeMs:   budgets[r.Intn(len(budgets))],
			MaxDepth: int64(r.Intn(6)),
		},
		Approval: approval,
	}
}

// ---- 생성기 메타테스트 (T2 완료 기준: 생성기가 실제로 생성한다) ----

const generatorSeeds = 100

func TestGeneratorEventSequencesAreSchemaValid(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	kindsSeen := map[gen.Kind]bool{}
	for seed := 0; seed < generatorSeeds; seed++ {
		r := rand.New(rand.NewSource(int64(seed)))
		events := genEventSequence(r)
		if events[0].Kind != gen.KindSessionStart || events[len(events)-1].Kind != gen.KindSessionEnd {
			t.Fatalf("seed %d: 세션 경계 이벤트 누락", seed)
		}
		var prevSeq int64
		for _, e := range events {
			kindsSeen[e.Kind] = true
			if e.Seq <= prevSeq {
				t.Fatalf("seed %d: seq 전순서 위반 (%d 다음 %d)", seed, prevSeq, e.Seq)
			}
			prevSeq = e.Seq
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.ValidateRecord(b); err != nil {
				t.Fatalf("seed %d seq %d (%s): 생성 이벤트가 스키마 위반: %v", seed, e.Seq, e.Kind, err)
			}
		}
	}
	if len(kindsSeen) < 15 {
		t.Errorf("생성기 다양성 부족: kind %d종만 등장 (%v)", len(kindsSeen), kindsSeen)
	}
}

func TestGeneratorProfilesAreDiverse(t *testing.T) {
	var emptyEgress, nonEmptyEgress, auto, manual int
	tokens := map[int64]bool{}
	for seed := 0; seed < 200; seed++ {
		r := rand.New(rand.NewSource(int64(seed)))
		p := genProfile(r)
		if len(p.Egress) == 0 {
			emptyEgress++
		} else {
			nonEmptyEgress++
		}
		if p.Approval == policy.ApprovalAuto {
			auto++
		} else {
			manual++
		}
		tokens[p.Budget.Tokens] = true
	}
	if emptyEgress == 0 || nonEmptyEgress == 0 {
		t.Error("egress 생성 다양성 부족: 빈/비어 있지 않은 allowlist가 모두 나와야 함")
	}
	if auto == 0 || manual == 0 {
		t.Error("승인 모드 다양성 부족")
	}
	if len(tokens) < 4 {
		t.Errorf("예산 다양성 부족: 토큰 예산 %d종", len(tokens))
	}
}

// 생성기 자체도 결정적이어야 한다 — 같은 seed는 같은 시퀀스를 만든다.
// (속성 테스트 실패의 재현 가능성 담보)
func TestGeneratorsAreDeterministic(t *testing.T) {
	for seed := 0; seed < 10; seed++ {
		a := genEventSequence(rand.New(rand.NewSource(int64(seed))))
		b := genEventSequence(rand.New(rand.NewSource(int64(seed))))
		ab, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		if string(ab) != string(bb) {
			t.Fatalf("seed %d: 이벤트 생성기가 비결정적", seed)
		}
	}
}
