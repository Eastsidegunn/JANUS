package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/processwire"
)

const (
	processSocketName       = "process.sock"
	processHandshakeTimeout = 2 * time.Second
	processStopSeconds      = "10"
	processAttachExitGrace  = 2 * time.Second
	maxWaitOutput           = 4096
)

var ErrProcessBrokerFatal = errors.New("world/local: process broker fatal")

type processBroker struct {
	ctx                          context.Context
	cancel                       context.CancelFunc
	spanID, leaseID, containerID string
	runner                       commandRunner
	starter                      commandStarter
	rootDir, socketPath          string
	listener                     net.Listener
	endpoint                     world.ProcessEndpoint

	mu                                  sync.Mutex
	lifecycleMu                         sync.Mutex
	control                             net.Conn
	controlEncoder                      *processwire.Encoder
	output                              net.Conn
	outputEncoder                       *processwire.Encoder
	controlUsed, outputUsed             bool
	started, stdinClosed, waitRequested bool
	waitAcked                           bool
	controlAckPending                   bool
	stopReason                          string
	waitResult                          *processwire.ExitObserved
	exitSent, streamEnded               bool
	attach                              startedCommand
	waiter                              startedCommand
	firstErr                            error
	closing                             bool

	outputReady   chan struct{}
	bothReady     chan struct{}
	peerTimerOnce sync.Once
	bothReadyOnce sync.Once
	containerDone chan struct{}
	streamDone    chan struct{}
	done          chan struct{}
	doneOnce      sync.Once
	wg            sync.WaitGroup
}

func startProcessBroker(parent context.Context, spanID, leaseID, containerID string, runner commandRunner) (*processBroker, error) {
	starter, ok := runner.(commandStarter)
	if !ok {
		return nil, fmt.Errorf("world/local: Podman runner가 streaming process를 지원하지 않음")
	}
	controlCap, err := randomCapability()
	if err != nil {
		return nil, err
	}
	outputCap, err := randomCapability()
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("/tmp", "hxp-")
	if err != nil {
		return nil, fmt.Errorf("world/local: process socket root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	path := filepath.Join(root, processSocketName)
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("world/local: process listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(root)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	b := &processBroker{
		ctx: ctx, cancel: cancel, spanID: spanID, leaseID: leaseID, containerID: containerID,
		runner: runner, starter: starter, rootDir: root, socketPath: path, listener: listener,
		endpoint:    world.NewProcessEndpoint("unix", path, leaseID, controlCap, outputCap),
		outputReady: make(chan struct{}), bothReady: make(chan struct{}), containerDone: make(chan struct{}), streamDone: make(chan struct{}), done: make(chan struct{}),
	}
	b.wg.Add(2)
	go b.accept()
	go b.watchParent(parent)
	return b, nil
}

func randomCapability() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("world/local: process capability: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func (b *processBroker) Endpoint() world.ProcessEndpoint { return b.endpoint }
func (b *processBroker) Done() <-chan struct{}           { return b.done }
func (b *processBroker) Err() error                      { b.mu.Lock(); defer b.mu.Unlock(); return b.firstErr }

func (b *processBroker) watchParent(parent context.Context) {
	defer b.wg.Done()
	select {
	case <-parent.Done():
		b.fail(fmt.Errorf("parent context: %w", parent.Err()))
	case <-b.ctx.Done():
	case <-b.done:
	}
}

func (b *processBroker) accept() {
	defer b.wg.Done()
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			b.mu.Lock()
			closing := b.closing
			b.mu.Unlock()
			if !closing {
				b.fail(fmt.Errorf("accept: %w", err))
			}
			return
		}
		b.wg.Add(1)
		go func() { defer b.wg.Done(); b.handshake(conn) }()
	}
}

func (b *processBroker) handshake(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(processHandshakeTimeout))
	decoder := processwire.NewDecoder(conn)
	frame, err := decoder.Read()
	if err != nil {
		_ = conn.Close()
		b.fail(fmt.Errorf("handshake: %w", err))
		return
	}
	if frame.Kind != processwire.KindHello || frame.Stream != processwire.StreamControl {
		_ = conn.Close()
		b.fail(fmt.Errorf("handshake: %w: hello 아님", processwire.ErrProtocol))
		return
	}
	var hello processwire.Hello
	if err := processwire.Unmarshal(frame.Payload, &hello); err != nil {
		_ = conn.Close()
		b.fail(err)
		return
	}
	if hello.Version != processwire.Version || hello.LeaseID != b.leaseID || hello.SpanID != b.spanID {
		_ = conn.Close()
		b.fail(fmt.Errorf("handshake: %w: lease/span/version 불일치", processwire.ErrProtocol))
		return
	}
	encoder := processwire.NewEncoder(conn)
	b.mu.Lock()
	valid := false
	switch hello.Role {
	case processwire.RoleControl:
		valid = hello.Capability == b.endpoint.ControlCapability() && !b.controlUsed
		if valid {
			b.controlUsed = true
			b.control = conn
			b.controlEncoder = encoder
		}
	case processwire.RoleOutput:
		valid = hello.Capability == b.endpoint.OutputCapability() && !b.outputUsed
		if valid {
			b.outputUsed = true
			b.output = conn
			b.outputEncoder = encoder
			close(b.outputReady)
		}
	}
	b.mu.Unlock()
	if !valid {
		_ = conn.Close()
		b.fail(fmt.Errorf("handshake: %w: role/capability 중복 또는 불일치", processwire.ErrProtocol))
		return
	}
	b.armPeerDeadline()
	_ = conn.SetReadDeadline(time.Time{})
	payload, _ := processwire.Marshal(processwire.Ack{RequestSeq: frame.Seq})
	if _, err := encoder.Write(processwire.KindHelloAck, processwire.StreamControl, 0, payload); err != nil {
		b.fail(err)
		return
	}
	if hello.Role == processwire.RoleControl {
		b.handleControl(conn, decoder, encoder)
	} else {
		b.wg.Add(1)
		go b.watchOutputPeer(conn)
	}
}

func (b *processBroker) watchOutputPeer(conn net.Conn) {
	defer b.wg.Done()
	var one [1]byte
	_, err := conn.Read(one[:])
	b.mu.Lock()
	closing := b.closing
	b.mu.Unlock()
	if closing || (errors.Is(err, io.EOF) && b.sessionComplete()) {
		return
	}
	if err == nil {
		b.fail(fmt.Errorf("output peer가 금지된 역방향 byte를 전송함"))
		return
	}
	b.fail(fmt.Errorf("output peer disconnect: %w", err))
}

func (b *processBroker) armPeerDeadline() {
	b.mu.Lock()
	both := b.controlUsed && b.outputUsed
	b.mu.Unlock()
	if both {
		b.bothReadyOnce.Do(func() { close(b.bothReady) })
		return
	}
	b.peerTimerOnce.Do(func() {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			select {
			case <-b.bothReady:
			case <-time.After(processHandshakeTimeout):
				b.fail(fmt.Errorf("handshake: control/output peer timeout"))
			case <-b.ctx.Done():
			}
		}()
	})
}

func (b *processBroker) handleControl(conn net.Conn, decoder *processwire.Decoder, encoder *processwire.Encoder) {
	for {
		frame, err := decoder.Read()
		if err != nil {
			b.mu.Lock()
			closing := b.closing
			b.mu.Unlock()
			if closing || (errors.Is(err, io.EOF) && b.sessionComplete()) {
				return
			}
			b.fail(fmt.Errorf("control read: %w", err))
			return
		}
		if frame.Stream != processwire.StreamControl {
			b.protocolError(encoder, frame.Seq, "control stream 불일치")
			return
		}
		b.mu.Lock()
		b.controlAckPending = true
		b.mu.Unlock()
		if err := b.handleControlFrame(frame, encoder); err != nil {
			b.protocolError(encoder, frame.Seq, err.Error())
			return
		}
	}
}

func (b *processBroker) handleControlFrame(frame processwire.Frame, encoder *processwire.Encoder) error {
	switch frame.Kind {
	case processwire.KindStart:
		if len(frame.Payload) != 0 {
			return fmt.Errorf("start payload는 비어야 함")
		}
		if err := b.start(frame.Seq); err != nil {
			return err
		}
		return b.sendAck(encoder, frame.Seq)
	case processwire.KindStdinData:
		if err := b.writeStdin(frame.Payload); err != nil {
			return err
		}
		return b.sendAck(encoder, frame.Seq)
	case processwire.KindStdinClose:
		if len(frame.Payload) != 0 {
			return fmt.Errorf("stdin_close payload는 비어야 함")
		}
		if err := b.closeStdin(); err != nil {
			return err
		}
		return b.sendAck(encoder, frame.Seq)
	case processwire.KindStop:
		var stop processwire.Stop
		if err := processwire.Unmarshal(frame.Payload, &stop); err != nil {
			return err
		}
		if stop.Reason == "" {
			return fmt.Errorf("stop reason이 비어 있음")
		}
		if err := b.stopContainer(b.ctx, stop.Reason); err != nil {
			return err
		}
		return b.sendAck(encoder, frame.Seq)
	case processwire.KindWait:
		if len(frame.Payload) != 0 {
			return fmt.Errorf("wait payload는 비어야 함")
		}
		b.mu.Lock()
		if !b.started || b.waitRequested {
			b.mu.Unlock()
			return fmt.Errorf("wait 상태 위반")
		}
		b.waitRequested = true
		b.mu.Unlock()
		if err := b.sendAck(encoder, frame.Seq); err != nil {
			return err
		}
		// Exit observation may race with this request. It is not visible to the
		// peer until the wait ACK is durable on the control stream.
		b.mu.Lock()
		b.waitAcked = true
		result := b.waitResult
		b.mu.Unlock()
		if result != nil {
			return b.sendExit()
		}
		return nil
	default:
		return fmt.Errorf("허용되지 않은 control kind=%d", frame.Kind)
	}
}

func (b *processBroker) sendAck(encoder *processwire.Encoder, requestSeq uint64) error {
	payload, _ := processwire.Marshal(processwire.Ack{RequestSeq: requestSeq})
	if _, err := encoder.Write(processwire.KindAck, processwire.StreamControl, 0, payload); err != nil {
		return err
	}
	// Any control operation may coincide with container exit after wait has
	// been armed. The request ACK is ordered before the asynchronous exit frame.
	b.mu.Lock()
	b.controlAckPending = false
	shouldSendExit := b.waitAcked && b.waitResult != nil && !b.exitSent
	b.mu.Unlock()
	if shouldSendExit {
		return b.sendExit()
	}
	return nil
}

func (b *processBroker) protocolError(encoder *processwire.Encoder, seq uint64, reason string) {
	payload, _ := processwire.Marshal(processwire.WireError{RequestSeq: seq, Reason: reason, Fatal: true})
	_, _ = encoder.Write(processwire.KindError, processwire.StreamControl, 0, payload)
	b.fail(fmt.Errorf("%w: %s", processwire.ErrProtocol, reason))
}

func (b *processBroker) start(_ uint64) error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	b.mu.Lock()
	if b.started || b.closing {
		b.mu.Unlock()
		return fmt.Errorf("start 중복")
	}
	b.mu.Unlock()
	select {
	case <-b.outputReady:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("output connection 대기 timeout")
	case <-b.ctx.Done():
		return b.ctx.Err()
	}
	// Start the sole authoritative wait before start-attach. A container that
	// exits immediately remains observable and is never removed early.
	waiter, err := b.starter.Start(b.ctx, "wait", "--condition", "exited", b.containerID)
	if err != nil {
		return fmt.Errorf("podman wait start: %w", err)
	}
	attach, err := b.starter.Start(b.ctx, "start", "--attach", "--interactive", "--sig-proxy=false", b.containerID)
	if err != nil {
		waiter.Kill()
		waiter.ClosePipes()
		return fmt.Errorf("podman start-attach: %w", err)
	}
	if err := b.ctx.Err(); err != nil {
		attach.Kill()
		attach.ClosePipes()
		waiter.Kill()
		waiter.ClosePipes()
		return err
	}
	b.mu.Lock()
	b.waiter = waiter
	b.attach = attach
	b.started = true
	b.mu.Unlock()
	b.wg.Add(2)
	go b.observeWait(waiter)
	go b.drainAttach(attach)
	return nil
}

func (b *processBroker) writeStdin(payload []byte) error {
	b.mu.Lock()
	attach, started, closed, exited := b.attach, b.started, b.stdinClosed, b.waitResult != nil
	b.mu.Unlock()
	if !started || closed || exited {
		return fmt.Errorf("stdin 상태 위반")
	}
	for len(payload) > 0 {
		n, err := attach.Stdin().Write(payload)
		if err != nil {
			return fmt.Errorf("container stdin: %w", err)
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}

func (b *processBroker) closeStdin() error {
	b.mu.Lock()
	if !b.started || b.stdinClosed {
		b.mu.Unlock()
		return fmt.Errorf("stdin_close 상태 위반")
	}
	b.stdinClosed = true
	attach := b.attach
	b.mu.Unlock()
	return attach.Stdin().Close()
}

func (b *processBroker) observeWait(waiter startedCommand) {
	defer b.wg.Done()
	stdoutCh := make(chan []byte, 1)
	stderrCh := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(io.LimitReader(waiter.Stdout(), maxWaitOutput+1)); stdoutCh <- data }()
	go func() { data, _ := io.ReadAll(io.LimitReader(waiter.Stderr(), maxWaitOutput+1)); stderrCh <- data }()
	<-waiter.Done()
	out, stderr := <-stdoutCh, <-stderrCh
	waiter.ClosePipes()
	result := processwire.ExitObserved{}
	if waiter.ExitErr() != nil {
		result.Code, result.Reason = -1, "podman wait 실패: "+waiter.ExitErr().Error()
	} else if len(out) > maxWaitOutput {
		result.Code, result.Reason = -1, "podman wait 출력 상한 초과"
	} else {
		fields := strings.Fields(string(out))
		code, err := strconv.Atoi(first(fields))
		if err != nil {
			result.Code, result.Reason = -1, "podman wait exit code 형식 오류"
		} else {
			result.Code = code
			result.Reason = "container exited"
		}
	}
	if len(stderr) > maxWaitOutput && result.Code >= 0 {
		result.Code, result.Reason = -1, "podman wait stderr 상한 초과"
	}
	b.mu.Lock()
	b.waitResult = &result
	requested := b.waitAcked && !b.controlAckPending
	attach := b.attach
	b.mu.Unlock()
	if attach != nil {
		_ = attach.Stdin().Close()
	}
	close(b.containerDone)
	if requested {
		if err := b.sendExit(); err != nil {
			b.fail(err)
		}
	}
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (b *processBroker) sendExit() error {
	b.mu.Lock()
	if b.exitSent || b.waitResult == nil || b.controlEncoder == nil {
		b.mu.Unlock()
		return nil
	}
	result := *b.waitResult
	b.exitSent = true
	encoder := b.controlEncoder
	b.mu.Unlock()
	payload, _ := processwire.Marshal(result)
	_, err := encoder.Write(processwire.KindExitObserved, processwire.StreamControl, 0, payload)
	if err == nil {
		b.finishIfComplete()
	}
	return err
}

type outputChunk struct {
	kind   processwire.Kind
	stream processwire.Stream
	data   []byte
}

func (b *processBroker) drainAttach(attach startedCommand) {
	defer b.wg.Done()
	defer close(b.streamDone)
	defer attach.ClosePipes()
	chunks := make(chan outputChunk)
	var readers sync.WaitGroup
	readOne := func(reader io.Reader, kind processwire.Kind, stream processwire.Stream) {
		defer readers.Done()
		buf := make([]byte, processwire.MaxPayload)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				chunk := outputChunk{kind: kind, stream: stream, data: append([]byte(nil), buf[:n]...)}
				select {
				case chunks <- chunk:
				case <-b.ctx.Done():
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
					b.fail(fmt.Errorf("attach output read: %w", err))
				}
				return
			}
		}
	}
	readers.Add(2)
	go readOne(attach.Stdout(), processwire.KindStdoutData, processwire.StreamStdout)
	go readOne(attach.Stderr(), processwire.KindStderrData, processwire.StreamStderr)
	readersDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(chunks)
		close(readersDone)
	}()
	go func() {
		select {
		case <-readersDone:
			return
		case <-b.containerDone:
		}
		// A stopped container has no future output. Give the attach readers a
		// short grace to drain kernel buffers; if the podman attach client still
		// holds a pipe, kill that client so stream teardown cannot consume the
		// lease cleanup budget. Normal output remains untouched when readers
		// reach EOF first.
		timer := time.NewTimer(processAttachExitGrace)
		defer timer.Stop()
		select {
		case <-readersDone:
		case <-timer.C:
			attach.Kill()
		}
	}()
	b.mu.Lock()
	encoder := b.outputEncoder
	b.mu.Unlock()
	for chunk := range chunks {
		if _, err := encoder.Write(chunk.kind, chunk.stream, 0, chunk.data); err != nil {
			b.fail(fmt.Errorf("output write: %w", err))
			return
		}
	}
	// Both attach pipes have reached EOF, so all container bytes have already
	// been forwarded. A podman start --attach client can nevertheless remain
	// alive after a forced container SIGKILL; do not spend the whole lease
	// cleanup budget waiting for that client. Killing only the attach process
	// after the readers finish preserves output while bounding stream teardown.
	if err := waitStartedCommandExit(attach, processAttachExitGrace); err != nil {
		b.fail(fmt.Errorf("attach process exit: %w", err))
		return
	}
	attachErr := attach.ExitErr()
	end := processwire.StreamEnd{}
	if attachErr != nil {
		end.AttachError = attachErr.Error()
	}
	payload, _ := processwire.Marshal(end)
	if _, err := encoder.Write(processwire.KindStreamEnd, processwire.StreamControl, 0, payload); err != nil {
		b.fail(fmt.Errorf("stream_end: %w", err))
		return
	}
	b.mu.Lock()
	b.streamEnded = true
	b.mu.Unlock()
	b.finishIfComplete()
}

func waitStartedCommandExit(process startedCommand, grace time.Duration) error {
	select {
	case <-process.Done():
		return nil
	case <-time.After(grace):
		process.Kill()
	}
	select {
	case <-process.Done():
		return nil
	case <-time.After(grace):
		return fmt.Errorf("attach process did not exit after kill")
	}
}

func (b *processBroker) stopContainer(ctx context.Context, reason string) error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	b.mu.Lock()
	if b.stopReason == "" {
		b.stopReason = reason
	}
	if !b.started || b.waitResult != nil {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()
	if _, err := b.runner.Run(ctx, "stop", "--time", processStopSeconds, b.containerID); err != nil {
		b.mu.Lock()
		done := b.waitResult != nil
		b.mu.Unlock()
		if done {
			return nil
		}
		if _, killErr := b.runner.Run(ctx, "kill", b.containerID); killErr != nil {
			return errors.Join(err, killErr)
		}
	}
	return nil
}

func (b *processBroker) fail(err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	if b.firstErr == nil {
		b.firstErr = errors.Join(ErrProcessBrokerFatal, err)
	}
	already := b.closing
	b.closing = true
	b.mu.Unlock()
	if already {
		return
	}
	b.cancel()
	_ = b.listener.Close()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = b.stopContainer(ctx, "process broker fatal")
		b.closeConnections()
	}()
}

func (b *processBroker) sessionComplete() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exitSent && b.streamEnded
}
func (b *processBroker) finishIfComplete() {
	if b.sessionComplete() {
		b.doneOnce.Do(func() { close(b.done) })
	}
}

func (b *processBroker) closeConnections() {
	b.mu.Lock()
	conns := []net.Conn{b.control, b.output}
	b.mu.Unlock()
	for _, c := range conns {
		if c != nil {
			_ = c.Close()
		}
	}
}

func (b *processBroker) Shutdown(ctx context.Context) error {
	b.lifecycleMu.Lock()
	b.mu.Lock()
	b.closing = true
	started := b.started
	b.mu.Unlock()
	b.lifecycleMu.Unlock()
	_ = b.listener.Close()
	joined := b.stopContainer(ctx, "lease shutdown")
	if started {
		select {
		case <-b.containerDone:
		case <-ctx.Done():
			joined = errors.Join(joined, fmt.Errorf("world/local: process broker container exit observation: %w", ctx.Err()))
		}
		select {
		case <-b.streamDone:
		case <-ctx.Done():
			joined = errors.Join(joined, fmt.Errorf("world/local: process broker output stream drain: %w", ctx.Err()))
		}
	}
	b.cancel()
	b.closeConnections()
	b.mu.Lock()
	attach, waiter := b.attach, b.waiter
	b.mu.Unlock()
	if attach != nil {
		attach.Kill()
		attach.ClosePipes()
	}
	if waiter != nil {
		waiter.Kill()
		waiter.ClosePipes()
	}
	b.wg.Wait()
	if err := os.RemoveAll(b.rootDir); err != nil {
		joined = errors.Join(joined, err)
	}
	b.doneOnce.Do(func() { close(b.done) })
	return errors.Join(joined, b.Err())
}
