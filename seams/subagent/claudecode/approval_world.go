package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/brokerwire"
)

const worldApprovalWorkers = 4

type worldApprovalRequest = brokerwire.Request
type worldApprovalResponse = brokerwire.Response
type worldApprovalHook = brokerwire.Hook
type worldApprovalDecision = brokerwire.Decision

type worldApprovalClient struct {
	state    *approvalServer
	endpoint world.Endpoint
	spanID   string
	ctx      context.Context
	cancel   context.CancelFunc

	mu       sync.Mutex
	firstErr error
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

func newApprovalTransport(w *wireWriter, cfg Config) (approvalTransport, error) {
	endpoint := cfg.WorldEndpoint
	if endpoint == (world.Endpoint{}) {
		return newApprovalServer(w)
	}
	if endpoint.Network() != "unix" || endpoint.Address() == "" || endpoint.Capability() == "" || cfg.WorldSpanID == "" {
		return nil, fmt.Errorf("claudecode: world approval endpoint 계약 위반")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &worldApprovalClient{
		state: newApprovalState(w), endpoint: endpoint, spanID: cfg.WorldSpanID,
		ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{},
	}
	for range worldApprovalWorkers {
		c.wg.Add(1)
		go c.poll()
	}
	return c, nil
}

func (c *worldApprovalClient) attach(done <-chan struct{}, kill func()) {
	c.state.attach(done, kill)
	// Process exit can precede EOF drain while native tool_call lines remain in
	// the kernel buffer. state uses Done to deny hooks, but broker connections
	// stay alive until Run finishes draining and calls Close, so those observed
	// intents are not lost.
}

func (c *worldApprovalClient) markReady() { c.state.markReady() }

// The container receives HX_APPROVAL_SOCKET from world/local. The host
// adapter must not overwrite it with the real T9 socket or expose that socket
// across the sandbox boundary.
func (c *worldApprovalClient) environment(base []string) []string {
	out := append([]string(nil), base...)
	for _, key := range []string{
		worldBrokerNetworkEnv, worldBrokerAddressEnv, worldBrokerCapabilityEnv,
		worldBrokerSpanEnv, approvalSocketEnv,
	} {
		out = removeEnv(out, key)
	}
	return out
}

func (c *worldApprovalClient) registerIntent(intent gen.AgentToolCallPayload) error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer c.closeConn(conn)
	if err := json.NewEncoder(conn).Encode(worldApprovalRequest{
		Operation: brokerwire.OperationIntent, Capability: c.endpoint.Capability(), SpanID: c.spanID,
		CallID: intent.CallID, Name: intent.Name, Args: intent.Args,
	}); err != nil {
		return err
	}
	var response worldApprovalResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("world approval intent: %s", response.Error)
	}
	return nil
}

func (c *worldApprovalClient) resolve(response gen.ApprovalResponsePayload) error {
	return c.state.resolve(response)
}

func (c *worldApprovalClient) denyAll(reason string, wait bool) int {
	return c.state.denyAll(reason, wait)
}

func (c *worldApprovalClient) failure() error {
	c.mu.Lock()
	err := c.firstErr
	c.mu.Unlock()
	return errors.Join(c.state.failure(), err)
}

func (c *worldApprovalClient) Close() {
	c.state.denyAll("world approval client 종료", false)
	c.state.doneOnce.Do(func() { close(c.state.done) })
	c.cancel()
	c.closeConnections()
	c.wg.Wait()
	c.state.Close()
}

func (c *worldApprovalClient) poll() {
	defer c.wg.Done()
	for {
		if err := c.pollOne(); err != nil {
			if c.ctx.Err() == nil {
				c.report(err)
			}
			return
		}
	}
}

func (c *worldApprovalClient) pollOne() error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer c.closeConn(conn)
	encoder, decoder := json.NewEncoder(conn), json.NewDecoder(conn)
	if err := encoder.Encode(worldApprovalRequest{
		Operation: brokerwire.OperationNext, Capability: c.endpoint.Capability(), SpanID: c.spanID,
	}); err != nil {
		return err
	}
	var response worldApprovalResponse
	if err := decoder.Decode(&response); err != nil {
		return err
	}
	if !response.OK || response.Hook == nil {
		return fmt.Errorf("world approval next: %s", response.Error)
	}
	hook := response.Hook
	requestID, pending, err := c.state.begin(hook.Raw, hook.RequestID, hook.Reason)
	if err != nil {
		reason := "approval_request durable 기록 실패"
		_ = encoder.Encode(worldApprovalDecision{
			RequestID: hook.RequestID, Decision: "deny", Reason: &reason,
		})
		return err
	}
	decision := c.state.awaitDecision(pending)
	if err := encoder.Encode(worldApprovalDecision{
		RequestID: requestID, Decision: decision.Decision, Reason: decision.Reason,
	}); err != nil {
		return err
	}
	response = worldApprovalResponse{}
	if err := decoder.Decode(&response); err != nil {
		return err
	}
	if !response.OK || !response.Delivered {
		return fmt.Errorf("world approval delivery: %s", response.Error)
	}
	close(pending.delivered)
	c.state.complete(requestID)
	return nil
}

func (c *worldApprovalClient) dial() (net.Conn, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(c.ctx, c.endpoint.Network(), c.endpoint.Address())
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.conns[conn] = struct{}{}
	c.mu.Unlock()
	return conn, nil
}

func (c *worldApprovalClient) closeConn(conn net.Conn) {
	c.mu.Lock()
	delete(c.conns, conn)
	c.mu.Unlock()
	_ = conn.Close()
}

func (c *worldApprovalClient) closeConnections() {
	c.mu.Lock()
	connections := make([]net.Conn, 0, len(c.conns))
	for conn := range c.conns {
		connections = append(connections, conn)
	}
	c.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (c *worldApprovalClient) report(err error) {
	c.mu.Lock()
	if c.firstErr == nil {
		c.firstErr = fmt.Errorf("claudecode: world approval broker: %w", err)
	}
	c.mu.Unlock()
	c.state.report(err)
	c.cancel()
	c.closeConnections()
}
