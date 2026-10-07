package local

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/core/runtimedir"
	"github.com/Eastsidegunn/JANUS/core/world/approvaltiming"
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

func TestApprovalBrokerDeadlineCapsLargeSessionBudget(t *testing.T) {
	b, err := startApprovalBroker(context.Background(), t.TempDir(), "2222222222222222",
		(approvaltiming.ApprovalWaitMax + time.Minute).Milliseconds(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Cleanup()
	if b.deadline.After(time.Now().Add(approvaltiming.ApprovalWaitMax)) {
		t.Fatalf("approval deadline=%s exceeds session window", b.deadline)
	}
}

func TestApprovalBrokerUsesHXRuntimeDir(t *testing.T) {
	runtimeRoot, err := os.MkdirTemp("/tmp", "hxd6-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	t.Setenv("HX_RUNTIME_DIR", runtimeRoot)
	b, err := startApprovalBroker(context.Background(), t.TempDir(), "2222222222222222", 1000, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Cleanup()
	if !strings.HasPrefix(b.rootDir, runtimeRoot+string(filepath.Separator)) {
		t.Fatalf("approval root=%q is outside HX_RUNTIME_DIR=%q", b.rootDir, runtimeRoot)
	}
	for name, path := range map[string]string{"host": b.hostPath, "relay": b.relayPath} {
		const prefix = "hxa-"
		if got := len(path) - len(runtimeRoot) - 1; got-len(filepath.Base(b.rootDir))+len(prefix)+10 > runtimedir.MaxSocketSuffixBytes {
			t.Fatalf("%s socket suffix=%d exceeds %d", name, got, runtimedir.MaxSocketSuffixBytes)
		}
	}
}

// Semantics follow the hook-first post-hoc correlation design decision. This test was
// TestApprovalHookBeforeIntentWaitsWithoutAllowAndThenMatches and asserted that
// a hook arriving before the native intent was parked until the intent came.
// Real claude-code runs the PreToolUse hook before it prints the assistant
// tool_use line, so the intent can never precede the hook's completion and
// parking it deadlocked every world-mode tool call. The hook raw carries
// tool_use_id/tool_name/tool_input, so it is delivered at once and the intent
// is correlated post hoc; a post-hoc mismatch is broker fatal (see
// TestApprovalHookBeforeIntentPostHocMismatchIsBrokerFatal).
func TestApprovalHookBeforeIntentIsDeliveredImmediatelyAndCorrelatedPostHoc(t *testing.T) {
	for _, decision := range []string{"allow", "deny"} {
		t.Run(decision, func(t *testing.T) {
			b := newTestApprovalBroker(t, 8, time.Second)
			adapter := openTestAdapter(t, b)
			sentRaw := hookRaw("call-late", "Write", `{"path":"x","n":1}`)
			hookResult := startTestHook(t, b, sentRaw)
			// No intent has been registered: delivery must not wait for it, and
			// it must reach the adapter as an ordinary request (Reason=nil) whose
			// raw is the native hook input unchanged.
			hook := adapter.readHook(t)
			if hook.Reason != nil || hook.RequestID == "" || !bytes.Equal(hook.Raw, sentRaw) {
				t.Fatalf("intent 전 hook이 평범한 approval request로 전달되지 않음: %+v", hook)
			}
			var delivered approvalNativeInput
			if err := json.Unmarshal(hook.Raw, &delivered); err != nil {
				t.Fatal(err)
			}
			deliveredArgs, err := canonicalObject(delivered.ToolInput)
			if err != nil {
				t.Fatal(err)
			}
			b.mu.Lock()
			pre := b.preIntents["call-late"]
			_, intentSeen := b.intents["call-late"]
			b.mu.Unlock()
			if pre == nil || pre.requestID != hook.RequestID || intentSeen {
				t.Fatalf("hook이 pre-intent로 기록되지 않음: pre=%+v intent=%v", pre, intentSeen)
			}
			reason := ""
			if decision == "deny" {
				reason = "policy deny"
			}
			adapter.decide(t, hook.RequestID, decision, reason)
			if got := receiveHookResult(t, hookResult); got.Decision != decision {
				t.Fatalf("pre-intent hook decision=%+v want %s", got, decision)
			}
			adapter.expectDelivered(t)

			// The native intent arrives afterwards with key-order-different args.
			sendTestIntent(t, b, "call-late", "Write", json.RawMessage(`{"n":1,"path":"x"}`))
			b.mu.Lock()
			_, stillPre := b.preIntents["call-late"]
			intent := b.intents["call-late"]
			b.mu.Unlock()
			if stillPre || intent == nil || !intent.consumed {
				t.Fatalf("사후 상관이 소비되지 않음: pre=%v intent=%+v", stillPre, intent)
			}
			// Post-hoc correlation proof: the args the adapter decided on are the
			// canonical args of the native intent that arrived later.
			if intent.name != delivered.ToolName || !bytes.Equal(intent.canonical, deliveredArgs) {
				t.Fatalf("approval request args와 사후 intent canonical 불일치: delivered=%s/%s intent=%s/%s",
					delivered.ToolName, deliveredArgs, intent.name, intent.canonical)
			}
			if err := b.Err(); err != nil {
				t.Fatalf("일치하는 사후 intent가 broker 오류를 만듦: %v", err)
			}
			if err := b.Shutdown(context.Background()); err != nil {
				t.Fatalf("상관 완료 뒤 Shutdown 오류: %v", err)
			}
			if warnings := b.Warnings(); len(warnings) != 0 {
				t.Fatalf("상관 완료 뒤 미상관 경고가 남음: %v", warnings)
			}
		})
	}
}

func TestApprovalHookBeforeIntentPostHocMismatchIsBrokerFatal(t *testing.T) {
	for _, tc := range []struct {
		name, intentName, intentArgs string
		expireFirst                  bool
	}{
		{name: "name", intentName: "Bash", intentArgs: `{"path":"x"}`},
		{name: "args", intentName: "Write", intentArgs: `{"path":"y"}`},
		{name: "args after deadline", intentName: "Write", intentArgs: `{"path":"y"}`, expireFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestApprovalBroker(t, 8, time.Second)
			adapter := openTestAdapter(t, b)
			firstResult := startTestHook(t, b, hookRaw("call-1", "Write", `{"path":"x"}`))
			first := adapter.readHook(t)
			if first.Reason != nil {
				t.Fatalf("pre-intent hook이 강제 deny됨: %+v", first)
			}
			adapter.decide(t, first.RequestID, "allow", "")
			if got := receiveHookResult(t, firstResult); got.Decision != "allow" {
				t.Fatalf("pre-intent hook decision=%+v", got)
			}
			adapter.expectDelivered(t)

			// Another hook is still pending (no adapter is reading) when the
			// mismatching intent arrives; broker fatal must fail it closed.
			pendingResult := startTestHook(t, b, hookRaw("call-2", "Read", `{}`))
			waitCondition(t, func() bool {
				b.mu.Lock()
				defer b.mu.Unlock()
				return len(b.hooks) == 1
			}, "두 번째 hook admission")
			if tc.expireFirst {
				b.expireAll()
			}
			response := sendTestAdapterRequest(t, b, approvalAdapterRequest{
				Operation: "intent", CallID: "call-1", Name: tc.intentName, Args: json.RawMessage(tc.intentArgs),
			})
			if response.OK || !strings.Contains(response.Error, "post-hoc intent mismatch") {
				t.Fatalf("사후 intent 불일치가 adapter에 오류로 보이지 않음: %+v", response)
			}
			waitCondition(t, func() bool { return b.Err() != nil }, "post-hoc mismatch fatal")
			if err := b.Err(); !strings.Contains(err.Error(), "post-hoc intent mismatch") {
				t.Fatalf("broker fatal 사유에 post-hoc intent mismatch 없음: %v", err)
			}
			if got := receiveHookResult(t, pendingResult); got.Decision != "deny" || got.Reason == nil ||
				*got.Reason != "approval relay fatal" {
				t.Fatalf("fatal 뒤 pending hook이 강제 deny되지 않음: %+v", got)
			}
			if conn, err := net.DialTimeout("unix", b.relayPath, 100*time.Millisecond); err == nil {
				conn.Close()
				t.Fatal("fatal 뒤 relay가 새 hook을 수용함")
			}
		})
	}
}

func TestApprovalDuplicatePreIntentHookIsForcedDeny(t *testing.T) {
	b := newTestApprovalBroker(t, 8, time.Second)
	adapter := openTestAdapter(t, b)
	firstResult := startTestHook(t, b, hookRaw("call-d", "Write", `{"path":"x"}`))
	first := adapter.readHook(t)
	if first.Reason != nil {
		t.Fatalf("첫 pre-intent hook이 강제 deny됨: %+v", first)
	}
	// The second hook for the same call arrives while the first is undecided.
	duplicateAdapter := openTestAdapter(t, b)
	duplicateResult := startTestHook(t, b, hookRaw("call-d", "Write", `{"path":"x"}`))
	duplicate := duplicateAdapter.readHook(t)
	if duplicate.RequestID == first.RequestID || duplicate.Reason == nil || *duplicate.Reason != "duplicate tool intent" {
		t.Fatalf("pre-intent duplicate가 강제 deny되지 않음: first=%+v duplicate=%+v", first, duplicate)
	}
	duplicateAdapter.decide(t, duplicate.RequestID, "allow", "")
	if got := receiveHookResult(t, duplicateResult); got.Decision != "deny" || got.Reason == nil ||
		!strings.Contains(*got.Reason, "강제 deny") {
		t.Fatalf("강제 deny를 allow로 바꾼 adapter가 fatal 처리되지 않음: %+v", got)
	}
	waitCondition(t, func() bool { return b.Err() != nil }, "forced deny violation fatal")
	if got := receiveHookResult(t, firstResult); got.Decision != "deny" {
		t.Fatalf("fatal 뒤 첫 pending hook이 deny되지 않음: %+v", got)
	}
}

func TestApprovalDuplicatePreIntentHookThenMatchingIntent(t *testing.T) {
	b := newTestApprovalBroker(t, 8, time.Second)
	adapter := openTestAdapter(t, b)
	firstResult := startTestHook(t, b, hookRaw("call-d", "Write", `{"path":"x"}`))
	first := adapter.readHook(t)
	adapter.decide(t, first.RequestID, "allow", "")
	if got := receiveHookResult(t, firstResult); got.Decision != "allow" {
		t.Fatalf("첫 pre-intent hook decision=%+v", got)
	}
	adapter.expectDelivered(t)

	duplicateAdapter := openTestAdapter(t, b)
	duplicateResult := startTestHook(t, b, hookRaw("call-d", "Write", `{"path":"x"}`))
	duplicate := duplicateAdapter.readHook(t)
	if duplicate.Reason == nil || *duplicate.Reason != "duplicate tool intent" {
		t.Fatalf("pre-intent duplicate 사유 이상: %+v", duplicate)
	}
	duplicateAdapter.decide(t, duplicate.RequestID, "deny", *duplicate.Reason)
	if got := receiveHookResult(t, duplicateResult); got.Decision != "deny" {
		t.Fatalf("pre-intent duplicate decision=%+v", got)
	}
	duplicateAdapter.expectDelivered(t)

	// The intent correlates with the first (delivered) hook; a later hook for
	// the same call follows the ordinary consumed-intent duplicate rule.
	sendTestIntent(t, b, "call-d", "Write", json.RawMessage(`{"path":"x"}`))
	lateAdapter := openTestAdapter(t, b)
	lateResult := startTestHook(t, b, hookRaw("call-d", "Write", `{"path":"x"}`))
	late := lateAdapter.readHook(t)
	if late.Reason == nil || *late.Reason != "duplicate tool intent" {
		t.Fatalf("상관 뒤 duplicate 사유 이상: %+v", late)
	}
	lateAdapter.decide(t, late.RequestID, "deny", *late.Reason)
	if got := receiveHookResult(t, lateResult); got.Decision != "deny" {
		t.Fatalf("상관 뒤 duplicate decision=%+v", got)
	}
	lateAdapter.expectDelivered(t)
	if err := b.Err(); err != nil {
		t.Fatalf("duplicate 경로가 broker 오류를 만듦: %v", err)
	}
}

func TestApprovalUncorrelatedPreIntentIsObservableAtShutdown(t *testing.T) {
	b := newTestApprovalBroker(t, 8, time.Second)
	var warned strings.Builder
	b.mu.Lock()
	b.warnOut = &warned
	b.mu.Unlock()
	adapter := openTestAdapter(t, b)
	hookResult := startTestHook(t, b, hookRaw("call-orphan", "Write", `{}`))
	hook := adapter.readHook(t)
	adapter.decide(t, hook.RequestID, "deny", "policy deny")
	if got := receiveHookResult(t, hookResult); got.Decision != "deny" {
		t.Fatalf("pre-intent hook decision=%+v", got)
	}
	adapter.expectDelivered(t)
	// The agent dies before printing the tool_use line: no intent ever arrives.
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("미상관 pre-intent가 fatal로 처리됨(경고성이어야 함): %v", err)
	}
	warnings := b.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "call-orphan") || !strings.Contains(warnings[0], hook.RequestID) {
		t.Fatalf("미상관 pre-intent가 관측되지 않음: %v", warnings)
	}
	if !strings.Contains(warned.String(), "call-orphan") {
		t.Fatalf("미상관 pre-intent 경고가 기록되지 않음: %q", warned.String())
	}
	if err := b.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if again := b.Warnings(); len(again) != 1 {
		t.Fatalf("미상관 경고가 중복 기록됨: %v", again)
	}
}

// Hook-first means the native intent always trails the decision, so it can
// race lease Shutdown. A matching late intent must not turn the close into a
// fatal; a mismatching one stays fatal even while closing.
func TestApprovalPostHocIntentDuringShutdown(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		match      bool
	}{
		{name: "match", args: `{"path":"x"}`, match: true},
		{name: "mismatch", args: `{"path":"y"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestApprovalBroker(t, 8, 2*time.Second)
			adapter := openTestAdapter(t, b)
			firstResult := startTestHook(t, b, hookRaw("call-1", "Write", `{"path":"x"}`))
			first := adapter.readHook(t)
			adapter.decide(t, first.RequestID, "allow", "")
			if got := receiveHookResult(t, firstResult); got.Decision != "allow" {
				t.Fatalf("pre-intent hook decision=%+v", got)
			}
			adapter.expectDelivered(t)

			// Keep Shutdown in its drain loop with a second undelivered hook.
			pendingResult := startTestHook(t, b, hookRaw("call-2", "Read", `{}`))
			waitCondition(t, func() bool {
				b.mu.Lock()
				defer b.mu.Unlock()
				return len(b.hooks) == 1
			}, "drain 대상 hook admission")
			shutdownErr := make(chan error, 1)
			go func() { shutdownErr <- b.Shutdown(context.Background()) }()
			waitCondition(t, func() bool {
				b.mu.Lock()
				defer b.mu.Unlock()
				return b.closing
			}, "Shutdown 시작")

			response := sendTestAdapterRequest(t, b, approvalAdapterRequest{
				Operation: "intent", CallID: "call-1", Name: "Write", Args: json.RawMessage(tc.args),
			})
			if !tc.match {
				if response.OK || !strings.Contains(response.Error, "post-hoc intent mismatch") {
					t.Fatalf("종료 중 사후 불일치가 fatal이 아님: %+v", response)
				}
				waitCondition(t, func() bool { return b.Err() != nil }, "종료 중 post-hoc mismatch fatal")
				if got := receiveHookResult(t, pendingResult); got.Decision != "deny" {
					t.Fatalf("fatal 뒤 pending hook이 deny되지 않음: %+v", got)
				}
				select {
				case err := <-shutdownErr:
					if err == nil || !strings.Contains(err.Error(), "post-hoc intent mismatch") {
						t.Fatalf("불일치 fatal이 Shutdown에 드러나지 않음: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("Shutdown 대기 timeout")
				}
				return
			}
			if !response.OK || b.Err() != nil {
				t.Fatalf("종료 중 일치 intent가 오류가 됨: response=%+v err=%v", response, b.Err())
			}
			drainAdapter := openTestAdapter(t, b)
			pending := drainAdapter.readHook(t)
			drainAdapter.decide(t, pending.RequestID, "deny", "policy deny")
			if got := receiveHookResult(t, pendingResult); got.Decision != "deny" {
				t.Fatalf("drain hook decision=%+v", got)
			}
			drainAdapter.expectDelivered(t)
			select {
			case err := <-shutdownErr:
				if err != nil {
					t.Fatalf("종료 중 일치 intent 뒤 Shutdown 오류: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Shutdown 대기 timeout")
			}
			if err := b.Cleanup(); err != nil {
				t.Fatalf("Close(Cleanup) 오류: %v", err)
			}
			if err := b.Err(); err != nil {
				t.Fatalf("종료 중 일치 intent가 broker 오류를 남김: %v", err)
			}
		})
	}
}

func TestApprovalUnconsumedIntentIsNotShutdownError(t *testing.T) {
	b := newTestApprovalBroker(t, 8, time.Second)
	// A native tool_call whose hook never ran (or ran outside the world) leaves
	// an unconsumed intent; lease close must not turn that into an error.
	sendTestIntent(t, b, "call-unused", "Read", json.RawMessage(`{}`))
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("미소비 intent가 Shutdown 오류가 됨: %v", err)
	}
	if warnings := b.Warnings(); len(warnings) != 0 {
		t.Fatalf("미소비 intent가 pre-intent 경고로 기록됨: %v", warnings)
	}
}

func TestApprovalPreIntentCountsAgainstIntentLedger(t *testing.T) {
	b := newTestApprovalBroker(t, 1, time.Second)
	adapter := openTestAdapter(t, b)
	hookResult := startTestHook(t, b, hookRaw("call-1", "Read", `{}`))
	hook := adapter.readHook(t)
	adapter.decide(t, hook.RequestID, "allow", "")
	if got := receiveHookResult(t, hookResult); got.Decision != "allow" {
		t.Fatalf("pre-intent hook decision=%+v", got)
	}
	adapter.expectDelivered(t)
	response := sendTestAdapterRequest(t, b, approvalAdapterRequest{
		Operation: "intent", CallID: "call-2", Name: "Read", Args: json.RawMessage(`{}`),
	})
	waitCondition(t, func() bool { return b.Err() != nil }, "pre-intent 포함 ledger 포화 fatal")
	if response.OK || !strings.Contains(response.Error, "ledger 포화") {
		t.Fatalf("pre-intent 기록이 ledger 한도 밖에 있음: response=%+v err=%v", response, b.Err())
	}
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

	// Was "intent timeout": a hook parked for a missing intent until the
	// deadline. Under the hook-first post-hoc correlation design, no hook is parked for its intent, so the
	// deadline forced deny applies to every hook admitted after expiry,
	// including one whose intent never arrived.
	t.Run("deadline expiry", func(t *testing.T) {
		b := newTestApprovalBroker(t, 8, 80*time.Millisecond)
		adapter := openTestAdapter(t, b)
		waitCondition(t, func() bool {
			b.mu.Lock()
			defer b.mu.Unlock()
			return b.expired
		}, "approval deadline 만료")
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
