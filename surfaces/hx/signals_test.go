package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/worldtest"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
)

type FakeSignalHub struct {
	mu      sync.Mutex
	targets []chan<- os.Signal
	cancels []context.CancelFunc
}

func (h *FakeSignalHub) hooks(forced chan<- int) signalHooks {
	return signalHooks{
		notifyContext: func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(parent)
			h.mu.Lock()
			h.cancels = append(h.cancels, cancel)
			h.mu.Unlock()
			return ctx, cancel
		},
		notify: func(ch chan<- os.Signal, _ ...os.Signal) { h.register(ch) },
		stop:   func(chan<- os.Signal) {},
		forceExit: func(code int) {
			forced <- code
		},
	}
}

func (h *FakeSignalHub) register(ch chan<- os.Signal) {
	h.mu.Lock()
	h.targets = append(h.targets, ch)
	h.mu.Unlock()
}

func (h *FakeSignalHub) Send(sig os.Signal) {
	h.mu.Lock()
	targets := append([]chan<- os.Signal(nil), h.targets...)
	cancels := append([]context.CancelFunc(nil), h.cancels...)
	h.mu.Unlock()
	// NotifyContext cancellation happens before the parallel signal channel is
	// notified. This makes a regression that passes its context to Launch
	// deterministically kill the adapter before the graceful stop path runs.
	for _, cancel := range cancels {
		cancel()
	}
	for _, target := range targets {
		target <- sig
	}
}

type FakeSignalLifecycle struct {
	releaseOnStop    bool
	release          chan struct{}
	stopOnce         sync.Once
	releaseOnce      sync.Once
	cleanupOnce      sync.Once
	stopCalled       chan struct{}
	stopReason       gen.StopPayloadReason
	stopResult       string
	cleanupCalls     atomic.Int64
	forceCloseCalls  atomic.Int64
	collectionEvents atomic.Int64
}

func newFakeSignalLifecycle(releaseOnStop bool) *FakeSignalLifecycle {
	return &FakeSignalLifecycle{
		releaseOnStop: releaseOnStop,
		release:       make(chan struct{}),
		stopCalled:    make(chan struct{}),
	}
}

func (f *FakeSignalLifecycle) StopWithResult(reason gen.StopPayloadReason, result string) error {
	f.stopReason, f.stopResult = reason, result
	f.stopOnce.Do(func() {
		close(f.stopCalled)
		if f.releaseOnStop {
			f.releaseOnce.Do(func() { close(f.release) })
		}
	})
	return nil
}

func (f *FakeSignalLifecycle) Wait(ctx context.Context) (gen.DonePayload, error) {
	select {
	case <-f.release:
		if f.stopResult != "" {
			return gen.DonePayload{Status: gen.DonePayloadStatusStopped, Result: "adapter stop"}, nil
		}
		return gen.DonePayload{Status: gen.DonePayloadStatusOk, Result: "normal"}, nil
	case <-ctx.Done():
		return gen.DonePayload{}, ctx.Err()
	}
}

func (f *FakeSignalLifecycle) Cleanup(context.Context) error {
	f.cleanupOnce.Do(func() {
		f.cleanupCalls.Add(1)
		f.collectionEvents.Add(1)
	})
	return nil
}

func (f *FakeSignalLifecycle) ForceClose(context.Context) error {
	f.forceCloseCalls.Add(1)
	return nil
}

func (f *FakeSignalLifecycle) Release() {
	f.releaseOnce.Do(func() { close(f.release) })
}

type FakeSignalLauncher struct {
	lifecycle  *FakeSignalLifecycle
	started    chan struct{}
	ctxStopped chan struct{}
	ctxOnce    sync.Once
}

func (f *FakeSignalLauncher) Launch(ctx context.Context, in sessionLaunch) (gen.DonePayload, error) {
	child := logd.NewSpanID()
	parent := in.RootSpan
	spawnPayload, err := json.Marshal(gen.SubagentSpawnPayload{
		Adapter: "fake", Instruction: in.Request.TaskRef.Instruction,
		Budget: gen.SpawnBudget{
			Tokens: in.Sandbox.Budget.Tokens, TimeMs: in.Sandbox.Budget.TimeMs,
			MaxDepth: in.Sandbox.Budget.MaxDepth,
		},
		WorldBackend: gen.SubagentSpawnPayloadWorldBackendNone,
		ControlMode:  gen.SubagentSpawnPayloadControlModeToolApproval,
	})
	if err != nil {
		return gen.DonePayload{}, err
	}
	if _, err := in.Log.Writer.Submit(context.Background(), gen.EventRecord{
		Ts: time.Now().UnixMilli(), TraceID: in.TraceID, SpanID: child, ParentSpanID: &parent,
		Kind: gen.KindSubagentSpawn, Actor: "parent", Payload: spawnPayload,
	}); err != nil {
		return gen.DonePayload{}, err
	}
	readyPayload, err := json.Marshal(gen.ReadyPayload{Grade: gen.ReadyPayloadGradeObservable})
	if err != nil {
		return gen.DonePayload{}, err
	}
	if _, err := in.Log.Writer.Submit(context.Background(), gen.EventRecord{
		Ts: time.Now().UnixMilli(), TraceID: in.TraceID, SpanID: child, ParentSpanID: &parent,
		Kind: gen.KindSubagentReady, Actor: "subagent:fake:1", Payload: readyPayload,
	}); err != nil {
		return gen.DonePayload{}, err
	}
	close(f.started)
	go func() {
		<-ctx.Done()
		f.ctxOnce.Do(func() { close(f.ctxStopped) })
	}()
	done, err := runProductionLifecycle(ctx, in.Signals, f.lifecycle, in.Stderr)
	if err != nil {
		return gen.DonePayload{}, err
	}
	payload, err := json.Marshal(done)
	if err != nil {
		return gen.DonePayload{}, err
	}
	_, err = in.Log.Writer.Submit(context.Background(), gen.EventRecord{
		Ts: time.Now().UnixMilli(), TraceID: in.TraceID, SpanID: child, ParentSpanID: &parent,
		Kind: gen.KindSubagentDone, Actor: "subagent:fake:1", Payload: payload,
	})
	return done, err
}

func TestSIGTERMRecordsStoppedDoneAndRunsCleanup(t *testing.T) {
	fixture := newProductionFixture(t)
	hub := &FakeSignalHub{}
	forced := make(chan int, 1)
	hooks := hub.hooks(forced)
	lifecycle := newFakeSignalLifecycle(false)
	launcher := &FakeSignalLauncher{
		lifecycle: lifecycle, started: make(chan struct{}), ctxStopped: make(chan struct{}),
	}
	requestBytes, err := json.Marshal(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	runDone := make(chan error, 1)
	go func() {
		runDone <- runProduction(parent, productionRun{
			RequestBytes: requestBytes, ProfilePath: fixture.profilePath, AcceptRoot: fixture.acceptRoot,
			Launcher: launcher, Stdout: &stdout, Stderr: &stderr, SignalHooks: &hooks,
		})
	}()
	<-launcher.started
	hub.Send(syscall.SIGTERM)
	<-lifecycle.stopCalled
	if lifecycle.stopReason != gen.StopPayloadReasonUser || lifecycle.stopResult != "signal: SIGTERM" {
		t.Fatalf("stop = %q %q", lifecycle.stopReason, lifecycle.stopResult)
	}
	select {
	case <-launcher.ctxStopped:
		t.Fatal("signal cancellation reached the production launcher context")
	default:
	}
	lifecycle.Release()
	err = <-runDone
	if err == nil || !strings.Contains(err.Error(), "status=stopped") {
		t.Fatalf("signal run error = %v", err)
	}
	if lifecycle.cleanupCalls.Load() != 1 {
		t.Fatalf("cleanup calls = %d", lifecycle.cleanupCalls.Load())
	}
	if got := stderr.String(); !strings.Contains(got, "received SIGTERM") {
		t.Fatalf("stderr diagnostic = %q", got)
	}

	controls := decodeControls(t, stdout.String())
	if len(controls) != 2 || controls[1].Done == nil || controls[1].Done.Status != "stopped" ||
		controls[1].Done.Result != "adapter stop" || controls[1].Done.Reason != "signal: SIGTERM" {
		t.Fatalf("terminal controls = %+v", controls)
	}
	log, err := sqlite.Open(context.Background(), controls[0].SessionRef.SessionDB)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	events, err := log.Reader.ReadFrom(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind != gen.KindSubagentDone {
			continue
		}
		var done gen.DonePayload
		if err := json.Unmarshal(event.Payload, &done); err != nil {
			t.Fatal(err)
		}
		found = done.Status == gen.DonePayloadStatusStopped && done.Result == "adapter stop"
	}
	if !found {
		t.Fatalf("durable stopped signal event missing: %+v", events)
	}
}

type actualSignalLifecycle struct {
	subagent *subagent.Subagent
	cleanup  atomic.Int64
}

func (l *actualSignalLifecycle) StopWithResult(reason gen.StopPayloadReason, result string) error {
	return l.subagent.StopWithResult(reason, result)
}

func (l *actualSignalLifecycle) Wait(ctx context.Context) (gen.DonePayload, error) {
	return l.subagent.Wait(ctx)
}

func (l *actualSignalLifecycle) ForceClose(context.Context) error { return nil }

func (l *actualSignalLifecycle) Cleanup(context.Context) error {
	l.cleanup.Add(1)
	return nil
}

type actualSignalLauncher struct {
	started chan struct{}
	life    *actualSignalLifecycle
}

func (l *actualSignalLauncher) Launch(ctx context.Context, in sessionLaunch) (gen.DonePayload, error) {
	script := `read task
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
read stop
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"stopped","result":"adapter stop"},"raw":""}'`
	sub, err := subagent.Spawn(ctx, in.Log.Writer, in.TraceID, in.RootSpan, 1, subagent.Spec{
		Adapter: "fake", Command: []string{"/bin/sh", "-c", script},
		Instruction: in.Request.TaskRef.Instruction, Workspace: "/workspace",
		Budget: in.Sandbox.Budget, ProfileID: in.Sandbox.ProfileID,
		Approval: policy.ApprovalManual, Decider: policy.DenyAll{},
	})
	if err != nil {
		return gen.DonePayload{}, err
	}
	l.life = &actualSignalLifecycle{subagent: sub}
	close(l.started)
	return runProductionLifecycle(ctx, in.Signals, l.life, in.Stderr)
}

func TestProductionSignalDoesNotCancelAdapterBeforeDurableStoppedDone(t *testing.T) {
	fixture := newProductionFixture(t)
	hub := &FakeSignalHub{}
	forced := make(chan int, 1)
	hooks := hub.hooks(forced)
	launcher := &actualSignalLauncher{started: make(chan struct{})}
	requestBytes, err := json.Marshal(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	runDone := make(chan error, 1)
	go func() {
		runDone <- runProduction(context.Background(), productionRun{
			RequestBytes: requestBytes, ProfilePath: fixture.profilePath, AcceptRoot: fixture.acceptRoot,
			Launcher: launcher, Stdout: &stdout, Stderr: &stderr, SignalHooks: &hooks,
		})
	}()
	<-launcher.started
	hub.Send(syscall.SIGTERM)
	if err := <-runDone; err == nil || !strings.Contains(err.Error(), "status=stopped") {
		t.Fatalf("signal run error = %v", err)
	}
	if launcher.life.cleanup.Load() != 1 {
		t.Fatalf("cleanup calls = %d", launcher.life.cleanup.Load())
	}
	controls := decodeControls(t, stdout.String())
	if len(controls) != 2 || controls[1].Done == nil || controls[1].Done.Status != "stopped" ||
		controls[1].Done.Result != "adapter stop" || controls[1].Done.Reason != "signal: SIGTERM" {
		t.Fatalf("terminal controls = %+v", controls)
	}
	log, err := sqlite.Open(context.Background(), controls[0].SessionRef.SessionDB)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	events, err := log.Reader.ReadFrom(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var durable []gen.DonePayload
	for _, event := range events {
		if event.Kind == gen.KindSubagentDone {
			var done gen.DonePayload
			if err := json.Unmarshal(event.Payload, &done); err != nil {
				t.Fatal(err)
			}
			durable = append(durable, done)
		}
	}
	if len(durable) != 1 || durable[0].Status != gen.DonePayloadStatusStopped || durable[0].Result != "adapter stop" {
		t.Fatalf("durable adapter done = %+v; stderr=%q", durable, stderr.String())
	}
}

func TestSecondSignalForceClosesBeforeGracefulCleanupRecordsCollection(t *testing.T) {
	hub := &FakeSignalHub{}
	forced := make(chan int, 1)
	hooks := hub.hooks(forced)
	ctx, signals := startSessionSignals(context.Background(), &hooks)
	defer signals.stop()
	lifecycle := newFakeSignalLifecycle(false)
	done := make(chan error, 1)
	go func() {
		_, err := runProductionLifecycle(ctx, signals, lifecycle, io.Discard)
		done <- err
	}()
	hub.Send(syscall.SIGTERM)
	<-lifecycle.stopCalled
	hub.Send(syscall.SIGINT)
	select {
	case code := <-forced:
		if code != 130 {
			t.Fatalf("forced exit code = %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("second signal did not force exit")
	}
	if lifecycle.forceCloseCalls.Load() != 1 {
		t.Fatalf("forced close calls = %d", lifecycle.forceCloseCalls.Load())
	}
	if lifecycle.cleanupCalls.Load() != 0 || lifecycle.collectionEvents.Load() != 0 {
		t.Fatalf("forced path consumed graceful cleanup: cleanup=%d collection=%d",
			lifecycle.cleanupCalls.Load(), lifecycle.collectionEvents.Load())
	}
	lifecycle.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if lifecycle.cleanupCalls.Load() != 1 || lifecycle.collectionEvents.Load() != 1 {
		t.Fatalf("graceful cleanup did not record collection: cleanup=%d collection=%d",
			lifecycle.cleanupCalls.Load(), lifecycle.collectionEvents.Load())
	}
}

func TestSecondSignalBeforeLifecycleClosesActiveLease(t *testing.T) {
	hub := &FakeSignalHub{}
	forced := make(chan int, 1)
	hooks := hub.hooks(forced)
	_, signals := startSessionSignals(context.Background(), &hooks)
	defer signals.stop()
	lease := worldtest.NewFakeActiveLease(world.ProcessEndpoint{}, world.ApprovalEndpoint{}, t.TempDir(), nil)
	armForcedLeaseCleanup(signals, &activeWorldSubagent{Lease: lease})
	hub.Send(syscall.SIGTERM)
	hub.Send(syscall.SIGINT)
	select {
	case code := <-forced:
		if code != 130 {
			t.Fatalf("forced exit code = %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("second signal did not force exit")
	}
	if got := lease.FakeCloseOrder(); len(got) != 4 {
		t.Fatalf("active lease cleanup stages = %v", got)
	}
}

func TestProductionLifecycleWithoutSignalIsUnchanged(t *testing.T) {
	lifecycle := newFakeSignalLifecycle(false)
	lifecycle.Release()
	done, err := runProductionLifecycle(context.Background(), nil, lifecycle, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != gen.DonePayloadStatusOk || done.Result != "normal" {
		t.Fatalf("normal done = %+v", done)
	}
	select {
	case <-lifecycle.stopCalled:
		t.Fatal("normal completion requested stop")
	default:
	}
	if lifecycle.cleanupCalls.Load() != 1 {
		t.Fatalf("normal cleanup calls = %d", lifecycle.cleanupCalls.Load())
	}
}

func TestTerminalReasonRequiresGracefulLifecycleStart(t *testing.T) {
	signals := &sessionSignals{gracefulStarted: make(chan struct{}), firstSignal: syscall.SIGTERM}
	if got := signals.terminalReason(); got != "" {
		t.Fatalf("late unconsumed signal reason = %q", got)
	}
	signals.markGracefulStarted()
	if got := signals.terminalReason(); got != "signal: SIGTERM" {
		t.Fatalf("consumed signal reason = %q", got)
	}
}
