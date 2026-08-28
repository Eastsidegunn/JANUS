package local

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/world/local/egressproxy"
)

const (
	auditSocketName = "audit.sock"
	auditMaxLine    = 64 * 1024
	auditIOTimeout  = 30 * time.Second
)

type effectBroker interface {
	SocketDir() string
	Ready(context.Context) error
	Effects() <-chan world.EffectAttempt
	Shutdown(context.Context) error
	Done() <-chan struct{}
	Err() error
}

type auditBrokerFactory func(string, string, int) (effectBroker, error)

type unixAuditBroker struct {
	spanID   string
	dir      string
	listener net.Listener
	queue    chan world.EffectAttempt
	effects  chan world.EffectAttempt
	ready    chan struct{}
	done     chan struct{}

	readyOnce    sync.Once
	shutdownOnce sync.Once
	sequence     atomic.Uint64

	mu           sync.Mutex
	closing      bool
	connections  map[net.Conn]struct{}
	handlerSlots chan struct{}
	err          error
	deferAck     bool
	handlers     sync.WaitGroup
}

func startAuditBroker(_ string, spanID string, capacity int) (effectBroker, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("world/local: audit queue capacity는 양수여야 함")
	}
	// Unix domain socket paths are much shorter than normal filesystem paths
	// (108 bytes on Linux). Keep this host-only capability in a short 0700
	// root, as the approval and process brokers already do; stateDir can be a
	// long application path and is not itself a security boundary for the
	// sidecar socket.
	dir, err := os.MkdirTemp("/tmp", "hxe-")
	if err != nil {
		return nil, fmt.Errorf("world/local: audit socket root: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("world/local: audit socket root mode: %w", err)
	}
	path := filepath.Join(dir, auditSocketName)
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("world/local: audit socket listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("world/local: audit socket mode: %w", err)
	}
	broker := &unixAuditBroker{
		spanID: spanID, dir: dir, listener: listener,
		queue: make(chan world.EffectAttempt, capacity), effects: make(chan world.EffectAttempt),
		ready: make(chan struct{}), done: make(chan struct{}), connections: map[net.Conn]struct{}{},
		handlerSlots: make(chan struct{}, capacity+1),
	}
	go broker.dispatch()
	go broker.accept()
	return broker, nil
}

func (b *unixAuditBroker) SocketDir() string                   { return b.dir }
func (b *unixAuditBroker) Effects() <-chan world.EffectAttempt { return b.effects }
func (b *unixAuditBroker) Done() <-chan struct{}               { return b.done }
func (b *unixAuditBroker) Err() error                          { b.mu.Lock(); defer b.mu.Unlock(); return b.err }
func (b *unixAuditBroker) EnableDurableAck()                   { b.deferAck = true }
func (b *unixAuditBroker) setErr(err error) {
	b.mu.Lock()
	b.err = errors.Join(b.err, err)
	b.mu.Unlock()
}
func (b *unixAuditBroker) markReady() { b.readyOnce.Do(func() { close(b.ready) }) }
func (b *unixAuditBroker) nextID() string {
	return fmt.Sprintf("%s-egress-%d", b.spanID, b.sequence.Add(1))
}
func (b *unixAuditBroker) Ready(ctx context.Context) error {
	select {
	case <-b.ready:
		return nil
	case <-b.done:
		return errors.Join(errors.New("world/local: proxy가 ready 전에 종료"), b.Err())
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *unixAuditBroker) accept() {
	defer func() {
		b.handlers.Wait()
		close(b.queue)
	}()
	for {
		connection, err := b.listener.Accept()
		if err != nil {
			b.mu.Lock()
			closing := b.closing
			b.mu.Unlock()
			if !closing {
				b.setErr(fmt.Errorf("world/local: audit accept: %w", err))
			}
			return
		}
		b.mu.Lock()
		if b.closing {
			b.mu.Unlock()
			connection.Close()
			continue
		}
		select {
		case b.handlerSlots <- struct{}{}:
			b.connections[connection] = struct{}{}
			b.handlers.Add(1)
			b.mu.Unlock()
			go b.handle(connection)
		default:
			b.mu.Unlock()
			// Closing without an ACK is fail-closed at UnixAuditSink and bounds
			// host goroutines even if the sidecar is flooded by its agent.
			connection.Close()
		}
	}
}

func (b *unixAuditBroker) handle(connection net.Conn) {
	defer func() {
		connection.Close()
		b.mu.Lock()
		delete(b.connections, connection)
		b.mu.Unlock()
		<-b.handlerSlots
		b.handlers.Done()
	}()
	_ = connection.SetDeadline(time.Now().Add(auditIOTimeout))
	reader := bufio.NewReader(io.LimitReader(connection, auditMaxLine+1))
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) > auditMaxLine {
		b.reply(connection, false, "audit line이 없거나 64KiB 상한 초과")
		return
	}
	var envelope struct {
		Type    string               `json:"type"`
		Attempt *egressproxy.Attempt `json:"attempt,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		b.reply(connection, false, "audit JSON 형식 오류")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		b.reply(connection, false, "audit JSON 뒤 추가 값")
		return
	}
	switch envelope.Type {
	case "ready":
		if envelope.Attempt != nil {
			b.reply(connection, false, "ready에 attempt가 포함됨")
			return
		}
		b.markReady()
		b.reply(connection, true, "")
	case "attempt":
		if envelope.Attempt == nil {
			b.reply(connection, false, "attempt가 없음")
			return
		}
		if err := validateAuditAttempt(*envelope.Attempt); err != nil {
			b.reply(connection, false, err.Error())
			return
		}
		attempt := world.EffectAttempt{
			ID: b.nextID(), SpanID: b.spanID, Kind: "egress", Target: envelope.Attempt.Domain,
			Method: envelope.Attempt.Method, RequestBytes: envelope.Attempt.RequestBytes,
			AtUnixMs: envelope.Attempt.AtUnixMs, Decision: world.EffectDecision(envelope.Attempt.Decision),
			Reason: envelope.Attempt.Reason,
		}
		ack := make(chan error, 1)
		if b.deferAck {
			attempt.Ack = func(err error) {
				select {
				case ack <- err:
				default:
				}
			}
		}
		select {
		case b.queue <- attempt:
			if !b.deferAck {
				b.reply(connection, true, "")
				return
			}
			// The proxy ACK is deliberately held until the surface has durably
			// committed the corresponding collector event. This prevents a
			// successful network exchange from outrunning the append-only log.
			select {
			case ackErr := <-ack:
				if ackErr != nil {
					b.reply(connection, false, ackErr.Error())
				} else {
					b.reply(connection, true, "")
				}
			case <-time.After(auditIOTimeout):
				b.reply(connection, false, "audit durable ACK timeout")
			}
		default:
			b.reply(connection, false, "audit queue 포화")
		}
	default:
		b.reply(connection, false, "미지 audit message")
	}
}

func validateAuditAttempt(attempt egressproxy.Attempt) error {
	if attempt.Decision != egressproxy.DecisionAllow && attempt.Decision != egressproxy.DecisionDeny {
		return errors.New("audit decision 위반")
	}
	if attempt.Method == "" || len(attempt.Method) > 32 || strings.ContainsAny(attempt.Method, "\x00\r\n") {
		return errors.New("audit method 위반")
	}
	if attempt.RequestBytes < 0 || attempt.AtUnixMs <= 0 {
		return errors.New("audit 숫자 필드 위반")
	}
	if len(attempt.Domain) > 253 || strings.ContainsAny(attempt.Domain, "\x00\r\n") {
		return errors.New("audit domain 위반")
	}
	if attempt.Decision == egressproxy.DecisionAllow {
		if attempt.Domain == "" || attempt.Reason != "" {
			return errors.New("allow audit 형태 위반")
		}
	}
	if attempt.Decision == egressproxy.DecisionDeny && attempt.Reason == "" {
		return errors.New("deny audit reason 누락")
	}
	return nil
}

func (b *unixAuditBroker) reply(connection net.Conn, ok bool, message string) {
	_ = json.NewEncoder(connection).Encode(struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}{OK: ok, Error: message})
}

func (b *unixAuditBroker) dispatch() {
	defer close(b.done)
	defer close(b.effects)
	for attempt := range b.queue {
		b.effects <- attempt
	}
}

func (b *unixAuditBroker) Shutdown(ctx context.Context) error {
	b.shutdownOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		b.mu.Unlock()
		if err := b.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			b.setErr(fmt.Errorf("world/local: audit listener close: %w", err))
		}
		// Do not close accepted connections here. The proxy process has already
		// stopped, and handlers must consume any complete record left in the
		// kernel buffer before queue closure. Each connection has its own finite
		// deadline, while the caller context bounds how long Shutdown waits.
	})
	select {
	case <-b.done:
		if err := os.RemoveAll(b.dir); err != nil {
			return fmt.Errorf("world/local: audit socket cleanup: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
