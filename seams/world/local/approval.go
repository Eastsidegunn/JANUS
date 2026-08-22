package local

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/approvalwire"
)

const (
	approvalHostSocketName   = "adapter.sock"
	approvalRelaySocketName  = "approve.sock"
	approvalRelayMount       = "/run/hx"
	approvalRelayPath        = approvalRelayMount + "/" + approvalRelaySocketName
	defaultApprovalCapacity  = 64
	defaultApprovalRate      = 128
	maxApprovalLineBytes     = 4 << 20
	maxApprovalEnvelopeBytes = 8 << 20
	maxApprovalLifetime      = 600 * time.Second
)

// The adapter endpoint and the container relay deliberately use different
// protocols and sockets. Only the request-only relay is mounted into the
// agent; capability-bearing adapter operations never cross the sandbox.
type approvalAdapterRequest = approvalwire.Request
type approvalAdapterResponse = approvalwire.Response
type approvalHookDelivery = approvalwire.Hook
type approvalAdapterDecision = approvalwire.Decision

type approvalRelayRequest struct {
	Raw []byte `json:"raw"`
}

type approvalRelayDecision struct {
	Decision string  `json:"decision"`
	Reason   *string `json:"reason,omitempty"`
}

type approvalRelayAck struct {
	Delivered bool `json:"delivered"`
}

type approvalNativeInput struct {
	HookEventName string          `json:"hook_event_name"`
	ToolUseID     string          `json:"tool_use_id"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

type approvalIntent struct {
	callID    string
	name      string
	canonical []byte
	consumed  bool
}

type approvalPendingHook struct {
	requestID string
	callID    string
	name      string
	canonical []byte
	raw       []byte
	reason    *string
	conn      net.Conn
	decoder   *json.Decoder
	done      chan struct{}
	once      sync.Once
}

type approvalBroker struct {
	spanID     string
	capability string
	rootDir    string
	hostDir    string
	relayDir   string
	hostPath   string
	relayPath  string
	host       net.Listener
	relay      net.Listener
	capacity   int
	rateLimit  int
	deadline   time.Time

	mu          sync.Mutex
	intents     map[string]*approvalIntent
	waiting     map[string][]*approvalPendingHook
	hooks       map[string]*approvalPendingHook
	conns       map[net.Conn]struct{}
	deliveries  chan *approvalPendingHook
	windowStart time.Time
	windowCount int
	closing     bool
	failed      bool
	expired     bool
	firstErr    error
	done        chan struct{}
	doneOnce    sync.Once
	wg          sync.WaitGroup
}

func startApprovalBroker(parent context.Context, _ string, spanID string, budgetMs int64, capacity int) (*approvalBroker, error) {
	if capacity == 0 {
		capacity = defaultApprovalCapacity
	}
	if capacity < 1 || !spanPattern.MatchString(spanID) || budgetMs <= 0 {
		return nil, fmt.Errorf("world/local: approval broker 설정 위반")
	}
	deadline := time.Now().Add(maxApprovalLifetime)
	if budgetMs < maxApprovalLifetime.Milliseconds() {
		deadline = time.Now().Add(time.Duration(budgetMs) * time.Millisecond)
	}
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	if !deadline.After(time.Now()) {
		return nil, fmt.Errorf("world/local: approval deadline이 이미 만료됨")
	}

	capabilityBytes := make([]byte, 32)
	if _, err := rand.Read(capabilityBytes); err != nil {
		return nil, fmt.Errorf("world/local: approval capability 발급: %w", err)
	}
	// Darwin's Unix socket path limit is shorter than a normal t.TempDir-based
	// world state path. A short per-lease 0700 root is also a tighter boundary:
	// only its relay child is mounted, while the host adapter socket stays a
	// sibling that the agent cannot traverse through that mount.
	rootDir, err := os.MkdirTemp("/tmp", "hxa-")
	if err != nil {
		return nil, fmt.Errorf("world/local: approval socket root: %w", err)
	}
	if err := os.Chmod(rootDir, 0o700); err != nil {
		_ = os.RemoveAll(rootDir)
		return nil, fmt.Errorf("world/local: approval socket root mode: %w", err)
	}
	hostDir := rootDir
	relayDir := filepath.Join(rootDir, "relay")
	if err := os.Mkdir(relayDir, 0o700); err != nil {
		_ = os.RemoveAll(rootDir)
		return nil, fmt.Errorf("world/local: approval relay dir: %w", err)
	}
	b := &approvalBroker{
		spanID: spanID, capability: hex.EncodeToString(capabilityBytes),
		rootDir: rootDir, hostDir: hostDir, relayDir: relayDir,
		hostPath:  filepath.Join(hostDir, approvalHostSocketName),
		relayPath: filepath.Join(relayDir, approvalRelaySocketName),
		capacity:  capacity, rateLimit: defaultApprovalRate, deadline: deadline,
		intents: map[string]*approvalIntent{}, waiting: map[string][]*approvalPendingHook{},
		hooks: map[string]*approvalPendingHook{}, deliveries: make(chan *approvalPendingHook, capacity),
		conns: map[net.Conn]struct{}{},
		done:  make(chan struct{}), windowStart: time.Now(),
	}
	b.host, err = net.Listen("unix", b.hostPath)
	if err != nil {
		b.removeDirs()
		return nil, fmt.Errorf("world/local: approval host listen: %w", err)
	}
	b.relay, err = net.Listen("unix", b.relayPath)
	if err != nil {
		b.host.Close()
		b.removeDirs()
		return nil, fmt.Errorf("world/local: approval relay listen: %w", err)
	}
	for _, path := range []string{b.hostPath, b.relayPath} {
		if err := os.Chmod(path, 0o600); err != nil {
			b.host.Close()
			b.relay.Close()
			b.removeDirs()
			return nil, fmt.Errorf("world/local: approval socket mode: %w", err)
		}
	}
	b.wg.Add(2)
	go b.acceptHost()
	go b.acceptRelay()
	go func() {
		select {
		case <-time.After(time.Until(deadline)):
			b.expireAll("approval deadline 초과")
		case <-b.done:
		}
	}()
	return b, nil
}

func (b *approvalBroker) Endpoint() world.ApprovalEndpoint {
	return world.NewApprovalEndpoint("unix", b.hostPath, b.capability)
}

func (b *approvalBroker) RelayDir() string { return b.relayDir }

func (b *approvalBroker) acceptHost() {
	defer b.wg.Done()
	for {
		conn, err := b.host.Accept()
		if err != nil {
			if !b.isClosing() {
				b.fail(fmt.Errorf("world/local: approval host accept: %w", err))
			}
			return
		}
		b.trackConn(conn)
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer b.untrackConn(conn)
			b.handleHost(conn)
		}()
	}
}

func (b *approvalBroker) handleHost(conn net.Conn) {
	decoder := json.NewDecoder(io.LimitReader(conn, maxApprovalEnvelopeBytes+1))
	encoder := json.NewEncoder(conn)
	var request approvalAdapterRequest
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		b.writeHostError(encoder, "adapter request 계약 위반")
		b.fail(fmt.Errorf("world/local: approval adapter request: %w", err))
		return
	}
	if request.Capability != b.capability || request.SpanID != b.spanID {
		b.writeHostError(encoder, "lease capability/span 불일치")
		b.fail(fmt.Errorf("world/local: approval adapter capability/span 불일치"))
		return
	}
	switch request.Operation {
	case approvalwire.OperationIntent:
		if err := b.registerIntent(request); err != nil {
			b.writeHostError(encoder, err.Error())
			b.fail(err)
			return
		}
		_ = encoder.Encode(approvalAdapterResponse{OK: true})
	case approvalwire.OperationNext:
		b.deliverNext(conn, decoder, encoder)
	default:
		b.writeHostError(encoder, "허용되지 않은 adapter operation")
		b.fail(fmt.Errorf("world/local: approval adapter operation %q", request.Operation))
	}
}

func (b *approvalBroker) deliverNext(conn net.Conn, decoder *json.Decoder, encoder *json.Encoder) {
	var hook *approvalPendingHook
	select {
	case hook = <-b.deliveries:
	case <-b.done:
		b.writeHostError(encoder, "approval broker 종료")
		return
	}
	if hook == nil {
		b.writeHostError(encoder, "approval broker 종료")
		return
	}
	if err := encoder.Encode(approvalAdapterResponse{OK: true, Hook: &approvalHookDelivery{
		RequestID: hook.requestID, Raw: hook.raw, Reason: hook.reason,
	}}); err != nil {
		b.failHook(hook, "adapter delivery 실패", err)
		return
	}
	var decision approvalAdapterDecision
	if err := decoder.Decode(&decision); err != nil {
		b.failHook(hook, "adapter decision 계약 위반", err)
		return
	}
	if decision.RequestID != hook.requestID || (decision.Decision != "allow" && decision.Decision != "deny") ||
		(decision.Decision == "deny" && (decision.Reason == nil || *decision.Reason == "")) {
		b.failHook(hook, "adapter decision 계약 위반", errors.New("request_id/decision/reason"))
		return
	}
	if hook.reason != nil && decision.Decision != "deny" {
		b.failHook(hook, "강제 deny를 adapter가 allow로 변경", errors.New("forced deny violation"))
		return
	}
	if err := json.NewEncoder(hook.conn).Encode(approvalRelayDecision{Decision: decision.Decision, Reason: decision.Reason}); err != nil {
		b.failHook(hook, "hook decision 전송 실패", err)
		return
	}
	var ack approvalRelayAck
	if err := hook.decoder.Decode(&ack); err != nil || !ack.Delivered {
		if err == nil {
			err = errors.New("delivered ack가 false")
		}
		b.failHook(hook, "hook delivery ACK 실패", err)
		return
	}
	b.completeHook(hook)
	_ = encoder.Encode(approvalAdapterResponse{OK: true, Delivered: true})
}

func (b *approvalBroker) acceptRelay() {
	defer b.wg.Done()
	for {
		conn, err := b.relay.Accept()
		if err != nil {
			if !b.isClosing() {
				b.fail(fmt.Errorf("world/local: approval relay accept: %w", err))
			}
			return
		}
		b.trackConn(conn)
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer b.untrackConn(conn)
			b.handleRelay(conn)
		}()
	}
}

func (b *approvalBroker) handleRelay(conn net.Conn) {
	decoder := json.NewDecoder(io.LimitReader(conn, maxApprovalEnvelopeBytes+1))
	var request approvalRelayRequest
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || len(request.Raw) == 0 || len(request.Raw) > maxApprovalLineBytes {
		if err == nil {
			err = fmt.Errorf("raw 크기 %d", len(request.Raw))
		}
		b.fail(fmt.Errorf("world/local: approval relay request 계약 위반: %w", err))
		return
	}
	var input approvalNativeInput
	if err := json.Unmarshal(request.Raw, &input); err != nil || input.HookEventName != "PreToolUse" ||
		input.ToolUseID == "" || input.ToolName == "" {
		b.fail(fmt.Errorf("world/local: approval native hook 계약 위반"))
		return
	}
	canonical, err := canonicalObject(input.ToolInput)
	if err != nil {
		b.fail(fmt.Errorf("world/local: approval hook args: %w", err))
		return
	}
	requestID, err := newApprovalRequestID()
	if err != nil {
		b.fail(err)
		return
	}
	hook := &approvalPendingHook{
		requestID: requestID, callID: input.ToolUseID, name: input.ToolName,
		canonical: canonical, raw: append([]byte(nil), request.Raw...), conn: conn, decoder: decoder,
		done: make(chan struct{}),
	}
	if err := b.admitHook(hook); err != nil {
		b.directDeny(hook, err.Error())
		b.fail(err)
		return
	}
	select {
	case <-hook.done:
	case <-b.done:
		b.directDeny(hook, "approval broker 종료")
		<-hook.done
	}
}

func (b *approvalBroker) admitHook(hook *approvalPendingHook) error {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return fmt.Errorf("approval broker 종료 중")
	}
	now := time.Now()
	if now.Sub(b.windowStart) >= time.Second {
		b.windowStart, b.windowCount = now, 0
	}
	b.windowCount++
	if b.windowCount > b.rateLimit {
		b.mu.Unlock()
		return fmt.Errorf("approval relay 요청률 한도 초과")
	}
	if len(b.hooks) >= b.capacity {
		b.mu.Unlock()
		return fmt.Errorf("approval relay pending 한도 초과")
	}
	b.hooks[hook.requestID] = hook
	if b.expired {
		hook.reason = stringPointer("approval deadline 초과")
		b.mu.Unlock()
		return b.enqueue(hook)
	}
	intent := b.intents[hook.callID]
	if intent == nil {
		b.waiting[hook.callID] = append(b.waiting[hook.callID], hook)
		b.mu.Unlock()
		return nil
	}
	b.classifyLocked(intent, hook)
	b.mu.Unlock()
	return b.enqueue(hook)
}

func (b *approvalBroker) registerIntent(request approvalAdapterRequest) error {
	canonical, err := canonicalObject(request.Args)
	if err != nil || request.CallID == "" || request.Name == "" {
		return fmt.Errorf("world/local: approval intent 계약 위반")
	}
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return fmt.Errorf("world/local: approval broker 종료 중")
	}
	if b.expired {
		b.mu.Unlock()
		return nil // deadline 뒤 intent는 권한을 만들지 않으며 hook은 강제 deny된다.
	}
	if _, exists := b.intents[request.CallID]; exists {
		b.mu.Unlock()
		return fmt.Errorf("world/local: duplicate native tool intent %s", request.CallID)
	}
	if len(b.intents) >= b.capacity {
		b.mu.Unlock()
		return fmt.Errorf("world/local: approval intent ledger 포화")
	}
	intent := &approvalIntent{callID: request.CallID, name: request.Name, canonical: canonical}
	b.intents[request.CallID] = intent
	waiting := b.waiting[request.CallID]
	delete(b.waiting, request.CallID)
	for _, hook := range waiting {
		b.classifyLocked(intent, hook)
	}
	b.mu.Unlock()
	for _, hook := range waiting {
		if err := b.enqueue(hook); err != nil {
			return err
		}
	}
	return nil
}

func (b *approvalBroker) classifyLocked(intent *approvalIntent, hook *approvalPendingHook) {
	switch {
	case intent.name != hook.name || !bytes.Equal(intent.canonical, hook.canonical):
		hook.reason = stringPointer("tool intent mismatch")
	case intent.consumed:
		hook.reason = stringPointer("duplicate tool intent")
	default:
		intent.consumed = true
	}
}

func (b *approvalBroker) enqueue(hook *approvalPendingHook) error {
	select {
	case b.deliveries <- hook:
		return nil
	default:
		err := fmt.Errorf("world/local: approval delivery queue 포화")
		b.directDeny(hook, err.Error())
		b.fail(err)
		return err
	}
}

func (b *approvalBroker) expireAll(reason string) {
	b.mu.Lock()
	b.expired = true
	var hooks []*approvalPendingHook
	for _, list := range b.waiting {
		for _, hook := range list {
			hook.reason = stringPointer(reason)
			hooks = append(hooks, hook)
		}
	}
	b.waiting = map[string][]*approvalPendingHook{}
	b.mu.Unlock()
	for _, hook := range hooks {
		_ = b.enqueue(hook)
	}
}

func (b *approvalBroker) failHook(hook *approvalPendingHook, reason string, cause error) {
	b.directDeny(hook, reason)
	b.fail(fmt.Errorf("world/local: %s: %w", reason, cause))
}

func (b *approvalBroker) directDeny(hook *approvalPendingHook, reason string) {
	hook.once.Do(func() {
		_ = hook.conn.SetDeadline(time.Now().Add(time.Second))
		_ = json.NewEncoder(hook.conn).Encode(approvalRelayDecision{Decision: "deny", Reason: &reason})
		var ack approvalRelayAck
		_ = hook.decoder.Decode(&ack)
		b.removeHook(hook)
		close(hook.done)
	})
}

func (b *approvalBroker) completeHook(hook *approvalPendingHook) {
	hook.once.Do(func() {
		b.removeHook(hook)
		close(hook.done)
	})
}

func (b *approvalBroker) removeHook(hook *approvalPendingHook) {
	b.mu.Lock()
	delete(b.hooks, hook.requestID)
	b.removeWaitingLocked(hook)
	b.mu.Unlock()
}

func (b *approvalBroker) removeWaitingLocked(hook *approvalPendingHook) {
	list := b.waiting[hook.callID]
	for i, item := range list {
		if item == hook {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(b.waiting, hook.callID)
	} else {
		b.waiting[hook.callID] = list
	}
}

func (b *approvalBroker) fail(err error) {
	b.mu.Lock()
	if b.firstErr == nil {
		b.firstErr = err
	}
	if b.failed {
		b.mu.Unlock()
		return
	}
	b.failed, b.closing = true, true
	hooks := make([]*approvalPendingHook, 0, len(b.hooks))
	for _, hook := range b.hooks {
		hooks = append(hooks, hook)
	}
	b.mu.Unlock()
	b.closeListeners()
	b.doneOnce.Do(func() { close(b.done) })
	for _, hook := range hooks {
		b.directDeny(hook, "approval relay fatal")
	}
	b.closeConnections()
}

func (b *approvalBroker) Shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithDeadline(ctx, b.deadline)
	defer cancel()
	b.mu.Lock()
	if !b.closing {
		b.closing = true
	}
	var waiting []*approvalPendingHook
	for _, list := range b.waiting {
		for _, hook := range list {
			hook.reason = stringPointer("lease 종료")
			waiting = append(waiting, hook)
		}
	}
	b.waiting = map[string][]*approvalPendingHook{}
	b.mu.Unlock()
	if b.relay != nil {
		_ = b.relay.Close()
	}
	for _, hook := range waiting {
		_ = b.enqueue(hook)
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		b.mu.Lock()
		remaining := len(b.hooks)
		b.mu.Unlock()
		if remaining == 0 {
			break
		}
		select {
		case <-shutdownCtx.Done():
			b.fail(fmt.Errorf("world/local: approval durable deny drain: %w", shutdownCtx.Err()))
			return errors.Join(shutdownCtx.Err(), b.Err())
		case <-ticker.C:
		}
	}
	b.doneOnce.Do(func() { close(b.done) })
	if b.host != nil {
		_ = b.host.Close()
	}
	b.closeConnections()
	b.wg.Wait()
	return b.Err()
}

func (b *approvalBroker) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.firstErr
}

func (b *approvalBroker) Cleanup() error {
	b.mu.Lock()
	b.closing = true
	b.mu.Unlock()
	b.closeListeners()
	b.doneOnce.Do(func() { close(b.done) })
	b.closeConnections()
	b.wg.Wait()
	return b.removeDirs()
}

func (b *approvalBroker) closeListeners() {
	if b.host != nil {
		_ = b.host.Close()
	}
	if b.relay != nil {
		_ = b.relay.Close()
	}
}

func (b *approvalBroker) trackConn(conn net.Conn) {
	b.mu.Lock()
	b.conns[conn] = struct{}{}
	b.mu.Unlock()
}

func (b *approvalBroker) untrackConn(conn net.Conn) {
	b.mu.Lock()
	delete(b.conns, conn)
	b.mu.Unlock()
	_ = conn.Close()
}

func (b *approvalBroker) closeConnections() {
	b.mu.Lock()
	connections := make([]net.Conn, 0, len(b.conns))
	for conn := range b.conns {
		connections = append(connections, conn)
	}
	b.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (b *approvalBroker) removeDirs() error {
	return os.RemoveAll(b.rootDir)
}

func (b *approvalBroker) isClosing() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closing
}

func (b *approvalBroker) writeHostError(encoder *json.Encoder, message string) {
	_ = encoder.Encode(approvalAdapterResponse{Error: message})
}

func canonicalObject(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// Preserve number lexemes instead of rounding through float64. Equivalent
	// object key order/spacing canonicalizes; alternate numeric spellings deny
	// conservatively rather than allowing a precision-collision match.
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("JSON object가 아님")
	}
	return json.Marshal(value)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("후행 JSON 값")
		}
		return err
	}
	return nil
}

func newApprovalRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("world/local: approval request_id 발급: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], value[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], value[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], value[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], value[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], value[10:16])
	return string(out[:]), nil
}

func stringPointer(value string) *string { return &value }
