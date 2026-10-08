package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

const (
	gracefulCleanupTimeout = 30 * time.Second
	// forcedCleanupTimeout bounds only the wait for the lease close token. Once
	// the forced close holds the token, each lease phase runs on its own
	// cancellation-free context, so a second signal can still take as long as
	// `podman stop` (tens of seconds) — containers are never orphaned.
	forcedCleanupTimeout = time.Second
)

type signalHooks struct {
	notifyContext func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)
	notify        func(chan<- os.Signal, ...os.Signal)
	stop          func(chan<- os.Signal)
	forceExit     func(int)
}

func defaultSignalHooks() signalHooks {
	return signalHooks{
		notifyContext: signal.NotifyContext,
		notify:        signal.Notify,
		stop:          signal.Stop,
		forceExit:     os.Exit,
	}
}

// sessionSignals exists only while the production session writer is open.
// NotifyContext exposes optional assembly cancellation, but process lifetime is
// driven only by the parallel channel, which preserves the first signal's
// identity and observes a second signal.
type sessionSignals struct {
	parent context.Context
	first  chan os.Signal
	done   chan struct{}
	// gracefulStarted is closed after the lifecycle has consumed the first
	// signal. It is test-visible synchronization for the second-signal path.
	gracefulStarted chan struct{}

	mu           sync.Mutex
	forceCleanup func()
	firstSignal  os.Signal
	hooks        signalHooks
	signalCh     chan os.Signal
	stopContext  context.CancelFunc
	stopOnce     sync.Once
	gracefulOnce sync.Once
}

func startSessionSignals(parent context.Context, hooks *signalHooks) (context.Context, *sessionSignals) {
	h := defaultSignalHooks()
	if hooks != nil {
		h = *hooks
	}
	ctx, stopContext := h.notifyContext(parent, os.Interrupt, syscall.SIGTERM)
	s := &sessionSignals{
		parent: parent, first: make(chan os.Signal, 1), done: make(chan struct{}),
		gracefulStarted: make(chan struct{}), hooks: h,
		signalCh: make(chan os.Signal, 2), stopContext: stopContext,
	}
	h.notify(s.signalCh, os.Interrupt, syscall.SIGTERM)
	go s.watch()
	return ctx, s
}

func (s *sessionSignals) watch() {
	var first os.Signal
	select {
	case first = <-s.signalCh:
		s.mu.Lock()
		s.firstSignal = first
		s.mu.Unlock()
		s.first <- first
	case <-s.done:
		return
	case <-s.parent.Done():
		return
	}
	select {
	case second := <-s.signalCh:
		s.mu.Lock()
		cleanup := s.forceCleanup
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		s.hooks.forceExit(signalExitCode(second))
	case <-s.done:
	case <-s.parent.Done():
	}
}

func (s *sessionSignals) markGracefulStarted() {
	s.gracefulOnce.Do(func() { close(s.gracefulStarted) })
}

func (s *sessionSignals) terminalReason() string {
	select {
	case <-s.gracefulStarted:
	default:
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstSignal == nil {
		return ""
	}
	return "signal: " + signalName(s.firstSignal)
}

func (s *sessionSignals) setForceCleanup(cleanup func()) {
	s.mu.Lock()
	s.forceCleanup = cleanup
	s.mu.Unlock()
}

func (s *sessionSignals) stop() {
	s.stopOnce.Do(func() {
		s.hooks.stop(s.signalCh)
		s.stopContext()
		close(s.done)
	})
}

func signalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return sig.String()
	}
}

func signalExitCode(sig os.Signal) int {
	if value, ok := sig.(syscall.Signal); ok {
		return 128 + int(value)
	}
	return 1
}

type productionLifecycle interface {
	StopWithResult(gen.StopPayloadReason, string) error
	Wait(context.Context) (gen.DonePayload, error)
	ForceClose(context.Context) error
	Cleanup(context.Context) error
}

type lifecycleResult struct {
	done gen.DonePayload
	err  error
}

// runProductionLifecycle is the one production terminal path for normal and
// signalled sessions: the signal branch adds Stop, then rejoins Wait and the
// same cleanup hook used by ordinary completion.
func runProductionLifecycle(ctx context.Context, signals *sessionSignals, lifecycle productionLifecycle, stderr io.Writer) (gen.DonePayload, error) {
	if stderr == nil {
		stderr = io.Discard
	}
	waitCtx := ctx
	if signals != nil {
		waitCtx = signals.parent
	}
	waited := make(chan lifecycleResult, 1)
	go func() {
		done, err := lifecycle.Wait(waitCtx)
		waited <- lifecycleResult{done: done, err: err}
	}()

	var result lifecycleResult
	var stopErr error
	if signals == nil {
		result = <-waited
	} else {
		signals.setForceCleanup(func() {
			forceCtx, cancel := context.WithTimeout(context.Background(), forcedCleanupTimeout)
			defer cancel()
			_ = lifecycle.ForceClose(forceCtx)
		})
		defer signals.setForceCleanup(nil)
		select {
		case result = <-waited:
		case sig := <-signals.first:
			signals.markGracefulStarted()
			reason := "signal: " + signalName(sig)
			fmt.Fprintf(stderr, "hx run: received %s; stopping session\n", signalName(sig))
			stopErr = lifecycle.StopWithResult(gen.StopPayloadReasonUser, reason)
			result = <-waited
		}
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), gracefulCleanupTimeout)
	cleanupErr := lifecycle.Cleanup(cleanupCtx)
	cancel()
	if result.err != nil || cleanupErr != nil {
		var waitErr error
		if result.err != nil {
			waitErr = fmt.Errorf("session wait: %w", result.err)
		}
		return gen.DonePayload{}, errors.Join(waitErr, cleanupErr)
	}
	if stopErr != nil {
		fmt.Fprintf(stderr, "hx run: graceful stop command failed after signal: %v\n", stopErr)
	}
	return result.done, nil
}
