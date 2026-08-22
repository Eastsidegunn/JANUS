package worldadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/approvalrelaywire"
	"github.com/Eastsidegunn/JANUS/core/world/approvalwire"
)

type approvalClient struct {
	ctx      context.Context
	cancel   context.CancelFunc
	endpoint world.ApprovalEndpoint
	spanID   string
	emit     func(gen.EventKind, json.RawMessage, []byte) error

	mu      sync.Mutex
	pending map[string]chan gen.ApprovalResponsePayload
	conns   map[net.Conn]struct{}
	err     error
	wg      sync.WaitGroup
}

func newApprovalClient(parent context.Context, endpoint world.ApprovalEndpoint, spanID string, emit func(gen.EventKind, json.RawMessage, []byte) error) (*approvalClient, error) {
	if endpoint.Network() != "unix" || endpoint.Address() == "" || endpoint.Capability() == "" || spanID == "" {
		return nil, fmt.Errorf("worldadapter: approval endpoint 계약 위반")
	}
	ctx, cancel := context.WithCancel(parent)
	c := &approvalClient{
		ctx: ctx, cancel: cancel, endpoint: endpoint, spanID: spanID, emit: emit,
		pending: map[string]chan gen.ApprovalResponsePayload{}, conns: map[net.Conn]struct{}{},
	}
	c.wg.Add(1)
	go c.poll()
	return c, nil
}

func (c *approvalClient) RegisterIntent(intent gen.AgentToolCallPayload) error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer c.release(conn)
	if err := json.NewEncoder(conn).Encode(approvalwire.Request{
		Operation: approvalwire.OperationIntent, Capability: c.endpoint.Capability(), SpanID: c.spanID,
		CallID: intent.CallID, Name: intent.Name, Args: intent.Args,
	}); err != nil {
		return err
	}
	var response approvalwire.Response
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("worldadapter: approval intent: %s", response.Error)
	}
	return nil
}

func (c *approvalClient) Resolve(response gen.ApprovalResponsePayload) error {
	c.mu.Lock()
	waiter := c.pending[response.RequestID]
	if waiter != nil {
		delete(c.pending, response.RequestID)
	}
	c.mu.Unlock()
	if waiter == nil {
		return fmt.Errorf("worldadapter: 미상관 또는 중복 approval_response %s", response.RequestID)
	}
	select {
	case waiter <- response:
		return nil
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

func (c *approvalClient) poll() {
	defer c.wg.Done()
	for c.ctx.Err() == nil {
		if err := c.pollOne(); err != nil {
			if c.ctx.Err() == nil {
				c.fail(err)
			}
			return
		}
	}
}

func (c *approvalClient) pollOne() error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer c.release(conn)
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	if err := enc.Encode(approvalwire.Request{
		Operation: approvalwire.OperationNext, Capability: c.endpoint.Capability(), SpanID: c.spanID,
	}); err != nil {
		return err
	}
	var brokerResponse approvalwire.Response
	if err := dec.Decode(&brokerResponse); err != nil {
		return err
	}
	if !brokerResponse.OK || brokerResponse.Hook == nil {
		return fmt.Errorf("worldadapter: approval next: %s", brokerResponse.Error)
	}
	hook := brokerResponse.Hook
	var native approvalrelaywire.NativeInput
	if err := json.Unmarshal(hook.Raw, &native); err != nil {
		return fmt.Errorf("worldadapter: approval raw decode: %w", err)
	}
	payload, err := json.Marshal(gen.ApprovalRequestPayload{
		RequestID: hook.RequestID, CallID: native.ToolUseID, Name: native.ToolName,
		Args: native.ToolInput, Reason: hook.Reason,
	})
	if err != nil {
		return err
	}
	waiter := make(chan gen.ApprovalResponsePayload, 1)
	c.mu.Lock()
	if _, exists := c.pending[hook.RequestID]; exists {
		c.mu.Unlock()
		return fmt.Errorf("worldadapter: 중복 approval request_id %s", hook.RequestID)
	}
	c.pending[hook.RequestID] = waiter
	c.mu.Unlock()
	if err := c.emit(gen.EventKindSubagentApprovalRequest, payload, hook.Raw); err != nil {
		return err
	}
	var decision gen.ApprovalResponsePayload
	select {
	case decision = <-waiter:
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
	if err := enc.Encode(approvalwire.Decision{
		RequestID: hook.RequestID, Decision: string(decision.Decision), Reason: decision.Reason,
	}); err != nil {
		return err
	}
	brokerResponse = approvalwire.Response{}
	if err := dec.Decode(&brokerResponse); err != nil {
		return err
	}
	if !brokerResponse.OK || !brokerResponse.Delivered {
		return fmt.Errorf("worldadapter: approval delivery: %s", brokerResponse.Error)
	}
	return nil
}

func (c *approvalClient) dial() (net.Conn, error) {
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

func (c *approvalClient) release(conn net.Conn) {
	c.mu.Lock()
	delete(c.conns, conn)
	c.mu.Unlock()
	_ = conn.Close()
}

func (c *approvalClient) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.cancel()
	c.closeConnections()
}

func (c *approvalClient) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *approvalClient) Close() {
	c.cancel()
	c.closeConnections()
	c.wg.Wait()
}

func (c *approvalClient) closeConnections() {
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
