package claudecode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/world"
)

type capturedLines struct {
	lines chan []byte
}

func (c capturedLines) Write(value []byte) (int, error) {
	copyValue := append([]byte(nil), value...)
	c.lines <- copyValue
	return len(value), nil
}

type fakeWorldApprovalBroker struct {
	listener net.Listener
	intents  chan worldApprovalRequest
	next     chan *fakeWorldNext
	done     chan struct{}
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

type fakeWorldNext struct {
	conn    net.Conn
	encoder *json.Encoder
	decoder *json.Decoder
	done    chan struct{}
}

func TestWorldApprovalClientRegistersIntentAndPreservesForcedHook(t *testing.T) {
	broker := newFakeWorldApprovalBroker(t)
	endpoint := world.NewEndpoint("unix", broker.listener.Addr().String(), "capability")
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan []byte, 1)
	w := &wireWriter{out: capturedLines{lines: lines}, vals: vals}
	transport, err := newApprovalTransport(w, Config{WorldEndpoint: endpoint, WorldSpanID: "2222222222222222"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var kills atomic.Int64
	transport.attach(done, func() { kills.Add(1) })
	transport.markReady()
	t.Cleanup(func() {
		close(done)
		transport.Close()
		broker.Close()
	})

	intent := gen.AgentToolCallPayload{CallID: "call-1", Name: "Write", Args: json.RawMessage(`{"b":2,"a":1}`)}
	if err := transport.registerIntent(intent); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-broker.intents:
		if got.CallID != intent.CallID || got.Name != intent.Name || !bytes.Equal(got.Args, intent.Args) {
			t.Fatalf("intent wire 이상: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("intent 대기 timeout")
	}

	session := receiveWorldNext(t, broker.next)
	raw := hookRawForWorldTest("call-1", "Write", `{"a":1,"b":2}`)
	reason := "duplicate tool intent"
	requestID := "11111111-1111-4111-8111-111111111111"
	if err := session.encoder.Encode(worldApprovalResponse{OK: true, Hook: &worldApprovalHook{
		RequestID: requestID, Raw: raw, Reason: &reason,
	}}); err != nil {
		t.Fatal(err)
	}

	var event gen.Event
	select {
	case line := <-lines:
		if err := json.Unmarshal(bytes.TrimSpace(line), &event); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approval_request event 대기 timeout")
	}
	if event.Kind != gen.EventKindSubagentApprovalRequest {
		t.Fatalf("kind=%s", event.Kind)
	}
	decodedRaw, err := base64.StdEncoding.DecodeString(event.Raw)
	if err != nil || !bytes.Equal(decodedRaw, raw) {
		t.Fatalf("relay raw 소실: raw=%q err=%v", decodedRaw, err)
	}
	var payload gen.ApprovalRequestPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RequestID != requestID || payload.CallID != "call-1" || payload.Reason == nil || *payload.Reason != reason {
		t.Fatalf("forced approval payload 이상: %+v", payload)
	}
	if err := transport.resolve(gen.ApprovalResponsePayload{
		RequestID: requestID, Decision: gen.ApprovalResponsePayloadDecisionDeny, Reason: &reason,
	}); err != nil {
		t.Fatal(err)
	}
	var decision worldApprovalDecision
	if err := session.decoder.Decode(&decision); err != nil {
		t.Fatal(err)
	}
	if decision.RequestID != requestID || decision.Decision != "deny" || decision.Reason == nil || *decision.Reason != reason {
		t.Fatalf("broker decision 이상: %+v", decision)
	}
	if err := session.encoder.Encode(worldApprovalResponse{OK: true, Delivered: true}); err != nil {
		t.Fatal(err)
	}
	close(session.done)
	if kills.Load() != 0 || transport.failure() != nil {
		t.Fatalf("정상 forced deny가 adapter fatal이 됨: kills=%d err=%v", kills.Load(), transport.failure())
	}
}

func TestWorldApprovalClientCloseUnblocksPendingDecision(t *testing.T) {
	broker := newFakeWorldApprovalBroker(t)
	endpoint := world.NewEndpoint("unix", broker.listener.Addr().String(), "capability")
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan []byte, 1)
	transport, err := newApprovalTransport(
		&wireWriter{out: capturedLines{lines: lines}, vals: vals},
		Config{WorldEndpoint: endpoint, WorldSpanID: "2222222222222222"},
	)
	if err != nil {
		t.Fatal(err)
	}
	transport.markReady()
	session := receiveWorldNext(t, broker.next)
	if err := session.encoder.Encode(worldApprovalResponse{OK: true, Hook: &worldApprovalHook{
		RequestID: "11111111-1111-4111-8111-111111111111",
		Raw:       hookRawForWorldTest("call-1", "Write", `{}`),
	}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lines:
	case <-time.After(2 * time.Second):
		t.Fatal("pending approval event 대기 timeout")
	}
	closed := make(chan struct{})
	go func() {
		transport.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("pending decision 중 world client Close가 정지함")
	}
	broker.Close()
}

func TestConfigFromEnvConsumesWorldCapabilityWithoutPassingItToAgent(t *testing.T) {
	t.Setenv(worldBrokerNetworkEnv, "unix")
	t.Setenv(worldBrokerAddressEnv, "/host/adapter.sock")
	t.Setenv(worldBrokerCapabilityEnv, "top-secret-capability")
	t.Setenv(worldBrokerSpanEnv, "2222222222222222")
	t.Setenv(approvalSocketEnv, "/host/real-approval.sock")
	cfg := ConfigFromEnv()
	if cfg.WorldEndpoint.Network() != "unix" || cfg.WorldEndpoint.Address() != "/host/adapter.sock" ||
		cfg.WorldEndpoint.Capability() != "top-secret-capability" || cfg.WorldSpanID != "2222222222222222" {
		t.Fatalf("world endpoint env 조립 실패: endpoint=%q/%q cap=%t span=%q",
			cfg.WorldEndpoint.Network(), cfg.WorldEndpoint.Address(), cfg.WorldEndpoint.Capability() != "", cfg.WorldSpanID)
	}
	for _, item := range cfg.Env {
		if strings.Contains(item, "top-secret-capability") || strings.Contains(item, "/host/real-approval.sock") ||
			strings.HasPrefix(item, worldBrokerNetworkEnv+"=") || strings.HasPrefix(item, worldBrokerSpanEnv+"=") {
			t.Fatalf("host broker/approval capability가 native env에 남음: %q", item)
		}
	}
}

func TestAdapterExecutableRegistersRealFixtureToolIntentWithWorldBroker(t *testing.T) {
	broker := newFakeWorldApprovalBroker(t)
	defer broker.Close()
	bins := buildAdapterBinaries(t)
	fixture := filepath.Join(fixtureDir, "02-single-tool.ndjson")
	run := runFixtureProcess(t, bins, fixture, []string{
		worldBrokerNetworkEnv + "=unix",
		worldBrokerAddressEnv + "=" + broker.listener.Addr().String(),
		worldBrokerCapabilityEnv + "=capability",
		worldBrokerSpanEnv + "=2222222222222222",
	}, nil)
	if run.err != nil {
		t.Fatalf("fixture adapter world client 실패: %v\n%s", run.err, run.stderr)
	}
	select {
	case intent := <-broker.intents:
		if intent.CallID != "toolu_01LncTj6fEAhfu68H1GFJABE" || intent.Name == "" || len(intent.Args) == 0 {
			t.Fatalf("T8 fixture native intent가 broker에 정확히 등록되지 않음: %+v", intent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("T8 fixture tool intent 등록 대기 timeout")
	}
}

func newFakeWorldApprovalBroker(t *testing.T) *fakeWorldApprovalBroker {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hx-world-client-")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "broker.sock"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	b := &fakeWorldApprovalBroker{
		listener: listener, intents: make(chan worldApprovalRequest, 8),
		next: make(chan *fakeWorldNext, worldApprovalWorkers), done: make(chan struct{}),
		conns: map[net.Conn]struct{}{},
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.conns[conn] = struct{}{}
			b.mu.Unlock()
			b.wg.Add(1)
			go b.handle(conn)
		}
	}()
	t.Cleanup(func() { os.RemoveAll(dir) })
	return b
}

func (b *fakeWorldApprovalBroker) handle(conn net.Conn) {
	defer b.wg.Done()
	defer func() {
		b.mu.Lock()
		delete(b.conns, conn)
		b.mu.Unlock()
		conn.Close()
	}()
	encoder, decoder := json.NewEncoder(conn), json.NewDecoder(conn)
	var request worldApprovalRequest
	if err := decoder.Decode(&request); err != nil {
		return
	}
	if request.Capability != "capability" || request.SpanID != "2222222222222222" {
		_ = encoder.Encode(worldApprovalResponse{Error: "scope mismatch"})
		return
	}
	switch request.Operation {
	case "intent":
		b.intents <- request
		_ = encoder.Encode(worldApprovalResponse{OK: true})
	case "next":
		session := &fakeWorldNext{conn: conn, encoder: encoder, decoder: decoder, done: make(chan struct{})}
		select {
		case b.next <- session:
		case <-b.done:
			return
		}
		select {
		case <-session.done:
		case <-b.done:
		}
	}
}

func (b *fakeWorldApprovalBroker) Close() {
	select {
	case <-b.done:
		return
	default:
		close(b.done)
	}
	b.listener.Close()
	b.mu.Lock()
	connections := make([]net.Conn, 0, len(b.conns))
	for conn := range b.conns {
		connections = append(connections, conn)
	}
	b.mu.Unlock()
	for _, conn := range connections {
		conn.Close()
	}
	b.wg.Wait()
}

func receiveWorldNext(t *testing.T, sessions <-chan *fakeWorldNext) *fakeWorldNext {
	t.Helper()
	select {
	case session := <-sessions:
		return session
	case <-time.After(2 * time.Second):
		t.Fatal("world approval next poll 대기 timeout")
		return nil
	}
}

func hookRawForWorldTest(callID, name, args string) []byte {
	return []byte(`{"hook_event_name":"PreToolUse","tool_use_id":"` + callID + `","tool_name":"` + name + `","tool_input":` + args + `}`)
}
