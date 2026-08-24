package local

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/core/world/processwire"
)

type fakeStartedCommand struct {
	stdinR, stdoutW, stderrW *os.File
	stdinW, stdoutR, stderrR *os.File
	done                     chan struct{}
	mu                       sync.Mutex
	exitErr                  error
	finishOnce               sync.Once
	closeOnce                sync.Once
}

func newFakeStartedCommand(t *testing.T) *fakeStartedCommand {
	t.Helper()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return &fakeStartedCommand{stdinR: stdinR, stdinW: stdinW, stdoutR: stdoutR, stdoutW: stdoutW, stderrR: stderrR, stderrW: stderrW, done: make(chan struct{})}
}
func (p *fakeStartedCommand) Stdin() io.WriteCloser { return p.stdinW }
func (p *fakeStartedCommand) Stdout() io.ReadCloser { return p.stdoutR }
func (p *fakeStartedCommand) Stderr() io.ReadCloser { return p.stderrR }
func (p *fakeStartedCommand) Done() <-chan struct{} { return p.done }
func (p *fakeStartedCommand) ExitErr() error        { p.mu.Lock(); defer p.mu.Unlock(); return p.exitErr }
func (p *fakeStartedCommand) Kill()                 { p.finish(errors.New("killed")); p.closeWriters() }
func (p *fakeStartedCommand) ClosePipes() {
	p.closeOnce.Do(func() { _ = p.stdinW.Close(); _ = p.stdoutR.Close(); _ = p.stderrR.Close(); _ = p.stdinR.Close() })
}
func (p *fakeStartedCommand) finish(err error) {
	p.finishOnce.Do(func() { p.mu.Lock(); p.exitErr = err; p.mu.Unlock(); close(p.done) })
}
func (p *fakeStartedCommand) completeWait(code string) {
	_, _ = io.WriteString(p.stdoutW, code+"\n")
	p.closeWriters()
	p.finish(nil)
}
func (p *fakeStartedCommand) closeWriters() { _ = p.stdoutW.Close(); _ = p.stderrW.Close() }

type fakeProcessRuntime struct {
	mu             sync.Mutex
	waiter, attach *fakeStartedCommand
	starts         [][]string
	runs           [][]string
	onStop         func()
}

func (r *fakeProcessRuntime) Start(_ context.Context, args ...string) (startedCommand, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "wait" {
		return r.waiter, nil
	}
	return r.attach, nil
}
func (r *fakeProcessRuntime) Run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.runs = append(r.runs, append([]string(nil), args...))
	hook := r.onStop
	r.mu.Unlock()
	if len(args) > 0 && args[0] == "stop" && hook != nil {
		hook()
	}
	return nil, nil
}
func (r *fakeProcessRuntime) counts() (waitStarts, attachStarts, stops, kills int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.starts {
		if len(a) > 0 && a[0] == "wait" {
			waitStarts++
		} else if len(a) > 0 && a[0] == "start" {
			attachStarts++
		}
	}
	for _, a := range r.runs {
		if len(a) > 0 && a[0] == "stop" {
			stops++
		}
		if len(a) > 0 && a[0] == "kill" {
			kills++
		}
	}
	return
}
func (r *fakeProcessRuntime) hasRun(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.runs {
		if len(a) > 0 && a[0] == name {
			return true
		}
	}
	return false
}
func (r *fakeProcessRuntime) startSnapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.starts))
	for i := range r.starts {
		out[i] = append([]string(nil), r.starts[i]...)
	}
	return out
}

type processClient struct {
	control    net.Conn
	output     net.Conn
	controlEnc *processwire.Encoder
	controlDec *processwire.Decoder
	outputDec  *processwire.Decoder
}

func connectProcessClient(t *testing.T, b *processBroker) *processClient {
	t.Helper()
	ep := b.Endpoint()
	out, err := net.Dial(ep.Network(), ep.Address())
	if err != nil {
		t.Fatal(err)
	}
	outEnc := processwire.NewEncoder(out)
	outDec := processwire.NewDecoder(out)
	helloOut, _ := processwire.Marshal(processwire.Hello{Version: processwire.Version, Role: processwire.RoleOutput, LeaseID: b.leaseID, SpanID: b.spanID, Capability: ep.OutputCapability()})
	if _, err = outEnc.Write(processwire.KindHello, processwire.StreamControl, 0, helloOut); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, out, outDec, "output hello"); f.Kind != processwire.KindHelloAck {
		t.Fatalf("output hello kind=%d", f.Kind)
	}
	control, err := net.Dial(ep.Network(), ep.Address())
	if err != nil {
		t.Fatal(err)
	}
	controlEnc := processwire.NewEncoder(control)
	controlDec := processwire.NewDecoder(control)
	helloControl, _ := processwire.Marshal(processwire.Hello{Version: processwire.Version, Role: processwire.RoleControl, LeaseID: b.leaseID, SpanID: b.spanID, Capability: ep.ControlCapability()})
	if _, err = controlEnc.Write(processwire.KindHello, processwire.StreamControl, 0, helloControl); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, control, controlDec, "control hello"); f.Kind != processwire.KindHelloAck {
		t.Fatalf("control hello kind=%d", f.Kind)
	}
	return &processClient{control: control, output: out, controlEnc: controlEnc, controlDec: controlDec, outputDec: outDec}
}
func (c *processClient) close() { _ = c.control.Close(); _ = c.output.Close() }
func (c *processClient) send(t *testing.T, kind processwire.Kind, payload []byte) {
	t.Helper()
	if _, err := c.controlEnc.Write(kind, processwire.StreamControl, 0, payload); err != nil {
		t.Fatal(err)
	}
}
func (c *processClient) ack(t *testing.T, what string) {
	t.Helper()
	f := readFrame(t, c.control, c.controlDec, what)
	if f.Kind != processwire.KindAck {
		t.Fatalf("%s kind=%d payload=%s", what, f.Kind, f.Payload)
	}
}

func TestProcessBrokerExitObservationIsIndependentOfOutputEOF(t *testing.T) {
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	// This models a container that has already exited before the wait command's
	// observer goroutine runs. No --rm or early remove may erase exit status.
	waiter.completeWait("7")
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start ack")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait ack")
	exitFrame := readFrame(t, client.control, client.controlDec, "exit_observed")
	if exitFrame.Kind != processwire.KindExitObserved {
		t.Fatalf("kind=%d", exitFrame.Kind)
	}
	var exit processwire.ExitObserved
	if err := processwire.Unmarshal(exitFrame.Payload, &exit); err != nil {
		t.Fatal(err)
	}
	if exit.Code != 7 {
		t.Fatalf("authoritative exit=%d", exit.Code)
	}
	_ = client.output.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	if _, err := client.outputDec.Read(); err == nil {
		t.Fatal("descendant-held output EOF 전에 stream_end가 도착함")
	}
	_ = client.output.SetReadDeadline(time.Time{})
	_, _ = io.WriteString(attach.stdoutW, "late-output")
	attach.closeWriters()
	attach.finish(nil)
	var output []byte
	for {
		f := readFrame(t, client.output, client.outputDec, "output drain")
		if f.Kind == processwire.KindStreamEnd {
			break
		}
		output = append(output, f.Payload...)
	}
	if string(output) != "late-output" {
		t.Fatalf("drained output=%q", output)
	}
	waitStarts, attachStarts, _, _ := runtime.counts()
	if waitStarts != 1 || attachStarts != 1 {
		t.Fatalf("starts wait=%d attach=%d", waitStarts, attachStarts)
	}
	starts := runtime.startSnapshot()
	if strings.Join(starts[0], " ") != "wait --condition exited "+fakeAgentID || strings.Join(starts[1], " ") != "start --attach --interactive --sig-proxy=false "+fakeAgentID {
		t.Fatalf("process lifecycle commands=%v", starts)
	}
	if runtime.hasRun("rm") {
		t.Fatal("exit 상태 관측 전에 container rm이 실행됨")
	}
	shutdownBroker(t, b)
	if _, err := attach.Stdin().Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed stdin err=%v", err)
	}
	if _, err := attach.Stdout().Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed stdout err=%v", err)
	}
}

func TestProcessBrokerStopIsIdempotentAndLateStopDoesNotSignalCompletedCID(t *testing.T) {
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	b := mustProcessBroker(t, context.Background(), runtime)
	// Force exit observation to finish while the stop handler still owns the
	// request. ExitObserved must not overtake the stop ACK.
	runtime.onStop = func() {
		waiter.completeWait("143")
		<-b.containerDone
		attach.closeWriters()
		attach.finish(errors.New("attach stopped"))
	}
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	stopPayload, _ := processwire.Marshal(processwire.Stop{Reason: "test stop"})
	client.send(t, processwire.KindStop, stopPayload)
	client.ack(t, "stop")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	if f := readFrame(t, client.control, client.controlDec, "exit"); f.Kind != processwire.KindExitObserved {
		t.Fatalf("exit kind=%d", f.Kind)
	}
	client.send(t, processwire.KindStop, stopPayload)
	client.ack(t, "late stop")
	_, _, stops, kills := runtime.counts()
	if stops != 1 || kills != 0 {
		t.Fatalf("late stop re-signaled CID: stop=%d kill=%d", stops, kills)
	}
	for {
		if f := readFrame(t, client.output, client.outputDec, "stream end"); f.Kind == processwire.KindStreamEnd {
			break
		}
	}
	shutdownBroker(t, b)
}

func TestProcessBrokerExitObservationCannotOvertakeWaitAck(t *testing.T) {
	waiter := newFakeStartedCommand(t)
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	b := &processBroker{
		controlEncoder: processwire.NewEncoder(server),
		waitRequested:  true,
		containerDone:  make(chan struct{}),
	}
	waiter.completeWait("0")
	b.wg.Add(1)
	go b.observeWait(waiter)
	select {
	case <-b.containerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("exit observation timeout")
	}

	decoder := processwire.NewDecoder(client)
	_ = client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := decoder.Read(); err == nil {
		t.Fatal("wait ACK 전 exit frame이 노출됨")
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("wait ACK 전 read err=%v", err)
	}
	_ = client.SetReadDeadline(time.Time{})

	b.mu.Lock()
	b.waitAcked = true
	b.mu.Unlock()
	sent := make(chan error, 1)
	go func() { sent <- b.sendExit() }()
	if frame := readFrame(t, client, decoder, "acked exit"); frame.Kind != processwire.KindExitObserved {
		t.Fatalf("acked exit kind=%d payload=%s", frame.Kind, frame.Payload)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	b.wg.Wait()
}

func TestProcessBrokerOutputBackpressureReachesContainerPipeWithoutLoss(t *testing.T) {
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	want := bytes.Repeat([]byte("b"), 8<<20)
	written := make(chan error, 1)
	go func() {
		data := want
		for len(data) > 0 {
			n, err := attach.stdoutW.Write(data)
			if err != nil {
				written <- err
				return
			}
			if n == 0 {
				written <- io.ErrShortWrite
				return
			}
			data = data[n:]
		}
		written <- nil
	}()
	select {
	case err := <-written:
		t.Fatalf("output consumer 정지 전에 container write가 완료됨: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	got := make([]byte, 0, len(want))
	for len(got) < len(want) {
		f := readFrame(t, client.output, client.outputDec, "backpressure drain")
		if f.Kind != processwire.KindStdoutData {
			t.Fatalf("drain kind=%d", f.Kind)
		}
		got = append(got, f.Payload...)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer 재개 뒤 container write timeout")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("backpressure drain bytes=%d want=%d", len(got), len(want))
	}
	waiter.completeWait("0")
	attach.closeWriters()
	attach.finish(nil)
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	_ = readFrame(t, client.control, client.controlDec, "exit")
	for {
		if f := readFrame(t, client.output, client.outputDec, "stream end"); f.Kind == processwire.KindStreamEnd {
			break
		}
	}
	shutdownBroker(t, b)
}

func TestProcessBrokerResourceExhaustionIsFatalBeforeContainerStart(t *testing.T) {
	runtime := &fakeProcessRuntime{waiter: newFakeStartedCommand(t), attach: newFakeStartedCommand(t)}
	b := mustProcessBroker(t, context.Background(), runtime)
	conn, err := net.Dial(b.Endpoint().Network(), b.Endpoint().Address())
	if err != nil {
		t.Fatal(err)
	}
	enc := processwire.NewEncoder(conn)
	dec := processwire.NewDecoder(conn)
	hello, _ := processwire.Marshal(processwire.Hello{Version: processwire.Version, Role: processwire.RoleControl, LeaseID: b.leaseID, SpanID: b.spanID, Capability: b.Endpoint().ControlCapability()})
	_, _ = enc.Write(processwire.KindHello, processwire.StreamControl, 0, hello)
	_ = readFrame(t, conn, dec, "hello")
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(processwire.MaxFrameBytes+1))
	_, _ = conn.Write(prefix[:])
	err = waitBrokerError(t, b)
	if !errors.Is(err, ErrProcessBrokerFatal) || !errors.Is(err, processwire.ErrResourceExhausted) {
		t.Fatalf("fatal err=%v", err)
	}
	waitStarts, attachStarts, stops, kills := runtime.counts()
	if waitStarts+attachStarts+stops+kills != 0 {
		t.Fatalf("exhaustion side effects: %d %d %d %d", waitStarts, attachStarts, stops, kills)
	}
	_ = conn.Close()
	shutdownBrokerAllowError(t, b)
}

func TestProcessBrokerParentCancellationStopsAndWaits(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	runtime.onStop = func() {
		waiter.completeWait("137")
		attach.closeWriters()
		attach.finish(errors.New("attach canceled"))
	}
	b := mustProcessBroker(t, parent, runtime)
	client := connectProcessClient(t, b)
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	cancel()
	err := waitBrokerError(t, b)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel fatal=%v", err)
	}
	select {
	case <-b.containerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel 뒤 authoritative wait timeout")
	}
	_, _, stops, _ := runtime.counts()
	if stops != 1 {
		t.Fatalf("cancel stop calls=%d", stops)
	}
	client.close()
	shutdownBrokerAllowError(t, b)
}

func TestProcessBrokerRejectsRoleReconnectBeforeContainerStart(t *testing.T) {
	runtime := &fakeProcessRuntime{waiter: newFakeStartedCommand(t), attach: newFakeStartedCommand(t)}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	defer client.close()
	conn, err := net.Dial(b.Endpoint().Network(), b.Endpoint().Address())
	if err != nil {
		t.Fatal(err)
	}
	enc := processwire.NewEncoder(conn)
	hello, _ := processwire.Marshal(processwire.Hello{Version: processwire.Version, Role: processwire.RoleOutput, LeaseID: b.leaseID, SpanID: b.spanID, Capability: b.Endpoint().OutputCapability()})
	_, _ = enc.Write(processwire.KindHello, processwire.StreamControl, 0, hello)
	err = waitBrokerError(t, b)
	if !errors.Is(err, ErrProcessBrokerFatal) || !errors.Is(err, processwire.ErrProtocol) {
		t.Fatalf("reconnect err=%v", err)
	}
	w, a, s, k := runtime.counts()
	if w+a+s+k != 0 {
		t.Fatalf("reconnect side effects=%d/%d/%d/%d", w, a, s, k)
	}
	_ = conn.Close()
	shutdownBrokerAllowError(t, b)
}

func TestProcessBrokerOutputDisconnectIsFatalBeforeContainerStart(t *testing.T) {
	runtime := &fakeProcessRuntime{waiter: newFakeStartedCommand(t), attach: newFakeStartedCommand(t)}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	_ = client.output.Close()
	err := waitBrokerError(t, b)
	if !errors.Is(err, ErrProcessBrokerFatal) {
		t.Fatalf("disconnect err=%v", err)
	}
	w, a, s, k := runtime.counts()
	if w+a+s+k != 0 {
		t.Fatalf("disconnect side effects=%d/%d/%d/%d", w, a, s, k)
	}
	_ = client.control.Close()
	shutdownBrokerAllowError(t, b)
}

func TestProcessBrokerRepeatedUnusedLeaseDoesNotAccumulateGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for range 5 {
		r := &fakeProcessRuntime{waiter: newFakeStartedCommand(t), attach: newFakeStartedCommand(t)}
		b := mustProcessBroker(t, context.Background(), r)
		shutdownBroker(t, b)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+3 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+3 {
		t.Fatalf("goroutine baseline=%d after=%d", baseline, got)
	}
}

func mustProcessBroker(t *testing.T, parent context.Context, r *fakeProcessRuntime) *processBroker {
	t.Helper()
	b, err := startProcessBroker(parent, strings.Repeat("2", 16), strings.Repeat("1", 64), fakeAgentID, r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func readFrame(t *testing.T, conn net.Conn, dec *processwire.Decoder, what string) processwire.Frame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	f, err := dec.Read()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return f
}
func shutdownBroker(t *testing.T, b *processBroker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
func shutdownBrokerAllowError(t *testing.T, b *processBroker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = b.Shutdown(ctx)
}
func waitBrokerError(t *testing.T, b *processBroker) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := b.Err(); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("broker error timeout")
	return nil
}
