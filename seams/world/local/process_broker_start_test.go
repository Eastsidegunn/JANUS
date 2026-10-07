package local

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/core/runtimedir"
	"github.com/Eastsidegunn/JANUS/core/world/processwire"
)

// fakePreStartRuntime models Podman's pre-start wait answer (T27 (f)): every
// `wait --condition exited` start hands out the next scripted waiter, and
// `container inspect` reports the scripted container status.
type fakePreStartRuntime struct {
	mu         sync.Mutex
	waiters    []startedCommand
	attach     *fakeStartedCommand
	status     []string // consumed per inspect; the last value repeats
	inspectErr error
	starts     [][]string
	runs       [][]string
	waitArmed  chan int
	onStop     func()
}

func (r *fakePreStartRuntime) Start(_ context.Context, args ...string) (startedCommand, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "wait" {
		n := 0
		for _, a := range r.starts {
			if a[0] == "wait" {
				n++
			}
		}
		if n > len(r.waiters) {
			return nil, errors.New("fakePreStartRuntime: unscripted wait")
		}
		select {
		case r.waitArmed <- n:
		default:
		}
		return r.waiters[n-1], nil
	}
	return r.attach, nil
}

func (r *fakePreStartRuntime) Run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.runs = append(r.runs, append([]string(nil), args...))
	hook := r.onStop
	var out []byte
	var err error
	if len(args) >= 2 && args[0] == "container" && args[1] == "inspect" {
		if r.inspectErr != nil {
			err = r.inspectErr
		} else {
			out = []byte(r.status[0] + "\n")
			if len(r.status) > 1 {
				r.status = r.status[1:]
			}
		}
	}
	r.mu.Unlock()
	if len(args) > 0 && args[0] == "stop" && hook != nil {
		hook()
	}
	return out, err
}

// signalingWaiter closes read on the first Stdout call. The broker reads a
// waiter's output only at the top of its observation loop, i.e. after any
// pre-start stop re-application for the previous iteration has returned, so
// read is an exact synchronisation point for "start confirmed".
type signalingWaiter struct {
	*fakeStartedCommand
	read chan struct{}
	once sync.Once
}

func newSignalingWaiter(t *testing.T) *signalingWaiter {
	return &signalingWaiter{fakeStartedCommand: newFakeStartedCommand(t), read: make(chan struct{})}
}

func (w *signalingWaiter) Stdout() io.ReadCloser {
	w.once.Do(func() { close(w.read) })
	return w.fakeStartedCommand.Stdout()
}

func (r *fakePreStartRuntime) currentStatus() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status[0]
}

func (r *fakePreStartRuntime) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, a := range r.runs {
		if len(a) > 0 && a[0] == "stop" {
			n++
		}
	}
	return n
}

func (r *fakePreStartRuntime) setStatus(status ...string) {
	r.mu.Lock()
	r.status = status
	r.mu.Unlock()
}

func (r *fakePreStartRuntime) startLines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.starts))
	for i, a := range r.starts {
		out[i] = strings.Join(a, " ")
	}
	return out
}

func (r *fakePreStartRuntime) inspectCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, a := range r.runs {
		if len(a) >= 2 && a[0] == "container" && a[1] == "inspect" {
			if strings.Join(a, " ") != "container inspect --format {{.State.Status}} "+fakeAgentID {
				panic("unexpected inspect argv: " + strings.Join(a, " "))
			}
			n++
		}
	}
	return n
}

func newPreStartBroker(t *testing.T, r *fakePreStartRuntime) *processBroker {
	t.Helper()
	b, err := startProcessBroker(context.Background(), strings.Repeat("2", 16), strings.Repeat("1", 64), fakeAgentID, r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func awaitWaitArmed(t *testing.T, r *fakePreStartRuntime, want int) {
	t.Helper()
	select {
	case n := <-r.waitArmed:
		if n != want {
			t.Fatalf("armed wait #%d, want #%d", n, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("wait #%d was not armed", want)
	}
}

func assertNoExitObserved(t *testing.T, b *processBroker, client *processClient, when string) {
	t.Helper()
	select {
	case <-b.containerDone:
		t.Fatalf("%s: containerDone closed before the container started", when)
	default:
	}
	_ = client.control.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	f, err := client.controlDec.Read()
	_ = client.control.SetReadDeadline(time.Time{})
	if err == nil {
		t.Fatalf("%s: control frame kind=%d payload=%s before the container started", when, f.Kind, f.Payload)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("%s: control read err=%v", when, err)
	}
}

func readExitObserved(t *testing.T, client *processClient) processwire.ExitObserved {
	t.Helper()
	f := readFrame(t, client.control, client.controlDec, "exit_observed")
	if f.Kind != processwire.KindExitObserved {
		t.Fatalf("exit kind=%d payload=%s", f.Kind, f.Payload)
	}
	var exit processwire.ExitObserved
	if err := processwire.Unmarshal(f.Payload, &exit); err != nil {
		t.Fatal(err)
	}
	return exit
}

func drainToStreamEnd(t *testing.T, client *processClient) {
	t.Helper()
	for {
		if f := readFrame(t, client.output, client.outputDec, "stream end"); f.Kind == processwire.KindStreamEnd {
			return
		}
	}
}

// T27 (f) defect 1: the authoritative wait is armed before start-attach, and
// Podman answers it with 0 while the container is still Configured/Created.
// That answer must never become ExitObserved, whatever state the container has
// reached by the time it is inspected; only the post-start wait is published,
// including a genuine exit code 0 after the start.
func TestProcessBrokerPreStartWaitZeroIsNotExitObserved(t *testing.T) {
	w1, w2, w3, w4, attach := newFakeStartedCommand(t), newFakeStartedCommand(t), newFakeStartedCommand(t), newFakeStartedCommand(t), newFakeStartedCommand(t)
	// Three pre-start answers. The third is inspected only after the
	// container has started (running): it is still the pre-start 0.
	w1.completeWait("0")
	w2.completeWait("0")
	w3.completeWait("0")
	r := &fakePreStartRuntime{waiters: []startedCommand{w1, w2, w3, w4}, attach: attach, status: []string{"created", "configured", "running"}, waitArmed: make(chan int, 8)}
	b := newPreStartBroker(t, r)
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	for n := 1; n <= 4; n++ {
		awaitWaitArmed(t, r, n)
	}
	assertNoExitObserved(t, b, client, "pre-start wait answers")
	if got := r.inspectCount(); got != 3 {
		t.Fatalf("inspect count=%d, want 3 (one per pre-start answer)", got)
	}
	// The post-start wait observes a genuine exit 0; it is published as is.
	w4.completeWait("0")
	if exit := readExitObserved(t, client); exit.Code != 0 || exit.Reason != waitReasonExited {
		t.Fatalf("started container exit=%+v", exit)
	}
	if got := r.inspectCount(); got != 3 {
		t.Fatalf("post-start wait was inspected again: inspect count=%d", got)
	}
	attach.closeWriters()
	attach.finish(nil)
	drainToStreamEnd(t, client)
	wait := "wait --condition exited " + fakeAgentID
	want := []string{wait, "start --attach --interactive --sig-proxy=false " + fakeAgentID, wait, wait, wait}
	if got := r.startLines(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lifecycle commands=%v want %v", got, want)
	}
	shutdownBroker(t, b)
}

func TestProcessBrokerUsesHXRuntimeDir(t *testing.T) {
	runtimeRoot, err := os.MkdirTemp("/tmp", "hxd6-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	t.Setenv("HX_RUNTIME_DIR", runtimeRoot)
	runtime := &fakeProcessRuntime{waiter: newFakeStartedCommand(t), attach: newFakeStartedCommand(t)}
	b := mustProcessBroker(t, context.Background(), runtime)
	if !strings.HasPrefix(b.rootDir, runtimeRoot+string(filepath.Separator)) {
		t.Fatalf("process root=%q is outside HX_RUNTIME_DIR=%q", b.rootDir, runtimeRoot)
	}
	const prefix = "hxp-"
	if got := len(b.socketPath) - len(runtimeRoot) - 1; got-len(filepath.Base(b.rootDir))+len(prefix)+10 > runtimedir.MaxSocketSuffixBytes {
		t.Fatalf("process socket suffix=%d exceeds %d", got, runtimedir.MaxSocketSuffixBytes)
	}
	shutdownBrokerAllowError(t, b)
}

// Review F1: a pre-start 0 inspected after the start (any started-side or
// unknown state) must be discarded, and the exit code of the post-start wait
// is the one published — never a fabricated 0 while the container lives.
func TestProcessBrokerPreStartZeroInspectedAfterStartIsDiscarded(t *testing.T) {
	for _, status := range []string{"running", "exited", "stopped", "stopping", "paused", "unknown-state"} {
		t.Run(status, func(t *testing.T) {
			w1, w2, attach := newFakeStartedCommand(t), newFakeStartedCommand(t), newFakeStartedCommand(t)
			w1.completeWait("0")
			r := &fakePreStartRuntime{waiters: []startedCommand{w1, w2}, attach: attach, status: []string{status}, waitArmed: make(chan int, 4)}
			b := newPreStartBroker(t, r)
			client := connectProcessClient(t, b)
			defer client.close()
			client.send(t, processwire.KindStart, nil)
			client.ack(t, "start")
			client.send(t, processwire.KindWait, nil)
			client.ack(t, "wait")
			awaitWaitArmed(t, r, 1)
			awaitWaitArmed(t, r, 2)
			assertNoExitObserved(t, b, client, "pre-start 0 inspected as "+status)
			w2.completeWait("5")
			if exit := readExitObserved(t, client); exit.Code != 5 {
				t.Fatalf("published exit=%+v, want the post-start wait's 5", exit)
			}
			if got := r.inspectCount(); got != 1 {
				t.Fatalf("inspect count=%d, want 1", got)
			}
			attach.closeWriters()
			attach.finish(nil)
			drainToStreamEnd(t, client)
			shutdownBroker(t, b)
		})
	}
}

// Review F2: a stop that reaches the container before it starts is a
// successful no-op in Podman (Created container), so it neither stops nor
// escalates. Once the start is confirmed the stop is applied again, and the
// post-start Podman wait remains the termination authority.
func TestProcessBrokerStopBeforeStartIsReappliedAfterStart(t *testing.T) {
	w1, w2, w3, attach := newFakeStartedCommand(t), newFakeStartedCommand(t), newFakeStartedCommand(t), newFakeStartedCommand(t)
	w1.completeWait("0")
	r := &fakePreStartRuntime{waiters: []startedCommand{w1, w2, w3}, attach: attach, status: []string{"created"}, waitArmed: make(chan int, 4)}
	b := newPreStartBroker(t, r)
	r.onStop = func() {
		// Podman: stop on a Created container succeeds and does nothing.
		if r.currentStatus() == "running" {
			r.setStatus("exited")
			w3.completeWait("143")
		}
	}
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	awaitWaitArmed(t, r, 1)
	awaitWaitArmed(t, r, 2)
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	stopPayload, _ := processwire.Marshal(processwire.Stop{Reason: "test stop"})
	client.send(t, processwire.KindStop, stopPayload)
	client.ack(t, "stop")
	if got := r.stopCount(); got != 1 {
		t.Fatalf("stop count=%d before start", got)
	}
	assertNoExitObserved(t, b, client, "pre-start stop no-op")
	// start-attach now starts the container; the wait armed before that
	// start still answers with the pre-start 0.
	r.setStatus("running")
	w2.completeWait("0")
	awaitWaitArmed(t, r, 3)
	if exit := readExitObserved(t, client); exit.Code != 143 {
		t.Fatalf("stopped exit=%+v, want 143 from the re-applied stop", exit)
	}
	if got := r.stopCount(); got != 2 {
		t.Fatalf("stop count=%d, want the pre-start stop re-applied once", got)
	}
	drainToStreamEnd(t, client)
	shutdownBroker(t, b)
}

// A stop after the start was confirmed goes through the regular T10 stop path
// and is not repeated.
func TestProcessBrokerStopAfterPreStartWaitUsesRearmedWait(t *testing.T) {
	w1, w2, attach := newFakeStartedCommand(t), newSignalingWaiter(t), newFakeStartedCommand(t)
	w1.completeWait("0")
	r := &fakePreStartRuntime{waiters: []startedCommand{w1, w2}, attach: attach, status: []string{"running"}, waitArmed: make(chan int, 4)}
	b := newPreStartBroker(t, r)
	r.onStop = func() {
		// Podman: stop on a Created container succeeds and does nothing; on a
		// running container it terminates it.
		if r.currentStatus() == "running" {
			r.setStatus("exited")
			w2.completeWait("143")
		}
	}
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	awaitWaitArmed(t, r, 1)
	awaitWaitArmed(t, r, 2)
	// The observer reads w2 only after the start-confirmation iteration,
	// including any stop re-application, has returned.
	select {
	case <-w2.read:
	case <-time.After(2 * time.Second):
		t.Fatal("post-start wait was not observed")
	}
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	assertNoExitObserved(t, b, client, "before stop")
	stopPayload, _ := processwire.Marshal(processwire.Stop{Reason: "test stop"})
	client.send(t, processwire.KindStop, stopPayload)
	client.ack(t, "stop")
	if exit := readExitObserved(t, client); exit.Code != 143 {
		t.Fatalf("stopped exit=%+v", exit)
	}
	if got := r.stopCount(); got != 1 {
		t.Fatalf("post-start stop count=%d, want 1", got)
	}
	drainToStreamEnd(t, client)
	shutdownBroker(t, b)
}

// If start-attach has already ended and the container never left the
// pre-start states, no start can follow: the session ends with a failed exit
// observation instead of a fabricated exit 0, and nothing is re-armed.
func TestProcessBrokerContainerNeverStartedIsFailedExit(t *testing.T) {
	w1, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	attach.closeWriters()
	attach.finish(errors.New("start-attach failed"))
	w1.completeWait("0")
	r := &fakePreStartRuntime{waiters: []startedCommand{w1}, attach: attach, status: []string{"created"}, waitArmed: make(chan int, 4)}
	b := newPreStartBroker(t, r)
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	exit := readExitObserved(t, client)
	if exit.Code != -1 || !strings.Contains(exit.Reason, "시작되지 않은") {
		t.Fatalf("never-started exit=%+v", exit)
	}
	drainToStreamEnd(t, client)
	if got := len(r.startLines()); got != 2 {
		t.Fatalf("never-started container re-armed wait: starts=%v", r.startLines())
	}
	shutdownBroker(t, b)
}

// An exit 0 whose start cannot be confirmed is not published as a clean exit.
func TestProcessBrokerUnconfirmedStartIsFailedExit(t *testing.T) {
	w1, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	w1.completeWait("0")
	r := &fakePreStartRuntime{waiters: []startedCommand{w1}, attach: attach, inspectErr: errors.New("no such container"), waitArmed: make(chan int, 4)}
	b := newPreStartBroker(t, r)
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	if exit := readExitObserved(t, client); exit.Code != -1 || !strings.Contains(exit.Reason, "inspect") {
		t.Fatalf("unconfirmed start exit=%+v", exit)
	}
	attach.closeWriters()
	attach.finish(nil)
	drainToStreamEnd(t, client)
	shutdownBroker(t, b)
}

// Non-zero codes cannot be Podman's pre-start answer and are published without
// a state probe, exactly as before.
func TestProcessBrokerNonZeroWaitSkipsStartProbe(t *testing.T) {
	w1, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	w1.completeWait("7")
	r := &fakePreStartRuntime{waiters: []startedCommand{w1}, attach: attach, status: []string{"created"}, waitArmed: make(chan int, 4)}
	b := newPreStartBroker(t, r)
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	if exit := readExitObserved(t, client); exit.Code != 7 {
		t.Fatalf("exit=%+v", exit)
	}
	if got := r.inspectCount(); got != 0 {
		t.Fatalf("inspect count=%d for non-zero exit", got)
	}
	attach.closeWriters()
	attach.finish(nil)
	drainToStreamEnd(t, client)
	shutdownBroker(t, b)
}
