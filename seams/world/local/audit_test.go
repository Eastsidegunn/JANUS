package local

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/world/local/egressproxy"
)

func TestAuditBrokerBackpressureAndDrainGate(t *testing.T) {
	stateDir := shortTempDir(t)
	value, err := startAuditBroker(stateDir, "2222222222222222", 1)
	if err != nil {
		t.Fatal(err)
	}
	broker := value.(*unixAuditBroker)
	sink := egressproxy.UnixAuditSink{Path: filepath.Join(broker.SocketDir(), auditSocketName)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sink.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := broker.Ready(ctx); err != nil {
		t.Fatal(err)
	}

	first := auditAttempt("first.example", 10, egressproxy.DecisionAllow, "")
	if err := sink.Submit(ctx, first); err != nil {
		t.Fatal(err)
	}
	waitCondition(t, func() bool { return len(broker.queue) == 0 }, "첫 audit가 effect consumer를 기다리는 상태")
	second := auditAttempt("second.example", 20, egressproxy.DecisionDeny, "정책 거부")
	if err := sink.Submit(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := sink.Submit(ctx, auditAttempt("third.example", 30, egressproxy.DecisionAllow, "")); err == nil {
		t.Fatal("bounded audit queue 포화가 성공으로 응답함")
	}

	shutdown := make(chan error, 1)
	go func() { shutdown <- broker.Shutdown(context.Background()) }()
	select {
	case err := <-shutdown:
		t.Fatalf("effect stream drain 전에 shutdown 완료: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	gotFirst := receiveEffect(t, broker.Effects())
	gotSecond := receiveEffect(t, broker.Effects())
	if gotFirst.SpanID != "2222222222222222" || gotFirst.Target != first.Domain ||
		gotFirst.Decision != world.EffectDecisionAllow || gotFirst.RequestBytes != 10 {
		t.Fatalf("첫 effect 변환 이상: %+v", gotFirst)
	}
	if gotSecond.Target != second.Domain || gotSecond.Decision != world.EffectDecisionDeny || gotSecond.Reason != second.Reason {
		t.Fatalf("둘째 effect 변환 이상: %+v", gotSecond)
	}
	if reflect.DeepEqual(gotFirst.ID, gotSecond.ID) || gotFirst.Kind != "egress" || gotSecond.Kind != "egress" {
		t.Fatalf("effect ID/kind 이상: first=%+v second=%+v", gotFirst, gotSecond)
	}
	if err := receiveError(t, shutdown, "audit broker shutdown"); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-broker.Effects():
		if ok {
			t.Fatal("drain 뒤 effect stream이 닫히지 않음")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("effect stream close 대기 timeout")
	}
}

func TestAuditBrokerRejectsMalformedAttemptWithoutEnqueue(t *testing.T) {
	stateDir := shortTempDir(t)
	value, err := startAuditBroker(stateDir, "2222222222222222", 1)
	if err != nil {
		t.Fatal(err)
	}
	broker := value.(*unixAuditBroker)
	sink := egressproxy.UnixAuditSink{Path: filepath.Join(broker.SocketDir(), auditSocketName)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bad := auditAttempt("bad.example", -1, egressproxy.DecisionAllow, "")
	if err := sink.Submit(ctx, bad); err == nil {
		t.Fatal("음수 request_bytes를 broker가 수용함")
	}
	if len(broker.queue) != 0 {
		t.Fatalf("위반 audit가 queue에 들어감: %d", len(broker.queue))
	}
	if err := broker.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func auditAttempt(domain string, size int64, decision egressproxy.Decision, reason string) egressproxy.Attempt {
	return egressproxy.Attempt{
		Domain: domain, Method: "GET", RequestBytes: size,
		AtUnixMs: 1234, Decision: decision, Reason: reason,
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "janus-audit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func receiveEffect(t *testing.T, effects <-chan world.EffectAttempt) world.EffectAttempt {
	t.Helper()
	select {
	case effect, ok := <-effects:
		if !ok {
			t.Fatal("effect stream 조기 종료")
		}
		return effect
	case <-time.After(2 * time.Second):
		t.Fatal("effect 수신 timeout")
		return world.EffectAttempt{}
	}
}

func waitCondition(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("%s 대기 timeout", what)
		}
		time.Sleep(time.Millisecond)
	}
}
