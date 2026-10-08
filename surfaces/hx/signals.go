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
	// forcedCleanupTimeout bounds a single escalation command or the wait for a
	// final-fallback Close attempt. The ordinary lifecycle retains the full
	// gracefulCleanupTimeout in which to record done{stopped} and collection.
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
	second chan os.Signal
	done   chan struct{}
	// gracefulStarted is closed after the lifecycle has consumed the first
	// signal. It is test-visible synchronization for the second-signal path.
	gracefulStarted chan struct{}

	mu           sync.Mutex
	forceCleanup func()
	firstSignal  os.Signal
	secondSignal os.Signal
	pendingExit  os.Signal
	lifecycleRun bool
	// cleanupTimeout is injectable only through the package-local test seam.
	// Production always initializes it to gracefulCleanupTimeout.
	cleanupTimeout time.Duration
	hooks          signalHooks
	signalCh       chan os.Signal
	stopContext    context.CancelFunc
	stopOnce       sync.Once
	gracefulOnce   sync.Once
}

func startSessionSignals(parent context.Context, hooks *signalHooks) (context.Context, *sessionSignals) {
	h := defaultSignalHooks()
	if hooks != nil {
		h = *hooks
	}
	ctx, stopContext := h.notifyContext(parent, os.Interrupt, syscall.SIGTERM)
	s := &sessionSignals{
		parent: parent, first: make(chan os.Signal, 1), second: make(chan os.Signal, 1), done: make(chan struct{}),
		gracefulStarted: make(chan struct{}), hooks: h,
		cleanupTimeout: gracefulCleanupTimeout,
		signalCh:       make(chan os.Signal, 2), stopContext: stopContext,
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
		if s.lifecycleRun {
			s.secondSignal = second
			s.mu.Unlock()
			s.second <- second
			return
		}
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
	reason := "signal: " + signalName(s.firstSignal)
	if s.secondSignal != nil {
		reason += "; escalated by " + signalName(s.secondSignal)
	}
	return reason
}

func (s *sessionSignals) beginLifecycle() {
	s.mu.Lock()
	s.lifecycleRun = true
	s.mu.Unlock()
}

func (s *sessionSignals) endLifecycle() os.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycleRun = false
	return s.secondSignal
}

func (s *sessionSignals) lifecycleTimeout() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupTimeout
}

// forceExitAfterLifecycle is called by the owner immediately after Launch has
// returned from its Wait and Cleanup path. The final fallback and the
// pre-lifecycle path exit directly instead and never arm this handoff.
func (s *sessionSignals) forceExitAfterLifecycle() {
	s.mu.Lock()
	sig := s.pendingExit
	s.pendingExit = nil
	s.mu.Unlock()
	if sig != nil {
		s.hooks.forceExit(signalExitCode(sig))
	}
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
	KillAgent(context.Context) error
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
	lifecycleEnded := false
	if signals != nil {
		waitCtx = signals.parent
		signals.beginLifecycle()
		defer func() {
			if !lifecycleEnded {
				signals.endLifecycle()
			}
		}()
	}
	waited := make(chan lifecycleResult, 1)
	go func() {
		done, err := lifecycle.Wait(waitCtx)
		waited <- lifecycleResult{done: done, err: err}
	}()

	var result lifecycleResult
	var stopErr error
	var escalated os.Signal
	var escalationTimer *time.Timer
	var escalationDeadline <-chan time.Time
	escalate := func(sig os.Signal) {
		if escalated != nil {
			return
		}
		escalated = sig
		escalationTimer = time.NewTimer(signals.lifecycleTimeout())
		escalationDeadline = escalationTimer.C
		fmt.Fprintf(stderr, "hx run: received %s; stop escalated, killing agent container immediately\n", signalName(sig))
		killCtx, cancel := context.WithTimeout(context.Background(), forcedCleanupTimeout)
		if err := lifecycle.KillAgent(killCtx); err != nil {
			fmt.Fprintf(stderr, "hx run: escalated agent kill failed; awaiting lifecycle fallback: %v\n", err)
		}
		cancel()
	}
	forceFallback := func() error {
		fmt.Fprintln(stderr, "hx run: escalated lifecycle did not finish within 30s; final fallback closes the lease and terminal records may be absent")
		closeCtx, cancel := context.WithTimeout(context.Background(), forcedCleanupTimeout)
		closed := make(chan struct{})
		go func() {
			_ = lifecycle.ForceClose(closeCtx)
			close(closed)
		}()
		select {
		case <-closed:
		case <-closeCtx.Done():
		}
		cancel()
		signals.hooks.forceExit(signalExitCode(escalated))
		return errors.New("escalated lifecycle timeout after final lease close")
	}
	if signals == nil {
		result = <-waited
	} else {
		select {
		case result = <-waited:
		case sig := <-signals.first:
			signals.markGracefulStarted()
			reason := "signal: " + signalName(sig)
			fmt.Fprintf(stderr, "hx run: received %s; stopping session\n", signalName(sig))
			stopErr = lifecycle.StopWithResult(gen.StopPayloadReasonUser, reason)
		waiting:
			for {
				select {
				case result = <-waited:
					break waiting
				case second := <-signals.second:
					escalate(second)
				case <-escalationDeadline:
					return gen.DonePayload{}, forceFallback()
				}
			}
		}
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), gracefulCleanupTimeout)
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- lifecycle.Cleanup(cleanupCtx) }()
	var cleanupErr error
	if signals == nil {
		cleanupErr = <-cleanupDone
	} else {
	cleanup:
		for {
			select {
			case cleanupErr = <-cleanupDone:
				break cleanup
			case second := <-signals.second:
				escalate(second)
			case <-escalationDeadline:
				cancel()
				return gen.DonePayload{}, forceFallback()
			}
		}
	}
	cancel()
	if signals != nil {
		second := signals.endLifecycle()
		lifecycleEnded = true
		// Cleanup may become ready in the same instant the watcher dispatches
		// the second signal. Closing the lifecycle gate under the same mutex
		// makes that race deterministic: a signal already assigned to this
		// lifecycle is escalated here; a later one takes the direct fallback.
		if escalated == nil && second != nil {
			escalate(second)
		}
	}
	if escalationTimer != nil {
		escalationTimer.Stop()
	}
	if escalated != nil {
		signals.mu.Lock()
		signals.pendingExit = escalated
		signals.mu.Unlock()
	}
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
