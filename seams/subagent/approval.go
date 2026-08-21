package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

type approvalCoordinator struct {
	sub        *Subagent
	writer     *logd.Writer
	traceID    string
	parentSpan string
	profileID  string
	mode       policy.ApprovalMode
	decider    policy.ApprovalDecider
	ctx        context.Context
	cancel     context.CancelFunc

	mu         sync.Mutex
	pending    map[string]struct{}
	terminated bool
	fatalErr   error
}

func newApprovalCoordinator(parent context.Context, s *Subagent, w *logd.Writer, traceID, parentSpan string, spec Spec) *approvalCoordinator {
	deadline := time.Now().Add(600 * time.Second)
	if spec.Budget.TimeMs < (600 * time.Second).Milliseconds() {
		deadline = time.Now().Add(time.Duration(spec.Budget.TimeMs) * time.Millisecond)
	}
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	a := &approvalCoordinator{
		sub: s, writer: w, traceID: traceID, parentSpan: parentSpan,
		profileID: spec.ProfileID, mode: spec.Approval, decider: spec.Decider,
		ctx: ctx, cancel: cancel, pending: map[string]struct{}{},
	}
	// Process exit and the single effective approval deadline are the only
	// cancellation sources. The deadline is min(parent, remaining budget,
	// 600s), so a hung Decider cannot leave a relay hook pending forever.
	go func() {
		<-s.proc.Done()
		cancel()
	}()
	return a
}

// start registers one request and launches its decision independently of the
// stdout pump. It returns immediately; the pump never waits for a decider.
func (a *approvalCoordinator) start(p gen.ApprovalRequestPayload) error {
	a.mu.Lock()
	if a.terminated {
		a.mu.Unlock()
		return fmt.Errorf("subagent: 승인 조정기가 이미 종료됨")
	}
	if _, exists := a.pending[p.RequestID]; exists {
		a.mu.Unlock()
		err := fmt.Errorf("subagent: 중복 approval request_id %s", p.RequestID)
		a.terminate(p.RequestID, "중복 승인 요청", err, false)
		return err
	}
	a.pending[p.RequestID] = struct{}{}
	a.mu.Unlock()

	req := policy.ApprovalRequest{
		RequestID: p.RequestID, CallID: p.CallID, ToolName: p.Name,
		Args: append(json.RawMessage(nil), p.Args...), SpanID: a.sub.childSpn,
	}
	forcedReason := ""
	if p.Reason != nil {
		forcedReason = *p.Reason
	}
	go a.resolve(req, forcedReason)
	return nil
}

func (a *approvalCoordinator) resolve(req policy.ApprovalRequest, forcedReason string) {
	var decision policy.ApprovalDecision
	var fatal error
	if forcedReason != "" {
		// The world relay has already proven this hook is duplicate, mismatched,
		// expired, or terminating. It still enters the durable approval stream,
		// but must never reach the parent Decider a second time.
		decision = policy.ApprovalDecision{Reason: forcedReason}
	} else {
		decision, fatal = a.decide(req)
	}
	reason := decision.Reason
	payload := gen.PolicyDecisionPayload{
		Decision:  gen.PolicyDecisionPayloadDecisionDeny,
		ProfileID: a.profileID,
	}
	if decision.Allow {
		payload.Decision = gen.PolicyDecisionPayloadDecisionAllow
	}
	if reason != "" {
		payload.Reason = &reason
	}
	encoded, err := json.Marshal(payload)
	if err == nil {
		_, err = a.writer.Submit(context.Background(), gen.EventRecord{
			Ts: now(), TraceID: a.traceID, SpanID: a.sub.childSpn,
			ParentSpanID: &a.parentSpan, Kind: gen.KindPolicyDecision,
			Actor: "parent", Payload: encoded,
		})
	}
	if err != nil {
		a.terminate(req.RequestID, "정책 판정 기록 실패", fmt.Errorf("subagent: policy/decision 기록: %w", err), false)
		return
	}

	response := gen.ApprovalResponsePayload{
		RequestID: req.RequestID,
		Decision:  gen.ApprovalResponsePayloadDecisionDeny,
	}
	if decision.Allow {
		response.Decision = gen.ApprovalResponsePayloadDecisionAllow
	}
	if reason != "" {
		response.Reason = &reason
	}
	if err := a.send(req.RequestID, response); err != nil {
		a.terminate(req.RequestID, "승인 응답 전송 실패", fmt.Errorf("subagent: approval_response 전송: %w", err), true)
		return
	}
	if fatal != nil {
		a.terminate(req.RequestID, reason, fatal, true)
	}
}

func (a *approvalCoordinator) decide(req policy.ApprovalRequest) (policy.ApprovalDecision, error) {
	if err := a.ctx.Err(); err != nil {
		return policy.ApprovalDecision{Reason: "승인 deadline: " + err.Error()}, nil
	}
	if a.mode == policy.ApprovalAuto {
		return policy.ApprovalDecision{Allow: true}, nil
	}
	if a.decider == nil {
		return policy.ApprovalDecision{Reason: "승인 결정자 미배선"}, nil
	}
	decision, err := a.decider.Decide(a.ctx, req)
	if err != nil {
		return policy.ApprovalDecision{Reason: "승인 결정자 오류: " + err.Error()}, nil
	}
	if !decision.Allow && decision.Reason == "" {
		reason := "승인 결정자 계약 위반: deny reason이 비어 있음"
		return policy.ApprovalDecision{Reason: reason}, errors.New(reason)
	}
	return decision, nil
}

// send removes the request from the pending set and delegates all fd
// serialization to Subagent.sendCommand -> procgroup.WriteLine (C-1).
func (a *approvalCoordinator) send(requestID string, response gen.ApprovalResponsePayload) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminated {
		return fmt.Errorf("승인 조정기 종료")
	}
	if _, ok := a.pending[requestID]; !ok {
		return fmt.Errorf("대기하지 않은 request_id %s", requestID)
	}
	delete(a.pending, requestID)
	return a.sub.sendCommand(gen.CommandCmdApprovalResponse, response)
}

// terminate publishes deny for the triggering request first, then every other
// pending request, and only then kills the adapter. This exceptional path is
// deliberately allowed to send deny when durable logging itself has failed;
// it can never send an unlogged allow.
func (a *approvalCoordinator) terminate(currentID, reason string, cause error, currentResponded bool) {
	a.mu.Lock()
	if a.terminated {
		a.mu.Unlock()
		return
	}
	a.terminated = true
	a.fatalErr = cause
	_, currentPending := a.pending[currentID]
	delete(a.pending, currentID)
	others := make([]string, 0, len(a.pending))
	for id := range a.pending {
		others = append(others, id)
	}
	a.pending = map[string]struct{}{}
	a.mu.Unlock()

	var sendErrs []error
	if currentPending && !currentResponded {
		if err := a.sendDeny(currentID, reason); err != nil {
			sendErrs = append(sendErrs, err)
		}
	}
	for _, id := range others {
		if err := a.sendDeny(id, reason); err != nil {
			sendErrs = append(sendErrs, err)
		}
	}
	// A policy stop follows every deny on the same procgroup.WriteLine stream,
	// so the adapter can forward denials to hooks before terminating. Killing
	// immediately here would only prove a pipe write, not delivery. If even the
	// stop cannot be sent, group kill is the final fallback.
	if err := a.sub.sendCommand(gen.CommandCmdStop, gen.StopPayload{Reason: gen.StopPayloadReasonPolicy}); err != nil {
		sendErrs = append(sendErrs, fmt.Errorf("policy stop 전송: %w", err))
		a.sub.proc.Kill()
	}
	if len(sendErrs) > 0 {
		a.mu.Lock()
		a.fatalErr = errors.Join(append([]error{a.fatalErr}, sendErrs...)...)
		a.mu.Unlock()
	}
}

func (a *approvalCoordinator) sendDeny(requestID, reason string) error {
	if reason == "" {
		reason = "승인 처리 실패"
	}
	return a.sub.sendCommand(gen.CommandCmdApprovalResponse, gen.ApprovalResponsePayload{
		RequestID: requestID,
		Decision:  gen.ApprovalResponsePayloadDecisionDeny,
		Reason:    &reason,
	})
}

func (a *approvalCoordinator) fatal() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fatalErr
}
