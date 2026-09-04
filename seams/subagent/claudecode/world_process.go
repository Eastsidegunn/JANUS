package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/world/processwire"
	"github.com/Eastsidegunn/JANUS/seams/subagent/internal/procgroup"
	"github.com/Eastsidegunn/JANUS/seams/subagent/worldadapter"
)

// runWorldProcess is the local-podman branch of the Claude adapter. The
// Claude executable is inside the world; this process only speaks the
// host-only ProcessEndpoint and parses the native bytes it receives. The
// direct procgroup branch remains in adapter.go for world_backend:none.
func runWorldProcess(ctx context.Context, in io.ReadCloser, stderr io.Writer, cfg Config, vals *validate.Validators, w *wireWriter, approvals approvalTransport, scanner *bufio.Scanner, taskLine []byte) error {
	process, err := worldadapter.ConnectProcess(ctx, cfg.ProcessEndpoint, cfg.WorldSpanID)
	if err != nil {
		return fmt.Errorf("claudecode: process endpoint: %w", err)
	}
	defer process.Close()
	tokenExpired, stopExpiry := monitorTokenExpiry(cfg.TokenExpiresAtUnixMs, process.Done(), func() { stopWorldProcess(process) })
	defer stopExpiry()

	stdoutR, stdoutW := io.Pipe()
	var authDiagnostic bytes.Buffer
	outputDone := make(chan error, 1)
	go func() {
		err := process.DrainOutput(func(frame processwire.Frame) error {
			switch frame.Kind {
			case processwire.KindStdoutData:
				_, err := stdoutW.Write(frame.Payload)
				if authDiagnostic.Len() < 64*1024 {
					remaining := 64*1024 - authDiagnostic.Len()
					capture := frame.Payload
					if len(capture) > remaining {
						capture = capture[:remaining]
					}
					_, _ = authDiagnostic.Write(capture)
				}
				return err
			case processwire.KindStderrData:
				_, err := stderr.Write(frame.Payload)
				if authDiagnostic.Len() < 64*1024 {
					remaining := 64*1024 - authDiagnostic.Len()
					if len(frame.Payload) > remaining {
						frame.Payload = frame.Payload[:remaining]
					}
					_, _ = authDiagnostic.Write(frame.Payload)
				}
				return err
			case processwire.KindStreamEnd:
				return nil
			default:
				return fmt.Errorf("claudecode: process output frame kind=%d", frame.Kind)
			}
		})
		_ = stdoutW.CloseWithError(err)
		outputDone <- err
	}()

	// Process exit is observed by the broker's independent wait path. Approval
	// cleanup is attached to that observation, not to output EOF.
	approvals.attach(process.Done(), func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = process.Stop(stopCtx, "claudecode approval failure")
	})
	startDone := make(chan error, 1)
	go func() { startDone <- process.Start(ctx, taskLine) }()

	parser := NewParser()
	readyEmitted := false
	adapterDone := make(chan struct{})
	commandErr := make(chan error, 1)
	go monitorWorldCommands(scanner, parser, process, approvals, adapterDone, commandErr)

	native := bufio.NewScanner(stdoutR)
	native.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	var pendingDone *Event
	var handlerErr error
	for native.Scan() {
		line := append([]byte(nil), native.Bytes()...)
		events, parseErr := parser.ParseLine(line)
		if parseErr != nil {
			handlerErr = parseErr
			break
		}
		for i := range events {
			e := events[i]
			if e.Kind == gen.EventKindSubagentDone {
				copy := e
				pendingDone = &copy
				continue
			}
			if e.Kind == gen.EventKindSubagentToolCall {
				var intent gen.AgentToolCallPayload
				if err := json.Unmarshal(e.Payload, &intent); err != nil {
					handlerErr = fmt.Errorf("claudecode: tool intent decode: %w", err)
					break
				}
				if err := approvals.registerIntent(intent); err != nil {
					handlerErr = fmt.Errorf("claudecode: world intent 등록: %w", err)
					break
				}
			}
			if handlerErr != nil {
				break
			}
			if err := w.emit(e.Kind, e.Payload, e.Raw); err != nil {
				handlerErr = err
				break
			}
			if e.Kind == gen.EventKindSubagentReady {
				readyEmitted = true
				approvals.markReady()
			}
		}
		if handlerErr != nil {
			break
		}
	}
	close(adapterDone)
	_ = in.Close()
	if handlerErr != nil {
		stopWorldProcess(process)
	}

	var outputErr error
	select {
	case outputErr = <-outputDone:
	case <-time.After(2 * time.Second):
		outputErr = fmt.Errorf("claudecode: process output drain timeout")
	}
	var startErr error
	select {
	case startErr = <-startDone:
	case <-ctx.Done():
		startErr = ctx.Err()
	}
	exit, waitErr := process.Wait(ctx)
	var exitErr error
	if startErr != nil {
		exitErr = startErr
	}
	if waitErr != nil {
		exitErr = errors.Join(exitErr, waitErr)
	} else if exit.Code != 0 {
		exitErr = fmt.Errorf("container exit code %d", exit.Code)
	}
	var scanErr error
	if handlerErr == nil && native.Err() != nil {
		scanErr = native.Err()
	}
	if handlerErr == nil && scanErr == nil && outputErr != nil && !errors.Is(outputErr, io.EOF) {
		scanErr = outputErr
	}
	var cmdErr error
	select {
	case cmdErr = <-commandErr:
	default:
	}
	drain := procgroup.DrainResult{HandlerErr: handlerErr, ScanErr: scanErr, ExitErr: exitErr}
	doneEvent, finishErr := finishNative(drain, pendingDone, parser.StopRequested())
	terminalErr := finishErr
	select {
	case <-tokenExpired:
		// Expiry is a credential failure, not a normal stopped completion. The
		// deterministic done result intentionally contains no token or response
		// body and is emitted below once the ready gate permits a terminal event.
		terminalErr = errTokenExpired
	default:
	}
	if terminalErr == nil {
		terminalErr = cmdErr
	}
	if terminalErr == nil {
		if approvalErr := approvals.failure(); approvalErr != nil {
			terminalErr = fmt.Errorf("claudecode: %w: %v", errApprovalHandshake, approvalErr)
		}
	}
	// Claude exits before stream-json/system-init when no credential is
	// available. That is a recognized authentication gate, not an adapter
	// protocol violation: publish the required terminal event without exposing
	// stderr, response bodies, or any credential material. Other pre-ready
	// failures retain the fail-closed no-output rule.
	if terminalErr != nil && !readyEmitted && authenticationFailure(authDiagnostic.String()) {
		terminalErr = errors.Join(errAuthenticationFailed, terminalErr)
		payload, err := json.Marshal(gen.ReadyPayload{Grade: gen.ReadyPayloadGradeObservable})
		if err != nil {
			return err
		}
		if err := w.emit(gen.EventKindSubagentReady, payload, nil); err != nil {
			return errors.Join(terminalErr, err)
		}
		readyEmitted = true
		approvals.markReady()
	}
	if terminalErr != nil {
		if readyEmitted {
			if err := emitFailureDone(w, terminalErr, parser.StopRequested()); err != nil {
				return errors.Join(terminalErr, fmt.Errorf("claudecode: 오류 done 방출: %w", err))
			}
		}
		return terminalErr
	}
	if !parser.Ready() {
		payload, err := json.Marshal(gen.ReadyPayload{Grade: gen.ReadyPayloadGradeObservable})
		if err != nil {
			return err
		}
		if err := w.emit(gen.EventKindSubagentReady, payload, nil); err != nil {
			return err
		}
		approvals.markReady()
	}
	return w.emit(doneEvent.Kind, doneEvent.Payload, doneEvent.Raw)
}

var errAuthenticationFailed = errors.New("Claude 인증 실패")

func authenticationFailure(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "not logged in") ||
		strings.Contains(lower, "please run /login") ||
		strings.Contains(lower, "authentication failed")
}

// monitorTokenExpiry owns the runtime expiry transition without retaining the
// secret value. A process that exits first is not reclassified as credential
// expiry; only the expiry deadline invokes the stop callback and closes the
// returned marker. The returned cleanup function is idempotent in practice
// because it is called once by runWorldProcess.
func monitorTokenExpiry(expiresAtUnixMs int64, processDone <-chan struct{}, stop func()) (<-chan struct{}, func()) {
	expired := make(chan struct{})
	stopWatch := make(chan struct{})
	var wg sync.WaitGroup
	if expiresAtUnixMs <= 0 {
		return expired, func() {}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		d := time.Until(time.UnixMilli(expiresAtUnixMs))
		if d < 0 {
			d = 0
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			stop()
			close(expired)
		case <-processDone:
		case <-stopWatch:
		}
	}()
	return expired, func() {
		select {
		case <-stopWatch:
		default:
			close(stopWatch)
		}
		wg.Wait()
	}
}

func monitorWorldCommands(scanner *bufio.Scanner, parser *Parser, process *worldadapter.ProcessClient, approvals approvalTransport, adapterDone <-chan struct{}, result chan<- error) {
	fail := func(err error) {
		approvals.denyAll("어댑터 command 오류", true)
		result <- err
		stopWorldProcess(process)
	}
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		vals, err := validate.New()
		if err != nil {
			fail(err)
			return
		}
		if err := vals.ValidateCommand(line); err != nil {
			fail(fmt.Errorf("claudecode: %w: %v", errInboundContract, err))
			return
		}
		var cmd gen.Command
		if err := json.Unmarshal(line, &cmd); err != nil {
			fail(fmt.Errorf("claudecode: %w: %v", errInboundContract, err))
			return
		}
		switch cmd.Cmd {
		case gen.CommandCmdStop:
			approvals.denyAll("중단 요청", true)
			parser.NoteStop()
			result <- nil
			stopWorldProcess(process)
			return
		case gen.CommandCmdApprovalResponse:
			var response gen.ApprovalResponsePayload
			if err := json.Unmarshal(cmd.Payload, &response); err != nil {
				fail(fmt.Errorf("claudecode: %w: %v", errInboundContract, err))
				return
			}
			if err := approvals.resolve(response); err != nil {
				fail(err)
				return
			}
		case gen.CommandCmdMessage:
			fail(fmt.Errorf("claudecode: %w — Claude 단발 print 세션에서 지원되지 않음", errMessageUnsupported))
			return
		case gen.CommandCmdTask:
			fail(fmt.Errorf("claudecode: %w", errDuplicateTask))
			return
		}
	}
	select {
	case <-adapterDone:
		result <- nil
		return
	default:
	}
	fail(fmt.Errorf("claudecode: %w", errCommandInputClosed))
}

func stopWorldProcess(process *worldadapter.ProcessClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = process.Stop(ctx, "adapter termination")
}
