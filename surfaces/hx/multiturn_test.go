package main

// T25 (SCP-T25-001) surface tests: session_mode acceptance, multiturn argv and
// spawn record, and the relay-socket send_message / events_tail / stop round
// trip over a real Unix socket and a real SQLite session log.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/worldtest"
	"github.com/Eastsidegunn/JANUS/seams/approvalrelay"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
	"github.com/Eastsidegunn/JANUS/seams/subagent/claudecode"
)

func TestProductionSessionModeAcceptance(t *testing.T) {
	f := newProductionFixture(t)
	run := func(req runRequest, endpoint string) (string, error) {
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = runProduction(context.Background(), productionRun{
			RequestBytes: data, ProfilePath: f.profilePath, AcceptRoot: f.acceptRoot,
			Launcher: f.launcher, Stdout: &out, ApprovalEndpoint: endpoint,
		})
		return out.String(), err
	}
	for _, tc := range []struct {
		name, mode, adapter, endpoint, code string
	}{
		{"미지 session_mode", "interactive", "claudecode", "/tmp/x.sock", codeUnsupportedContract},
		{"codex multiturn", "multiturn", "codex", "/tmp/x.sock", codeUnsupportedAdapter},
		{"제어 socket 없는 multiturn", "multiturn", "claudecode", "", codeUnsupportedContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := f.request
			req.SessionMode, req.AdapterID = tc.mode, tc.adapter
			out, err := run(req, tc.endpoint)
			var rerr *runError
			if !errors.As(err, &rerr) || rerr.Code != tc.code {
				t.Fatalf("code=%v want %s\n%s", err, tc.code, out)
			}
		})
	}
	if f.launcher.calls.Load() != 0 {
		t.Fatal("rejected session_mode reached launch")
	}
	// Rejections happened before the claim: the same key still accepts.
	req := f.request
	req.SessionMode = "multiturn"
	if out, err := run(req, "/tmp/unused.sock"); err != nil {
		t.Fatalf("multiturn accept: %v\n%s", err, out)
	}
	if f.launcher.calls.Load() != 1 || f.launcher.last.Request.SessionMode != "multiturn" {
		t.Fatalf("launch=%d mode=%q", f.launcher.calls.Load(), f.launcher.last.Request.SessionMode)
	}
}

func TestWorldLauncherMultiturnArgvBeforePrepare(t *testing.T) {
	cfg := validWorldConfig()
	adapter := cfg.Adapters["claudecode"]
	adapter.AgentArgv = []string{"/opt/agent", "existing-argument"}
	cfg.Adapters["claudecode"] = adapter
	for _, mode := range []string{"", "multiturn"} {
		backend := worldtest.NewFakeBackend(nil)
		sentinel := errors.New("fake prepare boundary")
		backend.FakeSetPrepareError(sentinel)
		launcher := worldLauncher{backend: backend, config: cfg}
		log, err := sqlite.Open(context.Background(), t.TempDir()+"/events.db")
		if err != nil {
			t.Fatal(err)
		}
		in := sessionLaunch{Log: log}
		in.Request.AdapterID = "claudecode"
		in.Request.SessionMode = mode
		in.Request.TaskRef.Instruction = "ls /workspace\n한글 'quotes'"
		if _, err := launcher.Launch(context.Background(), in); !errors.Is(err, sentinel) {
			t.Fatalf("Launch: %v", err)
		}
		log.Close()
		want := claudecode.ContainerArgvFor("/opt/agent", in.Request.TaskRef.Instruction, gen.SubagentSpawnPayloadSessionMode(mode))
		if mode == "" && !reflect.DeepEqual(want, claudecode.ContainerArgv("/opt/agent", in.Request.TaskRef.Instruction)) {
			t.Fatal("oneshot argv source drifted")
		}
		if got := backend.FakePreparedSpecs()[0].AgentArgv(); !reflect.DeepEqual(got, want) {
			t.Fatalf("mode %q argv=%q want=%q", mode, got, want)
		}
	}
}

// SCP-T25-001 §2 forward enforcement: the multiturn path always records
// session_mode in the durable spawn; oneshot keeps the T24 payload without it.
func TestProductionWorldSpawnRecordsSessionMode(t *testing.T) {
	for _, tc := range []struct {
		mode    gen.SubagentSpawnPayloadSessionMode
		adapter string
		want    string
	}{
		{"", "claudecode", ""},
		{gen.SubagentSpawnPayloadSessionModeMultiturn, "claudecode", `"session_mode":"multiturn"`},
		{gen.SubagentSpawnPayloadSessionModeMultiturn, "worldadapter", "reject"},
	} {
		ctx := context.Background()
		lower := t.TempDir()
		log, err := sqlite.Open(ctx, t.TempDir()+"/events.db")
		if err != nil {
			t.Fatal(err)
		}
		digest := "sha256:" + strings.Repeat("c", 64)
		prepared := worldtest.NewFakePreparedLease(world.SpawnMetadata{
			Backend: gen.SubagentSpawnPayloadWorldBackendLocalPodman, ProfileID: "p", ImageDigest: digest,
			Mounts: []gen.SubagentSpawnMount{{SourcePath: lower, TargetPath: gen.SubagentSpawnMountTargetPathWorkspace, Mode: gen.SubagentSpawnMountModeOverlay, UpperRef: "upper"}},
		}, "/host/upper", nil)
		activation := errors.New("stop after spawn commit")
		prepared.FakeSetActivateError(activation)
		effective := world.NewEffectivePolicy(policy.SandboxConfig{ProfileID: "p", Workspace: "/workspace", FSScope: []string{"/workspace"}, Budget: gen.Budget{Tokens: 1, TimeMs: 1, MaxDepth: 1}, Approval: policy.ApprovalManual})
		trace := strings.Repeat("1", 32)
		spec := world.NewSpawnSpec(effective, world.NewImageReference("repo", digest), []string{"agent"}, 0, trace, strings.Repeat("2", 16), world.AgentIdentity{UID: 1000, GID: 1000}, nil)
		_, err = startProductionWorld(ctx, worldLaunch{
			Backend: worldtest.NewFakeBackend(prepared), Writer: log.Writer, TraceID: trace, ParentSpan: strings.Repeat("3", 16),
			SpawnSpec: spec, AdapterCommand: []string{"unused"}, AdapterName: tc.adapter,
			ControlMode: gen.SubagentSpawnPayloadControlModeToolApproval, Instruction: "x", Workspace: lower,
			Budget: effective.Budget(), ProfileID: "p", SessionMode: tc.mode,
		})
		events, _ := log.Reader.ReadFrom(ctx, 1)
		log.Close()
		if tc.want == "reject" {
			if err == nil || !strings.Contains(err.Error(), "claudecode") || len(events) != 0 {
				t.Fatalf("non-claude multiturn: err=%v events=%d", err, len(events))
			}
			continue
		}
		if !errors.Is(err, activation) || len(events) != 1 || events[0].Kind != gen.KindSubagentSpawn {
			t.Fatalf("err=%v events=%v", err, events)
		}
		payload := string(events[0].Payload)
		if tc.want == "" && strings.Contains(payload, "session_mode") {
			t.Fatalf("oneshot spawn payload changed: %s", payload)
		}
		if tc.want != "" && !strings.Contains(payload, tc.want) {
			t.Fatalf("multiturn spawn payload lacks session_mode: %s", payload)
		}
	}
}

func relayCall[T any](t *testing.T, socket string, msg approvalrelay.Message) T {
	t.Helper()
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(c).Encode(msg); err != nil {
		t.Fatal(err)
	}
	var r T
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

const multiturnSurfaceRequestID = "22222222-2222-4222-8222-222222222222"

// T25 (a)(b)(c)(d) through the production relay wiring: selectApprovalDecider
// binds the session before listening, send_message reaches the subagent only
// via sessionMessageHandler (durable user/message first), the follow-up turn's
// tool approval waits on the same relay until an operator decides (no
// automatic allow), events_tail equals the pure projection of the SQLite log,
// and T19 stop keeps its idempotent / already_terminal semantics.
func TestMultiturnRelaySessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	log, err := sqlite.Open(ctx, filepath.Join(dir, "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	trace, root := logd.NewTraceID(), logd.NewSpanID()
	if err := log.Writer.InitBatch(ctx, []gen.EventRecord{{Ts: time.Now().UnixMilli(), TraceID: trace, SpanID: root, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("/tmp", "hxt25-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "control.sock")
	decider, closer, srv, err := selectApprovalDecider(sock, trace, "policy-hash", 10000,
		approvalrelay.SessionControl{SessionID: trace, Multiturn: true, Events: log.Reader.ReadFrom})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	relay := decider.(*approvalrelay.UnixApprovalRelay)

	send := func(text string) approvalrelay.Result {
		return relayCall[approvalrelay.Result](t, sock, approvalrelay.Message{Op: "send_message", SessionID: trace, Text: text})
	}
	tail := func(from int64) approvalrelay.TailResult {
		return relayCall[approvalrelay.TailResult](t, sock, approvalrelay.Message{Op: "events_tail", SessionID: trace, FromSeq: from})
	}
	if r := send("too early"); r.Reason != approvalrelay.ReasonSessionNotReady {
		t.Fatalf("before launch: %+v", r)
	}
	if r := tail(0); r.Status != "ok" || r.Session.State != "pending" || len(r.Events) != 1 {
		t.Fatalf("pre-spawn tail: %+v", r)
	}

	script := `read task
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/message","payload":{"text":"turn-1"},"raw":""}'
read message
printf '%s\n' '{"v":1,"kind":"subagent/message","payload":{"text":"turn-2"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/tool_call","payload":{"call_id":"call-2","name":"Bash","args":{"command":"true"}},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/approval_request","payload":{"request_id":"` + multiturnSurfaceRequestID + `","call_id":"call-2","name":"Bash","args":{"command":"true"}},"raw":"eyJ0b29sX25hbWUiOiJCYXNoIn0="}'
read response
printf '%s' "$response" > "$1"
read stop
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"stopped","result":"turn-2"},"raw":""}'`
	responsePath := filepath.Join(dir, "response.json")
	sub, err := subagent.Spawn(ctx, log.Writer, trace, root, 1, subagent.Spec{
		Adapter: "fake", Command: []string{"/bin/sh", "-c", script, "sh", responsePath}, Instruction: "첫 지시",
		Workspace: "/workspace", Budget: gen.Budget{Tokens: 100, TimeMs: 60000, MaxDepth: 1}, ProfileID: "p",
		Approval: policy.ApprovalManual, Decider: relay, SessionMode: gen.SubagentSpawnPayloadSessionModeMultiturn,
	})
	if err != nil {
		t.Fatal(err)
	}
	relay.SetStopHandler(func(m approvalrelay.Message) { _ = sub.Stop(gen.StopPayloadReason(m.Reason)) }, nil, nil)
	relay.SetMessageHandler(sessionMessageHandler(sub))

	waitKind := func(kind string) approvalrelay.Envelope {
		t.Helper()
		for until := time.Now().Add(3 * time.Second); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
			for _, e := range tail(0).Events {
				if e.Kind == kind {
					return e
				}
			}
		}
		t.Fatalf("%s never emitted", kind)
		return approvalrelay.Envelope{}
	}
	waitKind(string(gen.KindSubagentMessage))
	accepted := send("둘째 턴")
	if accepted.Status != "message_accepted" || accepted.MessageSeq == 0 {
		t.Fatalf("send_message: %+v", accepted)
	}
	request := waitKind(string(gen.KindSubagentApprovalRequest))
	// The follow-up turn's tool approval is pending on the relay: nothing
	// decided it automatically.
	var q approvalrelay.Result
	for until := time.Now().Add(2 * time.Second); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		q = relayCall[approvalrelay.Result](t, sock, approvalrelay.Message{Op: "query", TraceID: trace, SpanID: request.SpanID, RequestID: multiturnSurfaceRequestID})
		if q.Status == "pending" {
			break
		}
	}
	if q.Status != "pending" {
		t.Fatalf("follow-up approval not pending on relay: %+v", q)
	}
	if r := relayCall[approvalrelay.Result](t, sock, approvalrelay.Message{Op: "submit", TraceID: trace, SpanID: request.SpanID, RequestID: multiturnSurfaceRequestID, ResponseID: "resp-t25", Decision: "deny", Reason: "operator"}); r.Status != "decided" {
		t.Fatalf("submit: %+v", r)
	}
	decision := waitKind(string(gen.KindPolicyDecision))

	stop := approvalrelay.Message{Op: "stop", TraceID: trace, StopID: "t25-stop", Reason: "user"}
	if r := relayCall[approvalrelay.Result](t, sock, stop); r.Status != "stop_accepted" {
		t.Fatalf("stop: %+v", r)
	}
	done, err := sub.Wait(ctx)
	if err != nil || done.Status != gen.DonePayloadStatusStopped {
		t.Fatalf("done=%+v err=%v", done, err)
	}
	if r := send("after stop"); r.Reason != approvalrelay.ReasonSessionTerminal {
		t.Fatalf("send after stop: %+v", r)
	}
	if r := relayCall[approvalrelay.Result](t, sock, stop); r.Status != "stop_accepted" {
		t.Fatalf("stop idempotence: %+v", r)
	}

	events, err := log.Reader.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var doneSeq int64
	for _, e := range events {
		if e.Kind == gen.KindSubagentDone {
			doneSeq = e.Seq
		}
	}
	srv.MarkTerminal(doneSeq)
	if r := relayCall[approvalrelay.Result](t, sock, approvalrelay.Message{Op: "stop", TraceID: trace, StopID: "t25-late", Reason: "user"}); r.Status != "already_terminal" || r.TerminalRef != doneSeq {
		t.Fatalf("already_terminal: %+v", r)
	}

	// (a) contiguous seq with existing kinds after the durable user/message.
	user := events[accepted.MessageSeq-1]
	if user.Kind != gen.KindUserMessage || user.Actor != "parent" || string(user.Payload) != `{"text":"둘째 턴"}` {
		t.Fatalf("user/message=%+v", user)
	}
	var follow []gen.Kind
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq gap at %d", i)
		}
		if e.Seq > accepted.MessageSeq && e.Seq < decision.Seq {
			follow = append(follow, e.Kind)
		}
	}
	if want := []gen.Kind{gen.KindSubagentMessage, gen.KindSubagentToolCall, gen.KindSubagentApprovalRequest}; !reflect.DeepEqual(follow, want) {
		t.Fatalf("follow-up kinds=%v want=%v", follow, want)
	}
	var policyPayload gen.PolicyDecisionPayload
	if err := json.Unmarshal(events[decision.Seq-1].Payload, &policyPayload); err != nil || policyPayload.Decision != gen.PolicyDecisionPayloadDecisionDeny {
		t.Fatalf("policy decision=%s err=%v", events[decision.Seq-1].Payload, err)
	}
	// (d) the socket projection equals a fresh recomputation of the log.
	got := tail(0)
	want, err := approvalrelay.ProjectSession(events, trace, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events_tail != ProjectSession(log)\ngot:  %+v\nwant: %+v", got, want)
	}
	if got.Session.State != "exited" || got.Session.DoneStatus != "stopped" || got.Session.SessionMode != "multiturn" {
		t.Fatalf("status=%+v", got.Session)
	}
	if after := tail(accepted.MessageSeq); len(after.Events) == 0 || after.Events[0].Seq != accepted.MessageSeq+1 {
		t.Fatalf("cursor tail: %+v", after.Events)
	}
	last, _ := log.Reader.LastSeq(ctx)
	if last != int64(len(events)) {
		t.Fatalf("relay reads wrote to the log: last=%d events=%d", last, len(events))
	}
}

func TestSessionMessageHandlerMapsSeamRejections(t *testing.T) {
	ctx := context.Background()
	log, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "s.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	script := `read task
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
read stop
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"stopped","result":"x"},"raw":""}'`
	sub, err := subagent.Spawn(ctx, log.Writer, logd.NewTraceID(), logd.NewSpanID(), 1, subagent.Spec{
		Adapter: "fake", Command: []string{"/bin/sh", "-c", script}, Instruction: "x", Workspace: "/workspace",
		Budget: gen.Budget{Tokens: 1, TimeMs: 1000, MaxDepth: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionMessageHandler(sub)(ctx, "x"); !errors.Is(err, approvalrelay.ErrNotMultiturn) {
		t.Fatalf("oneshot: %v", err)
	}
	_ = sub.Stop(gen.StopPayloadReasonUser)
	if _, err := sub.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
