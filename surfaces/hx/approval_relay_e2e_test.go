package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

func TestApprovalRelayCoordinatorDurableRoundTrip(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "adapter.sh")
	const adapter = "#!/bin/sh\nprintf '%s\\n' '{\"v\":1,\"kind\":\"subagent/ready\",\"payload\":{\"grade\":\"observable\"},\"raw\":\"\"}'\nprintf '%s\\n' '{\"v\":1,\"kind\":\"subagent/approval_request\",\"payload\":{\"request_id\":\"req-e2e\",\"call_id\":\"call-1\",\"name\":\"Bash\",\"args\":{\"command\":\"true\"}},\"raw\":\"\"}'\nwhile IFS= read -r line; do\n  case \"$line\" in *approval_response*) break ;; esac\ndone\nprintf '%s\\n' '{\"v\":1,\"kind\":\"subagent/done\",\"payload\":{\"status\":\"ok\",\"result\":\"done\"},\"raw\":\"\"}'\n"
	if err := os.WriteFile(script, []byte(adapter), 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "session.sqlite")
	log, err := sqlite.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	trace := logd.NewTraceID()
	sock := filepath.Join("/tmp", "hxr-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".sock")
	srv, err := approvalrelay.NewServer(sock, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	listenErr := make(chan error, 1)
	go func() { listenErr <- srv.Listen() }()
	defer func() { _ = srv.Close() }()
	for i := 0; i < 100; i++ {
		if _, e := os.Stat(sock); e == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	relay, err := approvalrelay.NewServerApprovalRelay(srv, approvalrelay.RelayConfig{Endpoint: sock, TraceID: trace, PolicyHash: "p", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-listenErr:
		t.Fatalf("relay listen: %v", e)
	default:
	}
	sub, err := subagent.Spawn(context.Background(), log.Writer, trace, logd.NewSpanID(), 1, subagent.Spec{Adapter: "fake", Command: []string{"/bin/sh", script}, Instruction: "x", Workspace: "/workspace", Budget: gen.Budget{Tokens: 100, TimeMs: 60000, MaxDepth: 1}, ProfileID: "p", Approval: policy.ApprovalManual, Decider: relay})
	if err != nil {
		t.Fatal(err)
	}
	var reqEv gen.EventRecord
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		events, e := log.Reader.ReadFrom(context.Background(), 1)
		if e != nil {
			t.Fatal(e)
		}
		for _, ev := range events {
			if ev.Kind == gen.KindSubagentApprovalRequest {
				reqEv = ev
			}
		}
		if reqEv.Kind != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if reqEv.Kind == "" {
		t.Fatal("approval_request 이벤트 관측 timeout")
	}
	var req gen.SubagentApprovalRequestPayload
	if err := json.Unmarshal(reqEv.Payload, &req); err != nil {
		t.Fatal(err)
	}
	var query approvalrelay.Result
	for until := time.Now().Add(2 * time.Second); time.Now().Before(until); {
		query = socketRelayMessage(t, sock, approvalrelay.Message{Op: "query", TraceID: reqEv.TraceID, SpanID: reqEv.SpanID, RequestID: req.RequestID})
		if query.Status != "unknown" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if query.Status != "pending" || query.RequestDigest == "" || query.PolicyHash != "p" || query.DisplaySummary == "" || query.ExpiresAt == 0 {
		t.Fatalf("pending=%+v", query)
	}
	queryJSON, _ := json.Marshal(query)
	if !strings.HasPrefix(query.RequestDigest, "hx-args-digest-v1:") || strings.Contains(string(queryJSON), "command") || strings.Contains(string(queryJSON), "true") {
		t.Fatal("원문 args 노출")
	}
	accepted := socketRelayMessage(t, sock, approvalrelay.Message{Op: "submit", TraceID: reqEv.TraceID, SpanID: reqEv.SpanID, RequestID: req.RequestID, ResponseID: "resp-e2e", Decision: "allow"})
	if accepted.Status != "decided" {
		t.Fatalf("submit=%+v", accepted)
	}
	if _, err := sub.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := log.Reader.ReadFrom(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, ev := range events {
		if ev.Kind == gen.KindPolicyDecision {
			var p gen.PolicyDecisionPayload
			if json.Unmarshal(ev.Payload, &p) == nil && p.RequestID != nil && *p.RequestID == req.RequestID {
				seq = ev.Seq
				if p.ResponseID == nil || *p.ResponseID != "resp-e2e" || p.ActorRef == nil || *p.ActorRef != "unverified-local-operator" || p.DecisionSource == nil || string(*p.DecisionSource) != "relay" {
					t.Fatalf("durable payload=%+v", p)
				}
			}
		}
	}
	if seq == 0 {
		t.Fatal("policy/decision 이벤트 없음")
	}
	final := socketRelayMessage(t, sock, approvalrelay.Message{Op: "query", TraceID: reqEv.TraceID, SpanID: reqEv.SpanID, RequestID: req.RequestID})
	if final.Status != "decided" || final.ResponseSeq != seq {
		t.Fatalf("final=%+v seq=%d", final, seq)
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
