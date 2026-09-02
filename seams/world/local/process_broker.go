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
	processReadPollInterval = 100 * time.Millisecond
	maxWaitOutput           = 4096
)

var ErrProcessBrokerFatal = errors.New("world/local: process broker fatal")
var ErrStreamConsumerGone = errors.New("world/local: process broker stream consumer gone")

type streamStage string

const (
	streamStageChunkForward   streamStage = "chunk-forward"
	streamStageOutputWrite    streamStage = "output-write"
	streamStageReaderDrain    streamStage = "reader-drain"
	streamStageAttachExit     streamStage = "attach-exit"
	streamStageStreamEndWrite streamStage = "stream-end-write"
)

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
	outputMu                            sync.Mutex
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
	outputPeerGone                      bool
	stageMu                             sync.Mutex
	activeStages                        map[streamStage]time.Time
	firstBlockedStage                   streamStage

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
		outputReady: make(chan struct{}), bothReady: make(chan struct{}), containerDone: make(chan struct{}), streamDone: make(chan struct{}), done: make(chan struct{}), activeStages: make(map[streamStage]time.Time),
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

func (b *processBroker) enterStreamStage(stage streamStage) func() {
	b.stageMu.Lock()
	if b.activeStages == nil {
		b.activeStages = make(map[streamStage]time.Time)
	}
	b.activeStages[stage] = time.Now()
	b.stageMu.Unlock()
	return func() {
		b.stageMu.Lock()
		delete(b.activeStages, stage)
		b.stageMu.Unlock()
	}
}

func (b *processBroker) streamStageDiagnostic(markBlocked bool) string {
	b.stageMu.Lock()
	defer b.stageMu.Unlock()
	stage := streamStage("")
	started := time.Time{}
	// A reader-drain watcher exists for most of the session. Prefer the narrow
	// operation that can actually block progress when stages overlap.
	for _, candidate := range []streamStage{
		streamStageOutputWrite,
		streamStageChunkForward,
		streamStageStreamEndWrite,
		streamStageAttachExit,
		streamStageReaderDrain,
	} {
		if at, ok := b.activeStages[candidate]; ok {
			stage, started = candidate, at
			break
		}
	}
	if markBlocked && b.firstBlockedStage == "" && stage != "" {
		b.firstBlockedStage = stage
	}
	if stage == "" {
		stage = b.firstBlockedStage
	}
	if stage == "" {
		return "stage=none"
	}
	if started.IsZero() {
		return fmt.Sprintf("stage=%s", stage)
	}
	return fmt.Sprintf("stage=%s elapsed=%s", stage, time.Since(started).Round(time.Millisecond))
}

func (b *processBroker) markOutputPeerGone() {
	b.mu.Lock()
	b.outputPeerGone = true
	output := b.output
	b.mu.Unlock()
	// Closing the broker side is the transport-level wakeup for an encoder Write
	// already blocked in the kernel. Peer EOF alone is not guaranteed to wake a
	// concurrent writer promptly on every Unix implementation.
	if output != nil {
		_ = output.Close()
	}
}

func (b *processBroker) expectedConsumerGone() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.outputPeerGone && b.stopReason != ""
}

func (b *processBroker) stopRequested() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopReason != ""
}

func (b *processBroker) stoppedContainerExited() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopReason != "" && b.waitResult != nil
}

func (b *processBroker) expectedStoppedReadEnd(err error) bool {
	if !b.stopRequested() {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, os.ErrClosed) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

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
	if b.expectedStopConsumerGone(err) {
		b.markOutputPeerGone()
		return
	}
	b.fail(fmt.Errorf("output peer disconnect: %w", err))
}

func (b *processBroker) expectedStopConsumerGone(err error) bool {
	if err == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopReason == "" {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && !netErr.Timeout()
}

func (b *processBroker) expectedStopControlGone(err error) bool {
	if err == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopReason == "" || !b.exitSent {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && !netErr.Timeout()
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
			// After an explicit stop, ExitObserved is the final control-plane
			// obligation. The adapter may close this socket once it has consumed
			// that frame; a disconnect before exitSent remains broker-fatal.
			if b.expectedStopControlGone(err) {
				return
			}
			// Include the active stream stage in the fatal diagnostic. Control EOF
			// can be a consequence of an output-side backpressure failure; without
			// this marker the integration gate cannot distinguish the initiating
			// stage from the peer's final disconnect.
			b.mu.Lock()
			started, waitRequested, waitAcked := b.started, b.waitRequested, b.waitAcked
			exitSent, streamEnded := b.exitSent, b.streamEnded
			stopReason, waitResult := b.stopReason, b.waitResult
			b.mu.Unlock()
			b.fail(fmt.Errorf("control read: %w (%s started=%t wait_requested=%t wait_acked=%t exit_sent=%t stream_ended=%t stop=%t wait_result=%t)",
				err, b.streamStageDiagnostic(true), started, waitRequested, waitAcked, exitSent, streamEnded, stopReason != "", waitResult != nil))
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
			// A stopped adapter is permitted to close after its durable done. If
			// the Stop ACK was delivered and the only remaining control obligation
			// was ExitObserved, a peer-close error is the approved
			// consumer-gone-after-done terminal state, not a protocol violation.
			if b.expectedStopControlGone(err) {
				return
			}
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
	go b.forceStoppedStreamEnd()
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
	stopped := b.stopReason != ""
	b.mu.Unlock()
	close(b.containerDone)
	if attach != nil && stopped {
		// The authoritative wait has observed container termination. For an
		// explicit stop, close the parent attach pipes immediately so a Podman /
		// conmon-held writer cannot strand the drain readers. Natural exits retain
		// the full output-drain path; stopped test agents have no post-stop output
		// to preserve.
		interruptAttach(attach)
	} else if attach != nil {
		_ = attach.Stdin().Close()
	}
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
	forwardDone := make(chan struct{})
	defer close(forwardDone)
	var readers sync.WaitGroup
	readOne := func(reader io.Reader, kind processwire.Kind, stream processwire.Stream) {
		defer readers.Done()
		buf := make([]byte, processwire.MaxPayload)
		for {
			if deadline, ok := reader.(readDeadlineSetter); ok {
				// The deadline only returns control from a blocking kernel Read. A
				// timeout never decides termination: only the authoritative Podman
				// wait result plus an explicit stop does that.
				_ = deadline.SetReadDeadline(time.Now().Add(processReadPollInterval))
			}
			n, err := reader.Read(buf)
			if n > 0 {
				chunk := outputChunk{kind: kind, stream: stream, data: append([]byte(nil), buf[:n]...)}
				leave := b.enterStreamStage(streamStageChunkForward)
				select {
				case chunks <- chunk:
				case <-forwardDone:
					leave()
					return
				case <-b.ctx.Done():
					leave()
					return
				}
				leave()
			}
			if err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					if b.stoppedContainerExited() {
						return
					}
					continue
				}
				if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) && !b.expectedStoppedReadEnd(err) {
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
		leave := b.enterStreamStage(streamStageReaderDrain)
		readers.Wait()
		leave()
		close(chunks)
		close(readersDone)
	}()
	go func() {
		select {
		case <-readersDone:
			return
		case <-b.containerDone:
		}
		b.mu.Lock()
		stopped := b.stopReason != ""
		b.mu.Unlock()
		if !stopped {
			// Natural, abnormal, and orphan exits must retain the original
			// full-drain behavior; there may still be valid output to forward.
			return
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
			// Podman/conmon may retain the pipe writer after the client leader is
			// killed. This path is only for an explicitly stopped container; close
			// the parent read ends as the final bounded-release operation so the
			// drain readers cannot hold the lease indefinitely.
			interruptAttach(attach)
		}
	}()
	b.mu.Lock()
	encoder := b.outputEncoder
	b.mu.Unlock()
	for chunk := range chunks {
		if err := b.writeOutput(encoder, chunk.kind, chunk.stream, chunk.data); err != nil {
			if b.expectedConsumerGone() || b.expectedStopConsumerGone(err) {
				b.markOutputPeerGone()
				b.markStreamEnded()
				return
			}
			b.fail(fmt.Errorf("output write: %w", err))
			return
		}
	}
	// Both attach pipes have reached EOF, so all container bytes have already
	// been forwarded. A podman start --attach client can nevertheless remain
	// alive after a forced container SIGKILL; do not spend the whole lease
	// cleanup budget waiting for that client. Killing only the attach process
	// after the readers finish preserves output while bounding stream teardown.
	if b.stopRequested() {
		// Explicit stop plus completed reader drain is authoritative: the
		// container cannot produce more bytes and every buffered byte has already
		// entered the broker stream. End the attach client now instead of using a
		// grace timeout as the normal stopped-path termination mechanism.
		attach.Kill()
	}
	leaveAttach := b.enterStreamStage(streamStageAttachExit)
	err := waitStartedCommandExit(attach, processAttachExitGrace)
	leaveAttach()
	if err != nil {
		b.fail(fmt.Errorf("attach process exit: %w", err))
		return
	}
	attachErr := attach.ExitErr()
	end := processwire.StreamEnd{}
	if attachErr != nil {
		end.AttachError = attachErr.Error()
	}
	if err := b.sendStreamEnd(encoder, end); err != nil {
		if b.expectedConsumerGone() || b.expectedStopConsumerGone(err) {
			b.markOutputPeerGone()
			b.markStreamEnded()
			return
		}
		b.fail(fmt.Errorf("stream_end: %w", err))
		return
	}
	b.finishIfComplete()
}

func (b *processBroker) writeOutput(encoder *processwire.Encoder, kind processwire.Kind, stream processwire.Stream, payload []byte) error {
	b.outputMu.Lock()
	defer b.outputMu.Unlock()
	b.mu.Lock()
	ended := b.streamEnded
	b.mu.Unlock()
	if ended {
		return nil
	}
	leave := b.enterStreamStage(streamStageOutputWrite)
	_, err := encoder.Write(kind, stream, 0, payload)
	leave()
	return err
}

func (b *processBroker) sendStreamEnd(encoder *processwire.Encoder, end processwire.StreamEnd) error {
	b.outputMu.Lock()
	defer b.outputMu.Unlock()
	b.mu.Lock()
	if b.streamEnded {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()
	payload, _ := processwire.Marshal(end)
	leave := b.enterStreamStage(streamStageStreamEndWrite)
	_, err := encoder.Write(processwire.KindStreamEnd, processwire.StreamControl, 0, payload)
	leave()
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.streamEnded = true
	b.mu.Unlock()
	return nil
}

func (b *processBroker) markStreamEnded() {
	b.mu.Lock()
	b.streamEnded = true
	b.mu.Unlock()
	b.finishIfComplete()
}

func (b *processBroker) forceStoppedStreamEnd() {
	select {
	case <-b.containerDone:
	case <-b.ctx.Done():
		return
	}
	b.mu.Lock()
	stopped := b.stopReason != ""
	encoder := b.outputEncoder
	b.mu.Unlock()
	if !stopped || encoder == nil {
		return
	}
	timer := time.NewTimer(processAttachExitGrace)
	defer timer.Stop()
	select {
	case <-b.streamDone:
		return
	case <-timer.C:
	}
	// Explicit stop has no post-stop agent output. If Podman attach/conmon has
	// not delivered EOF by now, close the client and publish the terminal frame
	// from the broker so the adapter cannot remain circularly blocked on EOF.
	b.closeStoppedAttachPipes()
	if err := b.sendStreamEnd(encoder, processwire.StreamEnd{AttachError: "stopped attach stream forced closed"}); err != nil {
		b.fail(fmt.Errorf("stream_end: %w", err))
		return
	}
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
			b.closeStoppedAttachPipes()
			return nil
		}
		if _, killErr := b.runner.Run(ctx, "kill", b.containerID); killErr != nil {
			return errors.Join(err, killErr)
		}
	}
	return nil
}

func (b *processBroker) closeStoppedAttachPipes() {
	b.mu.Lock()
	attach := b.attach
	b.mu.Unlock()
	if attach != nil {
		// Explicit stop has completed at the container boundary. The attach
		// client must not keep its parent pipes open while the broker drains the
		// terminal stream. Kill the client as well as closing its parent ends;
		// ClosePipes is idempotent and the process reaper still owns command exit
		// observation.
		interruptAttach(attach)
	}
}

type readDeadlineSetter interface {
	SetReadDeadline(time.Time) error
}

func interruptAttach(attach startedCommand) {
	// On Linux, closing a pipe fd in another goroutine does not reliably wake a
	// read already executing in the kernel. Container exit is the authoritative
	// proof that no new bytes can be produced; expire both reads at that boundary
	// before closing the parent pipe ends. This is event-driven, not a timeout
	// used as a normal termination mechanism.
	for _, reader := range []io.ReadCloser{attach.Stdout(), attach.Stderr()} {
		if deadline, ok := reader.(readDeadlineSetter); ok {
			_ = deadline.SetReadDeadline(time.Now())
		}
	}
	attach.Kill()
	attach.ClosePipes()
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
		containerObserved := false
		select {
		case <-b.containerDone:
			containerObserved = true
		case <-ctx.Done():
			joined = errors.Join(joined, fmt.Errorf("world/local: process broker container exit observation: %w (%s)", ctx.Err(), b.streamStageDiagnostic(true)))
		}
		if !containerObserved {
			// No container boundary was observed. Cancel and close the transport now;
			// otherwise reader goroutines can remain blocked on a dead attach.
			b.cancel()
			b.closeConnections()
			b.closeStartedCommands()
		}
		select {
		case <-b.streamDone:
		case <-time.After(2 * time.Second):
			joined = errors.Join(joined, fmt.Errorf("world/local: process broker output stream drain: timeout (%s)", b.streamStageDiagnostic(true)))
			b.cancel()
			b.closeConnections()
			b.closeStartedCommands()
			select {
			case <-b.streamDone:
			case <-time.After(2 * time.Second):
				joined = errors.Join(joined, fmt.Errorf("world/local: process broker output stream drain: forced cleanup timeout (%s)", b.streamStageDiagnostic(true)))
			}
		}
	}
	b.cancel()
	b.closeConnections()
	b.closeStartedCommands()
	b.wg.Wait()
	if err := os.RemoveAll(b.rootDir); err != nil {
		joined = errors.Join(joined, err)
	}
	b.doneOnce.Do(func() { close(b.done) })
	return errors.Join(joined, b.Err())
}

func (b *processBroker) closeStartedCommands() {
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
}
