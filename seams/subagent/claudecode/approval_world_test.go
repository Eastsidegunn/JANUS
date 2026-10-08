package claudecode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/Eastsidegunn/JANUS/core/world/approvalrelaywire"
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
	listener       net.Listener
	relay          net.Listener
	intents        chan worldApprovalRequest
	next           chan *fakeWorldNext
	hooks          chan *fakeWorldHook
	relayFinished  chan error
	deliveredDelay time.Duration
	afterDelivered func()
	done           chan struct{}
	mu             sync.Mutex
	conns          map[net.Conn]struct{}
	wg             sync.WaitGroup
}

type fakeWorldNext struct {
	conn    net.Conn
	encoder *json.Encoder
	decoder *json.Decoder
	done    chan struct{}
}

type fakeWorldHook struct {
	raw       []byte
	decision  chan fakeWorldDecisionResult
	acked     chan error
	delivered chan error
}

type fakeWorldDecisionResult struct {
	decision worldApprovalDecision
	err      error
}

type fakeWorldApprovalBrokerOption func(*fakeWorldApprovalBroker)

// withFakeWorldApprovalRelay makes the approval fake own the same complete
// relay -> next poll -> decision -> hook ACK -> Delivered exchange as the real
// world broker. afterDelivered runs only after the adapter has consumed the
// Delivered response and closed that poll connection.
func withFakeWorldApprovalRelay(delay time.Duration, afterDelivered func()) fakeWorldApprovalBrokerOption {
	return func(b *fakeWorldApprovalBroker) {
		b.hooks = make(chan *fakeWorldHook, 1)
		b.relayFinished = make(chan error, 1)
		b.deliveredDelay = delay
		b.afterDelivered = afterDelivered
	}
}

func TestWorldApprovalClientRegistersIntentAndPreservesForcedHook(t *testing.T) {
	broker := newFakeWorldApprovalBroker(t)
	endpoint := world.NewApprovalEndpoint("unix", broker.listener.Addr().String(), "capability")
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan []byte, 1)
	w := &wireWriter{out: capturedLines{lines: lines}, vals: vals}
	transport, err := newApprovalTransport(w, Config{ApprovalEndpoint: endpoint, WorldSpanID: "2222222222222222"})
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
	endpoint := world.NewApprovalEndpoint("unix", broker.listener.Addr().String(), "capability")
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan []byte, 1)
	transport, err := newApprovalTransport(
		&wireWriter{out: capturedLines{lines: lines}, vals: vals},
		Config{ApprovalEndpoint: endpoint, WorldSpanID: "2222222222222222"},
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
	t.Setenv(worldApprovalNetworkEnv, "unix")
	t.Setenv(worldApprovalAddressEnv, "/host/adapter.sock")
	t.Setenv(worldApprovalCapabilityEnv, "top-secret-capability")
	t.Setenv(worldApprovalSpanEnv, "2222222222222222")
	t.Setenv(approvalSocketEnv, "/host/real-approval.sock")
	t.Setenv(world.ClaudeOAuthTokenEnv, "synthetic-oauth-token")
	cfg := ConfigFromEnv()
	if cfg.ApprovalEndpoint.Network() != "unix" || cfg.ApprovalEndpoint.Address() != "/host/adapter.sock" ||
		cfg.ApprovalEndpoint.Capability() != "top-secret-capability" || cfg.WorldSpanID != "2222222222222222" {
		t.Fatalf("world endpoint env 조립 실패: endpoint=%q/%q cap=%t span=%q",
			cfg.ApprovalEndpoint.Network(), cfg.ApprovalEndpoint.Address(), cfg.ApprovalEndpoint.Capability() != "", cfg.WorldSpanID)
	}
	for _, item := range cfg.Env {
		if strings.Contains(item, "top-secret-capability") || strings.Contains(item, "/host/real-approval.sock") ||
			strings.Contains(item, "synthetic-oauth-token") || strings.HasPrefix(item, worldApprovalNetworkEnv+"=") || strings.HasPrefix(item, worldApprovalSpanEnv+"=") {
			t.Fatalf("host broker/approval capability가 native env에 남음: %q", item)
		}
	}
}

func TestConfigFromEnvReadsProcessEndpointWithoutPassingBrokerInputsToAgent(t *testing.T) {
	t.Setenv(worldProcessNetworkEnv, "unix")
	t.Setenv(worldProcessAddressEnv, "/host/process.sock")
	t.Setenv(worldProcessLeaseEnv, "lease-1")
	t.Setenv(worldProcessControlEnv, "control-capability")
	t.Setenv(worldProcessOutputEnv, "output-capability")
	cfg := ConfigFromEnv()
	if cfg.ProcessEndpoint.Network() != "unix" || cfg.ProcessEndpoint.Address() != "/host/process.sock" ||
		cfg.ProcessEndpoint.LeaseID() != "lease-1" || cfg.ProcessEndpoint.ControlCapability() != "control-capability" ||
		cfg.ProcessEndpoint.OutputCapability() != "output-capability" {
		t.Fatalf("process endpoint env 조립 실패: %#v", cfg.ProcessEndpoint)
	}
	for _, item := range cfg.Env {
		for _, key := range []string{worldProcessNetworkEnv, worldProcessAddressEnv, worldProcessLeaseEnv, worldProcessControlEnv, worldProcessOutputEnv} {
			if strings.HasPrefix(item, key+"=") {
				t.Fatalf("host process endpoint가 native env에 남음: %q", item)
			}
		}
	}
}

func TestAdapterExecutableRegistersRealFixtureToolIntentWithWorldBroker(t *testing.T) {
	broker := newFakeWorldApprovalBroker(t)
	defer broker.Close()
	bins := buildAdapterBinaries(t)
	fixture := filepath.Join(fixtureDir, "02-single-tool.ndjson")
	run := runFixtureProcess(t, bins, fixture, []string{
		testApprovalGateDisabledEnv + "=1",
		worldApprovalNetworkEnv + "=unix",
		worldApprovalAddressEnv + "=" + broker.listener.Addr().String(),
		worldApprovalCapabilityEnv + "=capability",
		worldApprovalSpanEnv + "=2222222222222222",
	}, nil)
	if run.err != nil {
		t.Fatalf("fixture adapter world client 실패: %v\n%s", run.err, run.stderr)
	}
	select {
	case intent := <-broker.intents:
		if intent.CallID != "toolu_01Nn8KS7Rke53sMr4xJMyFCc" || intent.Name == "" || len(intent.Args) == 0 {
			t.Fatalf("T8 fixture native intent가 broker에 정확히 등록되지 않음: %+v", intent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("T8 fixture tool intent 등록 대기 timeout")
	}
}

func newFakeWorldApprovalBroker(t *testing.T, options ...fakeWorldApprovalBrokerOption) *fakeWorldApprovalBroker {
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
	for _, option := range options {
		option(b)
	}
	if b.hooks != nil {
		b.relay, err = net.Listen("unix", filepath.Join(dir, "relay.sock"))
		if err != nil {
			_ = listener.Close()
			_ = os.RemoveAll(dir)
			t.Fatal(err)
		}
		b.wg.Add(1)
		go b.acceptRelay()
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
		if b.hooks != nil {
			b.handleRelayPoll(conn, encoder, decoder)
			return
		}
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

func (b *fakeWorldApprovalBroker) acceptRelay() {
	defer b.wg.Done()
	conn, err := b.relay.Accept()
	if err == nil {
		b.mu.Lock()
		b.conns[conn] = struct{}{}
		b.mu.Unlock()
		err = b.handleRelay(conn)
		b.mu.Lock()
		delete(b.conns, conn)
		b.mu.Unlock()
		_ = conn.Close()
	}
	b.relayFinished <- err
}

func (b *fakeWorldApprovalBroker) handleRelay(conn net.Conn) error {
	decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
	var request approvalrelaywire.Request
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	hook := &fakeWorldHook{
		raw: append([]byte(nil), request.Raw...), decision: make(chan fakeWorldDecisionResult, 1),
		acked: make(chan error, 1), delivered: make(chan error, 1),
	}
	select {
	case b.hooks <- hook:
	case <-b.done:
		return errors.New("fake world approval broker closed")
	}
	var result fakeWorldDecisionResult
	select {
	case result = <-hook.decision:
	case <-b.done:
		return errors.New("fake world approval broker closed")
	}
	if result.err != nil {
		return result.err
	}
	if err := encoder.Encode(approvalrelaywire.Decision{
		Decision: result.decision.Decision, Reason: result.decision.Reason,
	}); err != nil {
		return err
	}
	var ack approvalrelaywire.Ack
	err := decoder.Decode(&ack)
	if err == nil && !ack.Delivered {
		err = errors.New("hxapprove delivered ack was false")
	}
	hook.acked <- err
	if err != nil {
		return err
	}
	select {
	case err = <-hook.delivered:
		return err
	case <-b.done:
		return errors.New("fake world approval broker closed")
	}
}

func (b *fakeWorldApprovalBroker) handleRelayPoll(conn net.Conn, encoder *json.Encoder, decoder *json.Decoder) {
	var hook *fakeWorldHook
	select {
	case hook = <-b.hooks:
	case <-b.done:
		return
	}
	fail := func(err error) {
		select {
		case hook.decision <- fakeWorldDecisionResult{err: err}:
		default:
		}
		select {
		case hook.delivered <- err:
		default:
		}
	}
	const requestID = "33333333-3333-4333-8333-333333333333"
	if err := encoder.Encode(worldApprovalResponse{OK: true, Hook: &worldApprovalHook{
		RequestID: requestID, Raw: hook.raw,
	}}); err != nil {
		fail(err)
		return
	}
	var decision worldApprovalDecision
	if err := decoder.Decode(&decision); err != nil {
		fail(err)
		return
	}
	if decision.RequestID != requestID || decision.Decision != "allow" {
		fail(errors.New("world adapter did not return allow"))
		return
	}
	hook.decision <- fakeWorldDecisionResult{decision: decision}
	select {
	case err := <-hook.acked:
		if err != nil {
			fail(err)
			return
		}
	case <-b.done:
		return
	}
	if b.deliveredDelay > 0 {
		timer := time.NewTimer(b.deliveredDelay)
		select {
		case <-timer.C:
		case <-b.done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
	if err := encoder.Encode(worldApprovalResponse{OK: true, Delivered: true}); err != nil {
		fail(err)
		return
	}
	// Waiting for EOF makes the test process-lifecycle hook deterministic: the
	// adapter decoded Delivered and completed pollOne before fake Claude exits.
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		fail(err)
		return
	}
	hook.delivered <- nil
	if b.afterDelivered != nil {
		b.afterDelivered()
	}
}

func (b *fakeWorldApprovalBroker) RelayAddress() string {
	if b.relay == nil {
		return ""
	}
	return b.relay.Addr().String()
}

func (b *fakeWorldApprovalBroker) Close() {
	select {
	case <-b.done:
		return
	default:
		close(b.done)
	}
	b.listener.Close()
	if b.relay != nil {
		b.relay.Close()
	}
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
