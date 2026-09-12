package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/seams/approvalrelay"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
)

type relayHarness struct {
	log         *sqlite.Log
	srv         *approvalrelay.Server
	sock, trace string
	sub         *subagent.Subagent
}

// startRelayHarness is the shared construction point for A/B scenarios.
// The current A body remains unchanged until the next focused refactor turn.
func startRelayHarness(t *testing.T, relayTimeout time.Duration) *relayHarness {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "adapter.sh")
	const adapter = "#!/bin/sh\nprintf '%s\\n' '{\"v\":1,\"kind\":\"subagent/ready\",\"payload\":{\"grade\":\"observable\"},\"raw\":\"\"}'\nprintf '%s\\n' '{\"v\":1,\"kind\":\"subagent/approval_request\",\"payload\":{\"request_id\":\"req-e2e\",\"call_id\":\"call-1\",\"name\":\"Bash\",\"args\":{\"command\":\"true\"}},\"raw\":\"\"}'\nwhile IFS= read -r line; do case \"$line\" in *approval_response*) break ;; esac; done\nprintf '%s\\n' '{\"v\":1,\"kind\":\"subagent/done\",\"payload\":{\"status\":\"ok\",\"result\":\"done\"},\"raw\":\"\"}'\n"
	if err := os.WriteFile(script, []byte(adapter), 0700); err != nil {
		t.Fatal(err)
	}
	log, err := sqlite.Open(context.Background(), filepath.Join(dir, "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	trace := logd.NewTraceID()
	sockDir, err := os.MkdirTemp("/tmp", "hxr-")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(sockDir, "approval.sock")
	srv, err := approvalrelay.NewServer(sock, relayTimeout)
	if err != nil {
		t.Fatal(err)
	}
	listenErr := make(chan error, 1)
	go func() { listenErr <- srv.Listen() }()
	for i := 0; i < 100; i++ {
		if _, e := os.Stat(sock); e == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case e := <-listenErr:
		t.Fatalf("relay listen: %v", e)
	default:
	}
	relay, err := approvalrelay.NewServerApprovalRelay(srv, approvalrelay.RelayConfig{Endpoint: sock, TraceID: trace, PolicyHash: "p", Timeout: relayTimeout})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := subagent.Spawn(context.Background(), log.Writer, trace, logd.NewSpanID(), 1, subagent.Spec{Adapter: "fake", Command: []string{"/bin/sh", script}, Instruction: "x", Workspace: "/workspace", Budget: gen.Budget{Tokens: 100, TimeMs: 60000, MaxDepth: 1}, ProfileID: "p", Approval: policy.ApprovalManual, Decider: relay})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub; _ = log.Close(); _ = srv.Close(); _ = os.RemoveAll(sockDir) })
	return &relayHarness{log: log, srv: srv, sock: sock, trace: trace, sub: sub}
}

func (h *relayHarness) events(t *testing.T) []gen.EventRecord {
	t.Helper()
	e, err := h.log.Reader.ReadFrom(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func (h *relayHarness) message(t *testing.T, msg approvalrelay.Message) approvalrelay.Result {
	return socketRelayMessage(t, h.sock, msg)
}
func (h *relayHarness) awaitApprovalRequest(t *testing.T) (gen.EventRecord, gen.SubagentApprovalRequestPayload) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range h.events(t) {
			if e.Kind == gen.KindSubagentApprovalRequest {
				var p gen.SubagentApprovalRequestPayload
				if json.Unmarshal(e.Payload, &p) == nil {
					return e, p
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("approval_request 이벤트 관측 timeout")
	return gen.EventRecord{}, gen.SubagentApprovalRequestPayload{}
}

func TestApprovalRelayCoordinatorDurableRoundTrip(t *testing.T) {
	h := startRelayHarness(t, 10*time.Second)
	ev, req := h.awaitApprovalRequest(t)
	var q approvalrelay.Result
	for until := time.Now().Add(2 * time.Second); time.Now().Before(until); {
		q = h.message(t, approvalrelay.Message{Op: "query", TraceID: ev.TraceID, SpanID: ev.SpanID, RequestID: req.RequestID})
		if q.Status != "unknown" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if q.Status != "pending" || !strings.HasPrefix(q.RequestDigest, "hx-args-digest-v1:") {
		t.Fatalf("pending=%+v", q)
	}
	raw, _ := json.Marshal(q)
	if strings.Contains(string(raw), "command") || strings.Contains(string(raw), "true") {
		t.Fatal("원문 args 노출")
	}
	if r := h.message(t, approvalrelay.Message{Op: "submit", TraceID: ev.TraceID, SpanID: ev.SpanID, RequestID: req.RequestID, ResponseID: "resp-e2e", Decision: "allow"}); r.Status != "decided" {
		t.Fatalf("submit=%+v", r)
	}
	if _, err := h.sub.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, e := range h.events(t) {
		if e.Kind == gen.KindPolicyDecision {
			var p gen.PolicyDecisionPayload
			json.Unmarshal(e.Payload, &p)
			if p.RequestID != nil && *p.RequestID == req.RequestID {
				seq = e.Seq
				if p.ResponseID == nil || p.ActorRef == nil || p.DecisionSource == nil {
					t.Fatalf("payload=%+v", p)
				}
			}
		}
	}
	if seq == 0 {
		t.Fatal("policy decision missing")
	}
	if r := h.message(t, approvalrelay.Message{Op: "query", TraceID: ev.TraceID, SpanID: ev.SpanID, RequestID: req.RequestID}); r.ResponseSeq != seq {
		t.Fatalf("seq=%+v want %d", r, seq)
	}
}

func socketRelayMessage(t *testing.T, socket string, msg approvalrelay.Message) approvalrelay.Result {
	t.Helper()
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(msg); err != nil {
		t.Fatal(err)
	}
	var r approvalrelay.Result
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}
