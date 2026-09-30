package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

// multiturnAdapterScript is a test-only §5.2 adapter: it requires the
// session-mode handoff, emits one turn, waits for a follow-up message, emits a
// second turn whose tool_use needs approval, and ends on stop.
func multiturnAdapterScript() string {
	return `read task
[ "$HX_SESSION_MODE" = multiturn ] || exit 3
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/message","payload":{"text":"turn-1"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/usage","payload":{"input_tokens":3,"output_tokens":2},"raw":""}'
read message
printf '%s' "$message" > "$1"
printf '%s\n' '{"v":1,"kind":"subagent/message","payload":{"text":"turn-2"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/tool_call","payload":{"call_id":"call-1","name":"Bash","args":{"command":"true"}},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/approval_request","payload":{"request_id":"` + approvalRequestID + `","call_id":"call-1","name":"Bash","args":{"command":"true"}},"raw":"eyJ0b29sX25hbWUiOiJCYXNoIn0="}'
read response
printf '%s' "$response" > "$2"
printf '%s\n' '{"v":1,"kind":"subagent/usage","payload":{"input_tokens":5,"output_tokens":7},"raw":""}'
read stop
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"stopped","result":"turn-2"},"raw":""}'`
}

func waitForKindCount(t *testing.T, store *FakeStore, kind gen.Kind, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, _ := store.ReadFrom(context.Background(), 1)
		count := 0
		for _, e := range events {
			if e.Kind == kind {
				count++
			}
		}
		if count >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s x%d not recorded", kind, n)
}

// T25 (a)(b): send_message on a running multiturn session is recorded as a
// durable user/message (child span, actor parent) before delivery; the
// follow-up turn's message/tool_call keep existing kinds with contiguous seq;
// the follow-up tool_use goes through the unchanged approval coordinator with
// the production DenyAll decider (no automatic allow).
func TestMultiturnSendRecordsUserMessageBeforeDelivery(t *testing.T) {
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	dir := t.TempDir()
	messagePath, responsePath := filepath.Join(dir, "message.json"), filepath.Join(dir, "response.json")
	spec := spawnSpec([]string{"/bin/sh", "-c", multiturnAdapterScript(), "sh", messagePath, responsePath}, "첫 지시")
	spec.SessionMode = gen.SubagentSpawnPayloadSessionModeMultiturn
	spec.ProfileID, spec.Approval, spec.Decider = "profile-1", policy.ApprovalManual, policy.DenyAll{}
	traceID, root := logd.NewTraceID(), logd.NewSpanID()
	if _, err := w.Submit(context.Background(), gen.EventRecord{
		Ts: now(), TraceID: traceID, SpanID: root, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	sub, err := Spawn(context.Background(), w, traceID, root, 1, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !sub.Multiturn() {
		t.Fatal("multiturn spec produced a oneshot subagent")
	}
	waitForKindCount(t, store, gen.KindSubagentUsage, 1)
	seq, err := sub.Send(context.Background(), "둘째 턴")
	if err != nil {
		t.Fatal(err)
	}
	waitForKindCount(t, store, gen.KindSubagentUsage, 2)
	if err := sub.Stop(gen.StopPayloadReasonUser); err != nil {
		t.Fatal(err)
	}
	done, err := sub.Wait(context.Background())
	if err != nil || done.Status != gen.DonePayloadStatusStopped {
		t.Fatalf("done=%+v err=%v", done, err)
	}

	events, _ := store.ReadFrom(context.Background(), 1)
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq gap at index %d: %d", i, e.Seq)
		}
	}
	var spawn gen.SubagentSpawnPayload
	if err := json.Unmarshal(events[1].Payload, &spawn); err != nil || events[1].Kind != gen.KindSubagentSpawn {
		t.Fatalf("spawn event=%s err=%v", events[1].Kind, err)
	}
	if spawn.SessionMode == nil || *spawn.SessionMode != gen.SubagentSpawnPayloadSessionModeMultiturn {
		t.Fatalf("multiturn spawn record lacks session_mode: %s", events[1].Payload)
	}
	user := events[seq-1]
	if user.Kind != gen.KindUserMessage || user.Actor != "parent" || user.SpanID != events[1].SpanID ||
		user.ParentSpanID == nil || *user.ParentSpanID != root || string(user.Payload) != `{"text":"둘째 턴"}` {
		t.Fatalf("user/message record=%+v payload=%s", user, user.Payload)
	}
	var after []gen.Kind
	for _, e := range events[seq:] {
		after = append(after, e.Kind)
	}
	want := []gen.Kind{gen.KindSubagentMessage, gen.KindSubagentToolCall, gen.KindSubagentApprovalRequest, gen.KindPolicyDecision, gen.KindSubagentUsage, gen.KindSubagentDone}
	if strings.Join(kindStrings(after), ",") != strings.Join(kindStrings(want), ",") {
		t.Fatalf("follow-up turn kinds=%v want=%v", after, want)
	}
	var decision gen.PolicyDecisionPayload
	if err := json.Unmarshal(events[seq+3].Payload, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Decision != gen.PolicyDecisionPayloadDecisionDeny {
		t.Fatalf("follow-up tool_use was not denied by DenyAll: %s", events[seq+3].Payload)
	}
	response := decodeApprovalResponse(t, responsePath)
	if response.Decision != gen.ApprovalResponsePayloadDecisionDeny {
		t.Fatalf("approval response=%+v", response)
	}
	b, err := os.ReadFile(messagePath)
	if err != nil {
		t.Fatal(err)
	}
	var cmd gen.Command
	if err := json.Unmarshal(b, &cmd); err != nil || cmd.Cmd != gen.CommandCmdMessage || string(cmd.Payload) != `{"text":"둘째 턴"}` {
		t.Fatalf("delivered command=%s err=%v", b, err)
	}
	// Model-visible input stays out of the parent history (FR-LOG-10).
	messages, err := logd.DeriveMessages(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.Role == logd.RoleUser {
			t.Fatalf("child user/message leaked into parent history: %+v", m)
		}
	}
}

func kindStrings(kinds []gen.Kind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

// T25 (e): send on a oneshot session is rejected deterministically without a
// durable record or a delivered command; the oneshot spawn record carries no
// session_mode (T24 bytes unchanged).
func TestSendRejectsOneshotSessionWithoutRecording(t *testing.T) {
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	messagePath := filepath.Join(t.TempDir(), "message")
	script := `read task
[ -z "$HX_SESSION_MODE" ] || exit 3
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
read next
printf '%s' "$next" > "$1"
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"stopped","result":"x"},"raw":""}'`
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{"/bin/sh", "-c", script, "sh", messagePath}, "지시"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Send(context.Background(), "주입"); !errors.Is(err, ErrNotMultiturn) {
		t.Fatalf("oneshot Send err=%v", err)
	}
	if err := sub.Stop(gen.StopPayloadReasonUser); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := store.ReadFrom(context.Background(), 1)
	for _, e := range events {
		if e.Kind == gen.KindUserMessage {
			t.Fatalf("rejected send left a durable record: %+v", e)
		}
	}
	if strings.Contains(string(events[0].Payload), "session_mode") {
		t.Fatalf("oneshot spawn payload changed: %s", events[0].Payload)
	}
	b, _ := os.ReadFile(messagePath)
	if !strings.Contains(string(b), `"cmd":"stop"`) {
		t.Fatalf("first command after task was not stop: %s", b)
	}
}

// T25 (c)(e): once a multiturn session has been stopped or has reported done,
// Send is rejected deterministically and nothing is recorded.
func TestSendRejectsTerminalMultiturnSession(t *testing.T) {
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	script := `read task
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
read stop
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"stopped","result":"x"},"raw":""}'`
	spec := spawnSpec([]string{"/bin/sh", "-c", script}, "지시")
	spec.SessionMode = gen.SubagentSpawnPayloadSessionModeMultiturn
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Stop(gen.StopPayloadReasonUser); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Send(context.Background(), "늦은 주입"); !errors.Is(err, ErrSessionTerminal) {
		t.Fatalf("Send after stop err=%v", err)
	}
	if _, err := sub.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Send(context.Background(), "종료 후 주입"); !errors.Is(err, ErrSessionTerminal) {
		t.Fatalf("Send after done err=%v", err)
	}
	events, _ := store.ReadFrom(context.Background(), 1)
	for _, e := range events {
		if e.Kind == gen.KindUserMessage {
			t.Fatalf("rejected send left a durable record: %+v", e)
		}
	}
}

func TestSpawnRejectsUnknownSessionMode(t *testing.T) {
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	spec := spawnSpec([]string{"/bin/true"}, "지시")
	spec.SessionMode = "interactive"
	if _, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1, spec); err == nil {
		t.Fatal("unknown session_mode accepted")
	}
}
