package claudecode

import (
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

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

const approvalSocketEnv = "HX_APPROVAL_SOCKET"

type hookSocketRequest struct {
	Raw []byte `json:"raw"`
}

type hookSocketDecision struct {
	Decision string  `json:"decision"`
	Reason   *string `json:"reason,omitempty"`
}

type hookSocketAck struct {
	Delivered bool `json:"delivered"`
}

type nativeHookInput struct {
	HookEventName string          `json:"hook_event_name"`
	ToolUseID     string          `json:"tool_use_id"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

type pendingApproval struct {
	decision  chan hookSocketDecision
	delivered chan struct{}
}

type approvalServer struct {
	dir      string
	path     string
	listener net.Listener
	writer   *wireWriter

	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{}
	doneOnce  sync.Once
	kill      func()

	mu        sync.Mutex
	pending   map[string]*pendingApproval
	completed map[string]bool
	closed    bool
	firstErr  error
	conns     map[net.Conn]struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func newApprovalServer(w *wireWriter) (*approvalServer, error) {
	dir, err := os.MkdirTemp("/tmp", "hxapprove-")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "approval.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	s := &approvalServer{
		dir: dir, path: path, listener: listener, writer: w,
		ready:   make(chan struct{}),
		done:    make(chan struct{}),
		pending: map[string]*pendingApproval{}, completed: map[string]bool{},
		conns: map[net.Conn]struct{}{},
	}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

func (s *approvalServer) attach(done <-chan struct{}, kill func()) {
	s.mu.Lock()
	s.kill = kill
	s.mu.Unlock()
	go func() {
		<-done
		s.doneOnce.Do(func() { close(s.done) })
		s.denyAll("Claude 프로세스 종료", false)
		s.closeNetwork()
	}()
}

func (s *approvalServer) markReady() { s.readyOnce.Do(func() { close(s.ready) }) }

func (s *approvalServer) environment(base []string) []string {
	out := append([]string(nil), base...)
	out = append(out, approvalSocketEnv+"="+s.path)
	return out
}

func (s *approvalServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				s.report(fmt.Errorf("claudecode: approval socket accept: %w", err))
			}
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handle(conn)
	}
}

func (s *approvalServer) handle(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		conn.Close()
	}()
	dec := json.NewDecoder(io.LimitReader(conn, 8<<20))
	var request hookSocketRequest
	if err := dec.Decode(&request); err != nil {
		s.report(fmt.Errorf("claudecode: hxapprove 요청: %w", err))
		return
	}
	if len(request.Raw) == 0 || len(request.Raw) > MaxLineBytes {
		s.report(fmt.Errorf("claudecode: hxapprove raw 크기 위반: %d", len(request.Raw)))
		return
	}
	var input nativeHookInput
	if err := json.Unmarshal(request.Raw, &input); err != nil {
		s.report(fmt.Errorf("claudecode: hxapprove raw JSON: %w", err))
		return
	}
	if input.HookEventName != "PreToolUse" || input.ToolUseID == "" || input.ToolName == "" || !isJSONObject(input.ToolInput) {
		s.report(fmt.Errorf("claudecode: hxapprove PreToolUse 입력 계약 위반"))
		return
	}

	select {
	case <-s.ready:
	case <-s.done:
		s.writeDecision(conn, hookSocketDecision{Decision: "deny", Reason: strPtr("ready 전 Claude 종료")})
		return
	}
	requestID, err := newRequestID()
	if err != nil {
		s.report(err)
		return
	}
	pending := &pendingApproval{decision: make(chan hookSocketDecision, 1), delivered: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.pending[requestID] = pending
	s.mu.Unlock()

	payload, err := json.Marshal(gen.ApprovalRequestPayload{
		RequestID: requestID, CallID: input.ToolUseID, Name: input.ToolName, Args: input.ToolInput,
	})
	if err != nil {
		s.report(err)
		return
	}
	// Unlike synthetic lifecycle events, this event has an upstream source:
	// raw is the exact hook stdin byte sequence received from hxapprove (C-3).
	if err := s.writer.emit(gen.EventKindSubagentApprovalRequest, payload, request.Raw); err != nil {
		s.report(fmt.Errorf("claudecode: approval_request 방출: %w", err))
		return
	}

	var decision hookSocketDecision
	select {
	case decision = <-pending.decision:
	case <-s.done:
		decision = hookSocketDecision{Decision: "deny", Reason: strPtr("Claude 프로세스 종료")}
	}
	if err := s.writeDecision(conn, decision); err != nil {
		s.report(fmt.Errorf("claudecode: hxapprove 판정 전달: %w", err))
		return
	}
	var ack hookSocketAck
	if err := dec.Decode(&ack); err != nil || !ack.Delivered {
		if err == nil {
			err = errors.New("delivered ack가 false")
		}
		s.report(fmt.Errorf("claudecode: hxapprove 전달 확인: %w", err))
		return
	}
	close(pending.delivered)
}

func (s *approvalServer) writeDecision(conn net.Conn, decision hookSocketDecision) error {
	return json.NewEncoder(conn).Encode(decision)
}

func (s *approvalServer) resolve(response gen.ApprovalResponsePayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.pending[response.RequestID]
	if !ok {
		if s.completed[response.RequestID] {
			return fmt.Errorf("claudecode: %w request_id=%s", errDuplicateApproval, response.RequestID)
		}
		return fmt.Errorf("claudecode: %w request_id=%s", errUnmatchedApproval, response.RequestID)
	}
	delete(s.pending, response.RequestID)
	s.completed[response.RequestID] = true
	pending.decision <- hookSocketDecision{Decision: string(response.Decision), Reason: response.Reason}
	return nil
}

// denyAll sends every denial first. When waitDelivery is true (stop/error),
// it waits only for hxapprove ACKs or native Done; no second timeout exists.
func (s *approvalServer) denyAll(reason string, waitDelivery bool) int {
	s.mu.Lock()
	items := make([]*pendingApproval, 0, len(s.pending))
	for id, pending := range s.pending {
		delete(s.pending, id)
		s.completed[id] = true
		items = append(items, pending)
	}
	s.mu.Unlock()
	decision := hookSocketDecision{Decision: "deny", Reason: &reason}
	for _, pending := range items {
		pending.decision <- decision
	}
	if waitDelivery {
		for _, pending := range items {
			select {
			case <-pending.delivered:
			case <-s.done:
				return len(items)
			}
		}
	}
	return len(items)
}

func (s *approvalServer) report(err error) {
	select {
	case <-s.done:
		return // process-driven socket teardown is not a new handshake failure
	default:
	}
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return
	default:
	}
	if s.firstErr == nil {
		s.firstErr = err
	}
	kill := s.kill
	s.mu.Unlock()
	if kill != nil {
		kill()
	}
}

func (s *approvalServer) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr
}

func (s *approvalServer) closeNetwork() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.listener.Close()
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
}

func (s *approvalServer) Close() {
	s.closeOnce.Do(func() {
		s.closeNetwork()
		s.wg.Wait()
		os.RemoveAll(s.dir)
	})
}

func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("claudecode: request_id 발급: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:]), nil
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := []byte(raw)
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t' || trimmed[0] == '\r' || trimmed[0] == '\n') {
		trimmed = trimmed[1:]
	}
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(raw)
}
