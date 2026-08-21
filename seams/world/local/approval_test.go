package local

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

type testAdapterSession struct {
	conn    net.Conn
	encoder *json.Encoder
	decoder *json.Decoder
}

type testHookResult struct {
	decision approvalRelayDecision
	err      error
}

func TestApprovalRelayMatchesOneShotAndAuditsDuplicate(t *testing.T) {
	b := newTestApprovalBroker(t, 8, time.Second)
	sendTestIntent(t, b, "call-1", "Write", json.RawMessage(`{"b":2,"a":1}`))

	firstAdapter := openTestAdapter(t, b)
	firstHook := startTestHook(t, b, hookRaw("call-1", "Write", `{"a":1,"b":2}`))
	first := firstAdapter.readHook(t)
	if first.Reason != nil || first.RequestID == "" {
		t.Fatalf("matching hook가 강제 deny됨: %+v", first)
	}
	firstAdapter.decide(t, first.RequestID, "allow", "")
	if got := receiveHookResult(t, firstHook); got.Decision != "allow" {
		t.Fatalf("matching hook decision=%+v", got)
	}
	firstAdapter.expectDelivered(t)

	duplicateAdapter := openTestAdapter(t, b)
	duplicateHook := startTestHook(t, b, hookRaw("call-1", "Write", `{"b":2,"a":1}`))
	duplicate := duplicateAdapter.readHook(t)
	if duplicate.RequestID == first.RequestID || duplicate.Reason == nil || *duplicate.Reason != "duplicate tool intent" {
		t.Fatalf("duplicate 상관/사유 이상: first=%+v duplicate=%+v", first, duplicate)
	}
	duplicateAdapter.decide(t, duplicate.RequestID, "deny", "duplicate tool intent")
	if got := receiveHookResult(t, duplicateHook); got.Decision != "deny" || got.Reason == nil || *got.Reason != "duplicate tool intent" {
		t.Fatalf("duplicate hook가 강제 deny되지 않음: %+v", got)
	}
	duplicateAdapter.expectDelivered(t)
}

func TestApprovalHookBeforeIntentWaitsWithoutAllowAndThenMatches(t *testing.T) {
	b := newTestApprovalBroker(t, 8, time.Second)
	adapter := openTestAdapter(t, b)
	hookResult := startTestHook(t, b, hookRaw("call-late", "Write", `{"path":"x"}`))
	waitCondition(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.waiting["call-late"]) == 1
	}, "intent 전 hook pending")
	select {
	case got := <-hookResult:
		t.Fatalf("intent 관측 전에 hook가 진행됨: %+v", got)
	case <-time.After(30 * time.Millisecond):
	}
	sendTestIntent(t, b, "call-late", "Write", json.RawMessage(`{"path":"x"}`))
	hook := adapter.readHook(t)
	if hook.Reason != nil {
		t.Fatalf("나중에 정확히 상관된 intent가 강제 deny됨: %+v", hook)
	}
	adapter.decide(t, hook.RequestID, "allow", "")
	if got := receiveHookResult(t, hookResult); got.Decision != "allow" {
		t.Fatalf("상관 뒤 hook decision=%+v", got)
	}
	adapter.expectDelivered(t)
}

func TestApprovalRelayMismatchAndTimeoutAreForcedDenyDeliveries(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		b := newTestApprovalBroker(t, 8, time.Second)
		sendTestIntent(t, b, "call-1", "Write", json.RawMessage(`{"path":"ok"}`))
		adapter := openTestAdapter(t, b)
		hookResult := startTestHook(t, b, hookRaw("call-1", "Write", `{"path":"other"}`))
		hook := adapter.readHook(t)
		if hook.Reason == nil || *hook.Reason != "tool intent mismatch" {
			t.Fatalf("mismatch reason=%v", hook.Reason)
		}
		adapter.decide(t, hook.RequestID, "deny", *hook.Reason)
		if got := receiveHookResult(t, hookResult); got.Decision != "deny" {
			t.Fatalf("mismatch decision=%+v", got)
		}
		adapter.expectDelivered(t)
	})

	t.Run("intent timeout", func(t *testing.T) {
		b := newTestApprovalBroker(t, 8, 80*time.Millisecond)
		adapter := openTestAdapter(t, b)
		hookResult := startTestHook(t, b, hookRaw("missing", "Write", `{}`))
		hook := adapter.readHook(t)
		if hook.Reason == nil || *hook.Reason != "approval deadline 초과" {
			t.Fatalf("timeout reason=%v", hook.Reason)
		}
		adapter.decide(t, hook.RequestID, "deny", *hook.Reason)
		if got := receiveHookResult(t, hookResult); got.Decision != "deny" {
			t.Fatalf("timeout decision=%+v", got)
		}
		adapter.expectDelivered(t)
	})
}

func TestApprovalRelayResourceExhaustionIsFatalAndVisible(t *testing.T) {
	t.Run("intent ledger", func(t *testing.T) {
		b := newTestApprovalBroker(t, 1, time.Second)
		sendTestIntent(t, b, "call-1", "Read", json.RawMessage(`{}`))
		response := sendTestAdapterRequest(t, b, approvalAdapterRequest{
			Operation: "intent", CallID: "call-2", Name: "Read", Args: json.RawMessage(`{}`),
		})
		waitCondition(t, func() bool { return b.Err() != nil }, "ledger 포화 fatal")
		if response.OK || !strings.Contains(response.Error, "ledger 포화") {
			t.Fatalf("ledger 포화가 평범한 deny/성공으로 숨음: response=%+v err=%v", response, b.Err())
		}
	})

	t.Run("pending and rate", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			configure func(*approvalBroker)
			want      string
		}{
			{"pending", func(*approvalBroker) {}, "pending 한도 초과"},
			{"rate", func(b *approvalBroker) { b.rateLimit = 1 }, "요청률 한도 초과"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				b := newTestApprovalBroker(t, 1, time.Second)
				tc.configure(b)
				first := startTestHook(t, b, hookRaw("missing-1", "Read", `{}`))
				waitCondition(t, func() bool {
					b.mu.Lock()
					defer b.mu.Unlock()
					return len(b.hooks) == 1
				}, "첫 approval hook admission")
				second := startTestHook(t, b, hookRaw("missing-2", "Read", `{}`))
				got := receiveHookResult(t, second)
				waitCondition(t, func() bool { return b.Err() != nil }, "approval 고갈 fatal")
				if got.Decision != "deny" || got.Reason == nil || !strings.Contains(*got.Reason, tc.want) {
					t.Fatalf("고갈 상태가 구분되지 않음: decision=%+v broker=%v", got, b.Err())
				}
				if firstGot := receiveHookResult(t, first); firstGot.Decision != "deny" {
					t.Fatalf("fatal 뒤 기존 pending이 deny되지 않음: %+v", firstGot)
				}
			})
		}
	})
}

func TestApprovalAdapterCapabilityAndSpanAreLeaseBound(t *testing.T) {
	b := newTestApprovalBroker(t, 4, time.Second)
	response := sendTestAdapterRequestRaw(t, b, approvalAdapterRequest{
		Operation: "intent", Capability: b.capability, SpanID: "3333333333333333",
		CallID: "call-1", Name: "Read", Args: json.RawMessage(`{}`),
	})
	waitCondition(t, func() bool { return b.Err() != nil }, "capability/span fatal")
	if response.OK || !strings.Contains(response.Error, "capability/span") {
		t.Fatalf("다른 span의 adapter가 수용됨: response=%+v err=%v", response, b.Err())
	}
}

func TestApprovalRelayOversizeIsFatalBeforeAdapterDelivery(t *testing.T) {
	b := newTestApprovalBroker(t, 4, time.Second)
	result := startTestHook(t, b, make([]byte, maxApprovalLineBytes+1))
	select {
	case got := <-result:
		if got.err == nil {
			t.Fatalf("oversize relay 입력이 평범한 decision으로 처리됨: %+v", got.decision)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("oversize relay 종료 대기 timeout")
	}
	waitCondition(t, func() bool { return b.Err() != nil }, "oversize relay fatal")
	if len(b.deliveries) != 0 {
		t.Fatalf("oversize relay 입력이 adapter delivery까지 도달함: %d", len(b.deliveries))
	}
}

func TestApprovalRelayProtocolCannotInjectAdapterOperations(t *testing.T) {
	b := newTestApprovalBroker(t, 4, time.Second)
	conn := dialTestSocket(t, b.relayPath)
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(map[string]any{
		"op": "intent", "raw": hookRaw("call-1", "Read", `{}`),
	}); err != nil {
		t.Fatal(err)
	}
	var response approvalRelayDecision
	if err := json.NewDecoder(conn).Decode(&response); err == nil {
		t.Fatalf("request-only relay가 adapter operation을 decision으로 처리함: %+v", response)
	}
	waitCondition(t, func() bool { return b.Err() != nil }, "relay operation injection fatal")
	if len(b.deliveries) != 0 {
		t.Fatalf("adapter operation injection이 host adapter까지 전달됨: %d", len(b.deliveries))
	}
}

func TestApprovalBrokerShutdownHasDeadlineWithoutAdapter(t *testing.T) {
	b := newTestApprovalBroker(t, 4, 80*time.Millisecond)
	hook := startTestHook(t, b, hookRaw("missing", "Read", `{}`))
	waitCondition(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.hooks) == 1
	}, "shutdown 전 pending hook")
	started := time.Now()
	err := b.Shutdown(context.Background())
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("adapter 없는 Shutdown이 bounded fatal이 아님: elapsed=%v err=%v", time.Since(started), err)
	}
	if got := receiveHookResult(t, hook); got.Decision != "deny" || got.Reason == nil {
		t.Fatalf("deadline shutdown이 hook을 fail-closed deny하지 않음: %+v", got)
	}
}

func newTestApprovalBroker(t *testing.T, capacity int, lifetime time.Duration) *approvalBroker {
	t.Helper()
	b, err := startApprovalBroker(context.Background(), t.TempDir(), "2222222222222222", lifetime.Milliseconds(), capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Cleanup() })
	return b
}

func sendTestIntent(t *testing.T, b *approvalBroker, callID, name string, args json.RawMessage) {
	t.Helper()
	response := sendTestAdapterRequest(t, b, approvalAdapterRequest{Operation: "intent", CallID: callID, Name: name, Args: args})
	if !response.OK {
		t.Fatalf("intent 등록 실패: %+v", response)
	}
}

func sendTestAdapterRequest(t *testing.T, b *approvalBroker, request approvalAdapterRequest) approvalAdapterResponse {
	t.Helper()
	request.Capability, request.SpanID = b.capability, b.spanID
	return sendTestAdapterRequestRaw(t, b, request)
}

func sendTestAdapterRequestRaw(t *testing.T, b *approvalBroker, request approvalAdapterRequest) approvalAdapterResponse {
	t.Helper()
	conn := dialTestSocket(t, b.hostPath)
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response approvalAdapterResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func openTestAdapter(t *testing.T, b *approvalBroker) *testAdapterSession {
	t.Helper()
	conn := dialTestSocket(t, b.hostPath)
	s := &testAdapterSession{conn: conn, encoder: json.NewEncoder(conn), decoder: json.NewDecoder(conn)}
	if err := s.encoder.Encode(approvalAdapterRequest{
		Operation: "next", Capability: b.capability, SpanID: b.spanID,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return s
}

func (s *testAdapterSession) readHook(t *testing.T) approvalHookDelivery {
	t.Helper()
	var response approvalAdapterResponse
	if err := s.decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Hook == nil {
		t.Fatalf("hook delivery 실패: %+v", response)
	}
	return *response.Hook
}

func (s *testAdapterSession) decide(t *testing.T, requestID, decision, reason string) {
	t.Helper()
	value := approvalAdapterDecision{RequestID: requestID, Decision: decision}
	if reason != "" {
		value.Reason = &reason
	}
	if err := s.encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
}

func (s *testAdapterSession) expectDelivered(t *testing.T) {
	t.Helper()
	var response approvalAdapterResponse
	if err := s.decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || !response.Delivered {
		t.Fatalf("hook ACK가 adapter까지 보존되지 않음: %+v", response)
	}
}

func startTestHook(t *testing.T, b *approvalBroker, raw []byte) <-chan testHookResult {
	t.Helper()
	result := make(chan testHookResult, 1)
	go func() {
		conn, err := net.DialTimeout("unix", b.relayPath, time.Second)
		if err != nil {
			result <- testHookResult{err: err}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if err := json.NewEncoder(conn).Encode(approvalRelayRequest{Raw: raw}); err != nil {
			result <- testHookResult{err: err}
			return
		}
		var decision approvalRelayDecision
		if err := json.NewDecoder(conn).Decode(&decision); err != nil {
			result <- testHookResult{err: err}
			return
		}
		if err := json.NewEncoder(conn).Encode(approvalRelayAck{Delivered: true}); err != nil {
			result <- testHookResult{err: err}
			return
		}
		result <- testHookResult{decision: decision}
	}()
	return result
}

func receiveHookResult(t *testing.T, result <-chan testHookResult) approvalRelayDecision {
	t.Helper()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.decision
	case <-time.After(3 * time.Second):
		t.Fatal("hook 결과 대기 timeout")
		return approvalRelayDecision{}
	}
}

func dialTestSocket(t *testing.T, path string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func hookRaw(callID, name, args string) []byte {
	return []byte(`{"hook_event_name":"PreToolUse","tool_use_id":"` + callID + `","tool_name":"` + name + `","tool_input":` + args + `}`)
}
