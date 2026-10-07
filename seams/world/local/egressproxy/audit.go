package egressproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

const auditDeadline = 30 * time.Second

type auditEnvelope struct {
	Type    string   `json:"type"`
	Attempt *Attempt `json:"attempt,omitempty"`
}

type auditReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// UnixAuditSink is the proxy-side capability. Only the sidecar receives the
// socket mount; the agent container never receives its path or endpoint.
type UnixAuditSink struct {
	Path string
}

func (s UnixAuditSink) Submit(ctx context.Context, attempt Attempt) error {
	return s.exchange(ctx, auditEnvelope{Type: "attempt", Attempt: &attempt})
}

// Ready announces that the helper has bound its TCP listener. The world does
// not start the agent until this host acknowledgment succeeds.
func (s UnixAuditSink) Ready(ctx context.Context) error {
	return s.exchange(ctx, auditEnvelope{Type: "ready"})
}

func (s UnixAuditSink) exchange(ctx context.Context, envelope auditEnvelope) error {
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", s.Path)
	if err != nil {
		return fmt.Errorf("audit socket 연결: %w", err)
	}
	defer connection.Close()
	deadline := time.Now().Add(auditDeadline)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return fmt.Errorf("audit socket deadline: %w", err)
	}
	if err := json.NewEncoder(connection).Encode(envelope); err != nil {
		return fmt.Errorf("audit 전송: %w", err)
	}
	var reply auditReply
	decoder := json.NewDecoder(bufio.NewReader(connection))
	if err := decoder.Decode(&reply); err != nil {
		return fmt.Errorf("audit ACK: %w", err)
	}
	if !reply.OK {
		if reply.Error == "" {
			reply.Error = "host broker가 audit를 거부함"
		}
		return errors.New(reply.Error)
	}
	return nil
}
