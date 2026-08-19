// Package subagent는 서브에이전트 seam이다: 어댑터 프로세스를 호스트 측에서
// 실행하고(FR-ADP-10) §5.2 NDJSON 와이어 프로토콜로 대화하며, 어댑터의
// 정규화 이벤트를 검증해 child span으로 세션 로그에 기록한다.
//
// 어댑터 계약은 spawn / send / events / stop의 최소 집합이다 (FR-ADP-02):
// Spawn이 프로세스를 띄워 task를 보내고, Send가 추가 입력을, Stop이 중단을
// 보내며, 이벤트 스트림은 내부 펌프가 writer로 흘린다. 자식의 중간 이벤트는
// 전부 child span에 기록되지만 부모 모델 히스토리에는 subagent/done의 최종
// 결과만 진입한다(FR-LOG-10 — logd.Replay의 프로젝션 규칙).
package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/seams/subagent/internal/procgroup"
)

// Spec은 spawn 명세다.
type Spec struct {
	Adapter     string   // 어댑터 이름 — actor "subagent:{adapter}:{n}" 구성
	Command     []string // 어댑터 실행 파일과 인자 (호스트 측 실행)
	Instruction string
	Workspace   string
	Budget      gen.Budget // 정책 병합이 끝난 실효 예산 (§5.2)
	Depth       int64      // 현재 spawn 깊이
	ProfileID   string
	Approval    policy.ApprovalMode
	Decider     policy.ApprovalDecider
}

// Subagent는 실행 중인 어댑터 프로세스 핸들이다.
type Subagent struct {
	actor     string
	proc      *procgroup.Process
	vals      *validate.Validators
	doneCh    chan waitResult
	childSpn  string
	approvals *approvalCoordinator
}

type waitResult struct {
	done gen.DonePayload
	err  error
}

// wireEvent는 어댑터 → 코어 NDJSON 한 줄이다 (§5.2 event).
type wireEvent struct {
	V       int64           `json:"v"`
	Kind    gen.Kind        `json:"kind"`
	Payload json.RawMessage `json:"payload"`
	Raw     string          `json:"raw"`
}

// Spawn은 어댑터를 실행하고 task를 보낸 뒤, 이벤트 펌프를 시작한다.
// n은 세션 내 서브에이전트 순번이다. 모든 이벤트는 새 child span으로
// (parent_span_id = parentSpan) writer를 경유해 기록된다.
func Spawn(ctx context.Context, w *logd.Writer, traceID, parentSpan string, n int, spec Spec) (*Subagent, error) {
	vals, err := validate.New()
	if err != nil {
		return nil, err
	}
	childSpan := logd.NewSpanID()
	actor := fmt.Sprintf("subagent:%s:%d", spec.Adapter, n)

	// spawn 이벤트 — 실행 환경 메타데이터의 진입점 (FR-SBX-06은 T10에서 확장)
	spawnPayload, err := json.Marshal(map[string]any{
		"adapter":     spec.Adapter,
		"instruction": spec.Instruction,
		"depth":       spec.Depth,
		"budget":      spec.Budget,
	})
	if err != nil {
		return nil, err
	}
	if _, err := w.Submit(ctx, gen.EventRecord{
		Ts: now(), TraceID: traceID, SpanID: childSpan, ParentSpanID: &parentSpan,
		Kind: gen.KindSubagentSpawn, Actor: "parent", Payload: spawnPayload,
	}); err != nil {
		return nil, fmt.Errorf("subagent: spawn 기록: %w", err)
	}

	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("subagent: 어댑터 명령이 비어 있음")
	}
	// 프로세스·파이프·단일 reap·그룹 kill·EOF drain은 같은 seam의
	// procgroup이 소유한다. exec.CommandContext를 쓰지 않아 watchCtx
	// goroutine이 누적되지 않으며 취소는 항상 프로세스 그룹 전체에 간다.
	proc, err := procgroup.Start(ctx, procgroup.Options{Command: spec.Command})
	if err != nil {
		return nil, fmt.Errorf("subagent: 어댑터 실행: %w", err)
	}

	s := &Subagent{
		actor: actor, proc: proc, vals: vals,
		doneCh: make(chan waitResult, 1), childSpn: childSpan,
	}
	s.approvals = newApprovalCoordinator(s, w, traceID, parentSpan, spec)
	if err := s.sendCommand(gen.CommandCmdTask, gen.TaskPayload{
		Instruction: spec.Instruction,
		Workspace:   spec.Workspace,
		Budget:      spec.Budget,
		Depth:       spec.Depth,
	}); err != nil {
		proc.Kill() // 회수는 procgroup reaper가 수행
		proc.ClosePipes()
		return nil, err
	}
	go s.pump(w, traceID, parentSpan)
	return s, nil
}

// Send는 추가 입력을 어댑터에 전달한다 (§5.2 message).
func (s *Subagent) Send(text string) error {
	return s.sendCommand(gen.CommandCmdMessage, gen.MessagePayload{Text: text})
}

// Stop은 중단을 요청한다 (§5.2 stop). 어댑터는 done(stopped)으로 응답해야 한다.
func (s *Subagent) Stop(reason gen.StopPayloadReason) error {
	return s.sendCommand(gen.CommandCmdStop, gen.StopPayload{Reason: reason})
}

// Wait는 subagent/done까지 기다려 최종 결과를 반환한다. ctx가 먼저
// 끝나면 프로세스 그룹을 kill하고 즉시 반환한다 — 회수(reap)는 reaper
// goroutine이 정확히 한 번 수행하므로 zombie가 남지 않는다.
func (s *Subagent) Wait(ctx context.Context) (gen.DonePayload, error) {
	select {
	case r := <-s.doneCh:
		return r.done, r.err
	case <-ctx.Done():
		s.proc.Kill()
		return gen.DonePayload{}, ctx.Err()
	}
}

// sendCommand는 코어 → 어댑터 명령을 자체 검증(ValidateCommand) 후 보낸다 —
// 코어가 계약 위반 명령을 만들면 어댑터에 도달하기 전에 실패한다.
func (s *Subagent) sendCommand(cmd gen.CommandCmd, payload any) error {
	p, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	line, err := json.Marshal(gen.Command{V: 1, Cmd: cmd, Payload: p})
	if err != nil {
		return err
	}
	if err := s.vals.ValidateCommand(line); err != nil {
		return fmt.Errorf("subagent: 발신 명령이 계약 위반: %w", err)
	}
	return s.proc.WriteLine(line)
}

// pump는 어댑터 stdout의 NDJSON을 검증·정규화해 writer로 흘린다.
//
// §5.2 시퀀스 강제 (FR-ADP-03): 첫 이벤트는 반드시 subagent/ready이고,
// ready 중복·done 중복·done 이후의 어떤 출력도 위반이다. 검증·시퀀스·
// writer 오류는 프로세스 종료(kill) 사유다 — 계약을 어기는 어댑터는
// 등록될 수 없다(§5.2)의 런타임 판.
//
// 프로세스 수명 주기: 종료 관측·회수는 reaper goroutine의 몫이다. 리더
// 종료 시 reaper가 잔여 그룹을 즉시 죽이므로 EOF는 항상 곧 도착하고,
// pump는 진짜 EOF까지 전량 drain한 뒤 결과를 확정하고 부모 측 파이프를
// 닫는다. exit 오류는 결과에 보존된다.
func (s *Subagent) pump(w *logd.Writer, traceID, parentSpan string) {
	var (
		sawReady bool
		done     *gen.DonePayload
	)
	// 이 callback만 §5.2 adapter stdout의 ready/done 시퀀스를 검사한다.
	// Claude CLI의 native stdout에는 §5.2 시퀀스 계약이 없으며, 해당
	// 어댑터는 같은 procgroup drain에 native Parser callback을 연결한다.
	drain := s.proc.DrainLines(4*1024*1024, func(line []byte) error {
		if len(bytes.TrimSpace(line)) == 0 {
			// "모든 메시지는 한 줄 JSON"(§5.2) — 공백 줄은 상태와 무관하게
			// 위반이다 (post-done 공백이 시퀀스 검사를 우회하면 안 된다).
			return fmt.Errorf("subagent: 공백 줄 출력 — §5.2 위반")
		}
		if done != nil {
			return fmt.Errorf("subagent: done 이후 출력 — §5.2 시퀀스 위반")
		}
		if err := s.vals.ValidateEvent(line); err != nil {
			return fmt.Errorf("subagent: 어댑터 이벤트가 §5.2 위반: %w", err)
		}
		var ev wireEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return err
		}
		if ev.Kind == gen.KindSubagentReady {
			if sawReady {
				return fmt.Errorf("subagent: ready 중복 — §5.2 시퀀스 위반")
			}
			sawReady = true
		} else if !sawReady {
			return fmt.Errorf("subagent: ready 전에 %s 수신 — 첫 이벤트는 subagent/ready여야 함 (FR-ADP-03)", ev.Kind)
		}
		rec := gen.EventRecord{
			Ts: now(), TraceID: traceID, SpanID: s.childSpn, ParentSpanID: &parentSpan,
			Kind: ev.Kind, Actor: s.actor, Payload: ev.Payload, Raw: &ev.Raw,
		}
		if ev.Kind == gen.KindSubagentUsage {
			var u gen.UsagePayload
			if err := json.Unmarshal(ev.Payload, &u); err == nil {
				rec.UsageIn, rec.UsageOut = &u.InputTokens, &u.OutputTokens
			}
		}
		if _, err := w.Submit(context.Background(), rec); err != nil {
			return fmt.Errorf("subagent: 이벤트 기록: %w", err)
		}
		if ev.Kind == gen.KindSubagentApprovalRequest {
			var request gen.ApprovalRequestPayload
			if err := json.Unmarshal(ev.Payload, &request); err != nil {
				return err
			}
			if err := s.approvals.start(request); err != nil {
				return err
			}
		}
		if ev.Kind == gen.KindSubagentDone {
			var d gen.DonePayload
			if err := json.Unmarshal(ev.Payload, &d); err != nil {
				return err
			}
			done = &d
			// callback은 성공하지만 drain은 계속된다 — done 이후 출력 감시.
		}
		return nil
	})

	approvalErr := s.approvals.fatal()
	switch {
	case approvalErr != nil:
		s.doneCh <- waitResult{err: approvalErr}
	case drain.HandlerErr != nil:
		s.doneCh <- waitResult{err: drain.HandlerErr}
	case drain.ScanErr != nil:
		s.doneCh <- waitResult{err: drain.ScanErr}
	case done == nil:
		s.doneCh <- waitResult{err: fmt.Errorf("subagent: 어댑터가 subagent/done 없이 종료함 (§5.2 위반, exit: %v)", drain.ExitErr)}
	case drain.ExitErr != nil:
		s.doneCh <- waitResult{err: fmt.Errorf("subagent: done 이후 비정상 종료: %w", drain.ExitErr)}
	default:
		s.doneCh <- waitResult{done: *done}
	}
}

func now() int64 { return time.Now().UnixMilli() }
