package logd

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// DerivedState는 이벤트 로그의 프로젝션이다 (FR-LOG-04). 모든 필드는
// Replay로 언제든 로그에서 재계산 가능하며, 동일 시퀀스의 재생은 동일
// 상태를 산출한다(FR-LOG-06 — 속성 테스트로 CI에서 검증).
type DerivedState struct {
	TraceID  string
	RootSpan string
	LastSeq  int64
	// Origin은 포크 세션의 원본 참조다 (FR-LOG-05). 포크가 아니면 nil.
	Origin *gen.SessionForkPayload
	// Messages는 부모 모델 가시 히스토리다 (FR-LOG-03) — 이 목록만으로
	// 모델 요청에 포함되는 내용을 재구성한다. 자식 span의 중간 이벤트는
	// 들어가지 않고 subagent/done의 최종 결과만 진입한다(FR-LOG-10).
	Messages []Message
	// Usage는 비용 집계 프로젝션이다: 전체와 actor별.
	UsageIn, UsageOut int64
	UsageByActor      map[string]Usage
	UsageBySpan       map[string]Usage
	UsageBySpanActor  map[string]map[string]Usage
	// 턴/스텝/스폰 카운트와 세션 종료 여부.
	Turns, Steps, Spawns int
	Ended                bool
}

// Message는 모델 가시 히스토리의 한 항목이다.
type Message struct {
	Seq     int64
	Role    Role
	SpanID  string
	Content json.RawMessage // 정규화 payload 원문
}

// Role은 히스토리 항목의 종류다.
type Role string

const (
	RoleUser           Role = "user"
	RoleAssistant      Role = "assistant"
	RoleToolCall       Role = "tool_call"
	RoleToolResult     Role = "tool_result"
	RoleSubagentResult Role = "subagent_result" // 자식의 최종 결과 (FR-LOG-10)
)

// Usage는 토큰 사용량 집계다.
type Usage struct {
	In, Out int64
}

// Replay는 이벤트 시퀀스에서 파생 상태를 재계산하는 순수 함수다.
// 입력을 변형하지 않으며, seq 전순서가 깨진 시퀀스는 거부한다 —
// 전순서는 writer가 보장하는 불변식이므로 위반은 데이터 손상이다.
func Replay(events []gen.EventRecord) (*DerivedState, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("logd: 빈 이벤트 시퀀스는 세션이 아니다")
	}
	s := &DerivedState{
		TraceID:          events[0].TraceID,
		RootSpan:         events[0].SpanID,
		UsageByActor:     map[string]Usage{},
		UsageBySpan:      map[string]Usage{},
		UsageBySpanActor: map[string]map[string]Usage{},
	}
	var prevSeq int64
	for _, e := range events {
		if e.Seq <= prevSeq {
			return nil, fmt.Errorf("logd: seq 전순서 위반 (%d 다음 %d)", prevSeq, e.Seq)
		}
		prevSeq = e.Seq
		if e.TraceID != s.TraceID {
			return nil, fmt.Errorf("logd: 세션 로그에 복수 trace_id (%s, %s)", s.TraceID, e.TraceID)
		}
		s.LastSeq = e.Seq

		if e.UsageIn != nil {
			u := s.UsageByActor[e.Actor]
			if err := addUsage(&s.UsageIn, *e.UsageIn); err != nil {
				return nil, fmt.Errorf("logd: seq %d usage_in: %w", e.Seq, err)
			}
			if err := addUsage(&u.In, *e.UsageIn); err != nil {
				return nil, fmt.Errorf("logd: seq %d actor %s usage_in: %w", e.Seq, e.Actor, err)
			}
			s.UsageByActor[e.Actor] = u
			spanUsage := s.UsageBySpan[e.SpanID]
			if err := addUsage(&spanUsage.In, *e.UsageIn); err != nil {
				return nil, fmt.Errorf("logd: seq %d span %s usage_in: %w", e.Seq, e.SpanID, err)
			}
			s.UsageBySpan[e.SpanID] = spanUsage
			byActor := s.UsageBySpanActor[e.SpanID]
			if byActor == nil {
				byActor = map[string]Usage{}
			}
			actorSpanUsage := byActor[e.Actor]
			if err := addUsage(&actorSpanUsage.In, *e.UsageIn); err != nil {
				return nil, fmt.Errorf("logd: seq %d span %s actor %s usage_in: %w", e.Seq, e.SpanID, e.Actor, err)
			}
			byActor[e.Actor] = actorSpanUsage
			s.UsageBySpanActor[e.SpanID] = byActor
		}
		if e.UsageOut != nil {
			u := s.UsageByActor[e.Actor]
			if err := addUsage(&s.UsageOut, *e.UsageOut); err != nil {
				return nil, fmt.Errorf("logd: seq %d usage_out: %w", e.Seq, err)
			}
			if err := addUsage(&u.Out, *e.UsageOut); err != nil {
				return nil, fmt.Errorf("logd: seq %d actor %s usage_out: %w", e.Seq, e.Actor, err)
			}
			s.UsageByActor[e.Actor] = u
			spanUsage := s.UsageBySpan[e.SpanID]
			if err := addUsage(&spanUsage.Out, *e.UsageOut); err != nil {
				return nil, fmt.Errorf("logd: seq %d span %s usage_out: %w", e.Seq, e.SpanID, err)
			}
			s.UsageBySpan[e.SpanID] = spanUsage
			byActor := s.UsageBySpanActor[e.SpanID]
			if byActor == nil {
				byActor = map[string]Usage{}
			}
			actorSpanUsage := byActor[e.Actor]
			if err := addUsage(&actorSpanUsage.Out, *e.UsageOut); err != nil {
				return nil, fmt.Errorf("logd: seq %d span %s actor %s usage_out: %w", e.Seq, e.SpanID, e.Actor, err)
			}
			byActor[e.Actor] = actorSpanUsage
			s.UsageBySpanActor[e.SpanID] = byActor
		}

		switch e.Kind {
		case gen.KindSessionFork:
			var p gen.SessionForkPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("logd: seq %d session/fork payload: %w", e.Seq, err)
			}
			// 이 세션 자신의 포크 기록은 첫 session/fork다 — 이후에 오는
			// 것은 원본에서 복사된 과거 포크 이력이므로 덮어쓰지 않는다.
			if s.Origin == nil {
				s.Origin = &p
			}
		case gen.KindSessionEnd:
			s.Ended = true
		case gen.KindTurnStart:
			s.Turns++
		case gen.KindStepStart:
			s.Steps++
		case gen.KindSubagentSpawn:
			// payload의 world_backend 판별·분기별 metadata는 writer의 저장 직전
			// contracts 검증으로 확정된다. Replay는 backend와 무관하게 spawn 수를
			// 투영하며 payload 원문은 이벤트 로그에 그대로 남는다.
			s.Spawns++
		case gen.KindUserMessage:
			s.appendMessage(e, RoleUser)
		case gen.KindAssistantMessage:
			s.appendMessage(e, RoleAssistant)
		case gen.KindToolCall:
			s.appendMessage(e, RoleToolCall)
		case gen.KindToolResult:
			s.appendMessage(e, RoleToolResult)
		case gen.KindSubagentDone:
			// 자식의 최종 결과만 부모 컨텍스트에 진입한다 (FR-LOG-10)
			s.appendMessage(e, RoleSubagentResult)
		}
		// 그 외 kind(assistant/chunk, 자식 중간 이벤트, 수집기·정책·훅)는
		// 로그에는 있으나 부모 모델 히스토리에는 포함되지 않는다.
	}
	return s, nil
}

// appendMessage는 부모 모델 가시 항목을 추가한다. 대화·툴 이벤트는
// 루트 span의 것만 히스토리에 들어간다 — 자식 span의 동종 이벤트는
// 자식 trace의 기록일 뿐이다(FR-LOG-10). subagent/done은 예외로
// 자식 span에서 발생하지만 결과가 부모 히스토리에 진입한다.
func (s *DerivedState) appendMessage(e gen.EventRecord, role Role) {
	if role != RoleSubagentResult && e.SpanID != s.RootSpan {
		return
	}
	content := make(json.RawMessage, len(e.Payload))
	copy(content, e.Payload)
	s.Messages = append(s.Messages, Message{
		Seq: e.Seq, Role: role, SpanID: e.SpanID, Content: content,
	})
}

// addUsage는 checked addition이다 — 비용·예산 프로젝션은 fail-closed여야
// 하므로 음수 usage와 int64 overflow(음수 래핑)를 조용히 통과시키지 않는다.
func addUsage(total *int64, v int64) error {
	if v < 0 {
		return fmt.Errorf("음수 usage %d", v)
	}
	if *total > math.MaxInt64-v {
		return fmt.Errorf("usage 합산 int64 overflow (%d + %d)", *total, v)
	}
	*total += v
	return nil
}

// DeriveMessages는 모델 가시 히스토리 프로젝션이다 (FR-LOG-03/04).
func DeriveMessages(events []gen.EventRecord) ([]Message, error) {
	s, err := Replay(events)
	if err != nil {
		return nil, err
	}
	return s.Messages, nil
}

// ReplayReader는 저장된 로그 전체를 읽어 파생 상태를 재계산한다 —
// 캐시된 프로젝션은 언제든 이것으로 재계산 가능해야 한다(FR-LOG-04).
func ReplayReader(ctx context.Context, r Reader) (*DerivedState, error) {
	events, err := r.ReadFrom(ctx, 1)
	if err != nil {
		return nil, err
	}
	return Replay(events)
}
