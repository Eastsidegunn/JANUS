package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/seams/subagent/internal/procgroup"
)

const (
	defaultClaudeExecutable = "claude"
	claudeSettingSources    = "project,local"
	maxCommandBytes         = 4 << 20

	// C안(제안서 §7.2): user settings를 제외하고, 이 인라인 PreToolUse
	// hook만 명시적으로 주입한다. --bare는 OAuth/keychain을 읽지 않으므로
	// 금지하고, --safe-mode는 hook 자체를 끄므로 금지한다. Phase C가
	// hxapprove 실행 파일과 소켓 환경을 배선한다.
	claudeApprovalHookSettings = `{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"hxapprove","timeout":600}]}]}}`
)

var (
	errMessageUnsupported = errors.New("message 미지원")
	errInboundContract    = errors.New("수신 command 계약 위반")
	errNativeContract     = errors.New("native stream 계약 위반")
	errNativeRead         = errors.New("native stream 읽기 실패")
	errUnmatchedApproval  = errors.New("미상관 approval_response")
	errDuplicateTask      = errors.New("task 중복")
	errCommandRead        = errors.New("command 읽기 실패")
	errCommandInputClosed = errors.New("실행 중 stdin 종료")
)

// Config contains host-controlled process settings. ClaudeBin is a single
// executable path, never a shell command.
type Config struct {
	ClaudeBin string
	Env       []string
}

// ConfigFromEnv returns the production configuration. HX_CLAUDE_BIN exists so
// tests and operators can select an installed Claude binary without a shell.
func ConfigFromEnv() Config {
	bin := os.Getenv("HX_CLAUDE_BIN")
	if bin == "" {
		bin = defaultClaudeExecutable
	}
	return Config{ClaudeBin: bin, Env: os.Environ()}
}

// wireWriter serializes and validates every adapter → core event.
type wireWriter struct {
	mu   sync.Mutex
	out  io.Writer
	vals *validate.Validators
}

func (w *wireWriter) emit(kind gen.EventKind, payload json.RawMessage, raw []byte) error {
	line, err := json.Marshal(gen.Event{
		V: 1, Kind: kind, Payload: payload, Raw: RawB64(raw),
	})
	if err != nil {
		return err
	}
	if err := w.vals.ValidateEvent(line); err != nil {
		return fmt.Errorf("claudecode: 발신 이벤트가 계약 위반: %w", err)
	}
	line = append(line, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(line) > 0 {
		n, err := w.out.Write(line)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		line = line[n:]
	}
	return nil
}

// Run executes one §5.2 task session. Native completion never waits for the
// command scanner: on some platforms closing a pipe does not interrupt an
// already-blocked read. The process exit reclaims that goroutine and fd.
func Run(ctx context.Context, in io.ReadCloser, out, stderr io.Writer, cfg Config) error {
	if cfg.ClaudeBin == "" {
		return fmt.Errorf("claudecode: Claude 실행 파일이 비어 있음")
	}
	vals, err := validate.New()
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxCommandBytes)

	cmd, err := readCommand(scanner, vals)
	if err != nil {
		return err
	}
	if cmd.Cmd != gen.CommandCmdTask {
		return fmt.Errorf("claudecode: 첫 command는 task여야 함 (got %s)", cmd.Cmd)
	}
	var task gen.TaskPayload
	if err := json.Unmarshal(cmd.Payload, &task); err != nil {
		return fmt.Errorf("claudecode: task payload: %w", err)
	}

	// task.Workspace는 policy/T10이 준비한 pristine 작업공간이다. Claude의
	// cwd를 이 경로로 고정하고 user settings는 flag로 제외한다.
	native, err := procgroup.Start(ctx, procgroup.Options{
		Command: claudeCommand(cfg, task), Dir: task.Workspace, Env: cfg.Env, Stderr: stderr,
	})
	if err != nil {
		return fmt.Errorf("claudecode: Claude 실행: %w", err)
	}

	parser := NewParser()
	w := &wireWriter{out: out, vals: vals}
	readyEmitted := false
	adapterDone := make(chan struct{})
	commandErr := make(chan error, 1)
	go monitorCommands(scanner, vals, parser, native, adapterDone, commandErr)

	var pendingDone *Event
	drain := native.DrainLines(MaxLineBytes, func(line []byte) error {
		events, err := parser.ParseLine(line)
		if err != nil {
			return err
		}
		for i := range events {
			e := events[i]
			if e.Kind == gen.EventKindSubagentDone {
				copy := e
				pendingDone = &copy
				continue // exit 상태를 반영한 뒤 terminal event로 마지막에 방출
			}
			if err := w.emit(e.Kind, e.Payload, e.Raw); err != nil {
				return err
			}
			if e.Kind == gen.EventKindSubagentReady {
				readyEmitted = true
			}
		}
		return nil
	})
	close(adapterDone)
	in.Close()
	var cmdErr error
	select {
	case cmdErr = <-commandErr:
	default:
	}

	doneEvent, finishErr := finishNative(drain, pendingDone, parser.StopRequested())
	terminalErr := finishErr // B-7: native handler/scan failure precedes command-side failure.
	if terminalErr == nil {
		terminalErr = cmdErr
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
		readyEmitted = true
	}
	return w.emit(doneEvent.Kind, doneEvent.Payload, doneEvent.Raw)
}

func emitFailureDone(w *wireWriter, cause error, stopRequested bool) error {
	status := gen.DonePayloadStatusError
	if stopRequested {
		status = gen.DonePayloadStatusStopped
	}
	payload, err := json.Marshal(gen.DonePayload{
		Status: status,
		Result: "(어댑터 오류: " + terminalCause(cause) + ")",
	})
	if err != nil {
		return err
	}
	return w.emit(gen.EventKindSubagentDone, payload, nil)
}

func terminalCause(err error) string {
	for _, item := range []struct {
		target error
		text   string
	}{
		{errMessageUnsupported, "message 미지원"},
		{errInboundContract, "수신 command 계약 위반"},
		{errNativeContract, "native stream 계약 위반"},
		{errNativeRead, "native stream 읽기 실패"},
		{errUnmatchedApproval, "미상관 approval_response"},
		{errDuplicateTask, "task 중복"},
		{errCommandRead, "command 읽기 실패"},
		{errCommandInputClosed, "실행 중 stdin 종료"},
	} {
		if errors.Is(err, item.target) {
			return item.text
		}
	}
	return "내부 실행 실패"
}

func readCommand(scanner *bufio.Scanner, vals *validate.Validators) (gen.Command, error) {
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return gen.Command{}, fmt.Errorf("claudecode: command 읽기: %w", err)
		}
		return gen.Command{}, fmt.Errorf("claudecode: task 전에 stdin 종료")
	}
	line := scanner.Bytes()
	if err := vals.ValidateCommand(line); err != nil {
		return gen.Command{}, fmt.Errorf("claudecode: 수신 command가 계약 위반: %w", err)
	}
	var cmd gen.Command
	if err := json.Unmarshal(line, &cmd); err != nil {
		return gen.Command{}, err
	}
	return cmd, nil
}

func monitorCommands(scanner *bufio.Scanner, vals *validate.Validators, parser *Parser, native *procgroup.Process, adapterDone <-chan struct{}, result chan<- error) {
	fail := func(err error) {
		// Publish the cause before kill: DrainLines may finish immediately after
		// the signal, and the adapter must not lose the command-side error.
		result <- err
		native.Kill()
	}
	for scanner.Scan() {
		line := scanner.Bytes()
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
			parser.NoteStop()
			result <- nil
			native.Kill()
			return
		case gen.CommandCmdMessage:
			fail(fmt.Errorf("claudecode: %w — Claude 단발 print 세션에서 지원되지 않음", errMessageUnsupported))
			return
		case gen.CommandCmdApprovalResponse:
			fail(fmt.Errorf("claudecode: %w", errUnmatchedApproval))
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
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		fail(fmt.Errorf("claudecode: %w: %v", errCommandRead, err))
		return
	}
	fail(fmt.Errorf("claudecode: %w", errCommandInputClosed))
}

func claudeCommand(cfg Config, task gen.TaskPayload) []string {
	return []string{
		cfg.ClaudeBin,
		"-p", task.Instruction,
		"--output-format", "stream-json",
		"--verbose",
		"--no-session-persistence",
		"--permission-mode", "manual",
		"--setting-sources", claudeSettingSources,
		"--settings", claudeApprovalHookSettings,
	}
}

// finishNative fixes the DrainResult precedence at the adapter boundary:
// handler contract error > scanner error > process exit mapping.
func finishNative(drain procgroup.DrainResult, pending *Event, stopRequested bool) (Event, error) {
	if drain.HandlerErr != nil {
		return Event{}, fmt.Errorf("claudecode: %w: %w", errNativeContract, drain.HandlerErr)
	}
	if drain.ScanErr != nil {
		return Event{}, fmt.Errorf("claudecode: %w: %w", errNativeRead, drain.ScanErr)
	}
	if pending != nil {
		out := *pending
		if drain.ExitErr != nil {
			var payload gen.DonePayload
			if err := json.Unmarshal(out.Payload, &payload); err != nil {
				return Event{}, err
			}
			if stopRequested {
				payload.Status = gen.DonePayloadStatusStopped
			} else {
				payload.Status = gen.DonePayloadStatusError
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				return Event{}, err
			}
			out.Payload = encoded
		}
		return out, nil
	}

	status := gen.DonePayloadStatusError
	if stopRequested {
		status = gen.DonePayloadStatusStopped
	}
	reason := "process_exit"
	if drain.ExitErr != nil {
		reason = "abnormal_exit"
	}
	payload, err := json.Marshal(gen.DonePayload{
		Status: status,
		Result: fmt.Sprintf("(결과 없음: subtype=missing_result, terminal_reason=%s)", reason),
	})
	if err != nil {
		return Event{}, err
	}
	return Event{Kind: gen.EventKindSubagentDone, Payload: payload}, nil
}
