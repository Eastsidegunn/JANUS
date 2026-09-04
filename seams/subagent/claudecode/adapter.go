package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/subagent/internal/procgroup"
)

const (
	defaultClaudeExecutable    = "claude"
	claudeSettingSources       = "project,local"
	maxCommandBytes            = 4 << 20
	worldApprovalNetworkEnv    = "HX_WORLD_APPROVAL_NETWORK"
	worldApprovalAddressEnv    = "HX_WORLD_APPROVAL_ADDRESS"
	worldApprovalCapabilityEnv = "HX_WORLD_APPROVAL_CAPABILITY"
	worldApprovalSpanEnv       = "HX_WORLD_APPROVAL_SPAN_ID"
	worldAdapterSpanEnv        = "HX_WORLD_SPAN_ID"
	worldProcessNetworkEnv     = "HX_WORLD_PROCESS_NETWORK"
	worldProcessAddressEnv     = "HX_WORLD_PROCESS_ADDRESS"
	worldProcessLeaseEnv       = "HX_WORLD_PROCESS_LEASE_ID"
	worldProcessControlEnv     = "HX_WORLD_PROCESS_CONTROL_CAPABILITY"
	worldProcessOutputEnv      = "HX_WORLD_PROCESS_OUTPUT_CAPABILITY"

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
	errApprovalHandshake  = errors.New("승인 handshake 실패")
	errDuplicateApproval  = errors.New("중복 approval_response")
	errTokenExpired       = errors.New("token expired")
)

// Config contains host-controlled process settings. ClaudeBin is a single
// executable path, never a shell command.
type Config struct {
	ClaudeBin string
	Env       []string
	// ProcessEndpoint is non-zero only for local-podman. A zero endpoint keeps
	// the legacy host procgroup path used by world_backend:none.
	ProcessEndpoint      world.ProcessEndpoint
	ApprovalEndpoint     world.ApprovalEndpoint
	WorldSpanID          string
	TokenExpiresAtUnixMs int64
}

type approvalTransport interface {
	attach(<-chan struct{}, func())
	markReady()
	environment([]string) []string
	registerIntent(gen.AgentToolCallPayload) error
	resolve(gen.ApprovalResponsePayload) error
	denyAll(string, bool) int
	failure() error
	Close()
}

// ConfigFromEnv returns the production configuration. HX_CLAUDE_BIN exists so
// tests and operators can select an installed Claude binary without a shell.
func ConfigFromEnv() Config {
	bin := os.Getenv("HX_CLAUDE_BIN")
	if bin == "" {
		bin = defaultClaudeExecutable
	}
	env := os.Environ()
	endpoint := world.NewApprovalEndpoint(
		os.Getenv(worldApprovalNetworkEnv),
		os.Getenv(worldApprovalAddressEnv),
		os.Getenv(worldApprovalCapabilityEnv),
	)
	spanID := os.Getenv(worldApprovalSpanEnv)
	// worldadapter is the shared host-side endpoint serializer and publishes
	// the lease span as HX_WORLD_SPAN_ID. Keep the older approval-specific name
	// as a compatibility input for direct adapter tests and existing callers,
	// but use the shared name when it is the only one present.
	if spanID == "" {
		spanID = os.Getenv(worldAdapterSpanEnv)
	}
	processEndpoint := world.NewProcessEndpoint(
		os.Getenv(worldProcessNetworkEnv), os.Getenv(worldProcessAddressEnv), os.Getenv(worldProcessLeaseEnv),
		os.Getenv(worldProcessControlEnv), os.Getenv(worldProcessOutputEnv),
	)
	tokenExpiry := int64(0)
	if raw := os.Getenv("HX_CLAUDE_TOKEN_EXPIRES_AT_MS"); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			tokenExpiry = -1
		} else {
			tokenExpiry = parsed
		}
	}
	// Broker capability and the host's real approval socket are host-adapter
	// inputs, never native-agent environment. Direct mode adds its fresh local
	// approval socket back below; world mode relies on the container's relay.
	for _, key := range []string{
		worldApprovalNetworkEnv, worldApprovalAddressEnv, worldApprovalCapabilityEnv,
		worldApprovalSpanEnv, worldAdapterSpanEnv, approvalSocketEnv,
		worldProcessNetworkEnv, worldProcessAddressEnv, worldProcessLeaseEnv,
		worldProcessControlEnv, worldProcessOutputEnv,
		world.ClaudeOAuthTokenEnv,
	} {
		env = removeEnv(env, key)
	}
	// hxapprove is installed beside the adapter binary. Prepending exactly that
	// directory keeps the approved inline hook command constant in one place.
	if executable, err := os.Executable(); err == nil {
		dir := filepath.Dir(executable)
		env = replaceEnv(env, "PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return Config{ClaudeBin: bin, Env: env, ProcessEndpoint: processEndpoint, ApprovalEndpoint: endpoint, WorldSpanID: spanID, TokenExpiresAtUnixMs: tokenExpiry}
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return append(out, prefix+value)
}

func removeEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return out
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
	if cfg.TokenExpiresAtUnixMs < 0 {
		return fmt.Errorf("claudecode: token expiry metadata is invalid")
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

	w := &wireWriter{out: out, vals: vals}
	approvals, err := newApprovalTransport(w, cfg)
	if err != nil {
		return fmt.Errorf("claudecode: approval socket: %w", err)
	}
	defer approvals.Close()
	if cfg.ProcessEndpoint != (world.ProcessEndpoint{}) {
		taskLine, marshalErr := json.Marshal(cmd)
		if marshalErr != nil {
			return marshalErr
		}
		return runWorldProcess(ctx, in, stderr, cfg, vals, w, approvals, scanner, append(taskLine, '\n'))
	}

	// task.Workspace는 policy/T10이 준비한 pristine 작업공간이다. Claude의
	// cwd를 이 경로로 고정하고 user settings는 flag로 제외한다.
	native, err := procgroup.Start(ctx, procgroup.Options{
		Command: claudeCommand(cfg, task), Dir: task.Workspace,
		Env: approvals.environment(cfg.Env), Stderr: stderr,
	})
	if err != nil {
		return fmt.Errorf("claudecode: Claude 실행: %w", err)
	}
	// 지시는 argv(-p)로 전달하므로 Claude의 stdin은 쓰지 않는다. 열어두면
	// Claude가 stdin 입력을 3초간 기다린 뒤 경고와 함께 진행한다(smoke 실측).
	native.CloseStdin()
	approvals.attach(native.Done(), native.Kill)

	parser := NewParser()
	readyEmitted := false
	adapterDone := make(chan struct{})
	commandErr := make(chan error, 1)
	go monitorCommands(scanner, vals, parser, native, approvals, adapterDone, commandErr)

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
			if e.Kind == gen.EventKindSubagentToolCall {
				var intent gen.AgentToolCallPayload
				if err := json.Unmarshal(e.Payload, &intent); err != nil {
					return fmt.Errorf("claudecode: tool intent decode: %w", err)
				}
				if err := approvals.registerIntent(intent); err != nil {
					return fmt.Errorf("claudecode: world intent 등록: %w", err)
				}
			}
			if err := w.emit(e.Kind, e.Payload, e.Raw); err != nil {
				return err
			}
			if e.Kind == gen.EventKindSubagentReady {
				readyEmitted = true
				approvals.markReady()
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
	if terminalErr == nil {
		if approvalErr := approvals.failure(); approvalErr != nil {
			terminalErr = fmt.Errorf("claudecode: %w: %v", errApprovalHandshake, approvalErr)
		}
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
		approvals.markReady()
	}
	return w.emit(doneEvent.Kind, doneEvent.Payload, doneEvent.Raw)
}

func emitFailureDone(w *wireWriter, cause error, stopRequested bool) error {
	status := gen.DonePayloadStatusError
	if stopRequested {
		status = gen.DonePayloadStatusStopped
	}
	result := "(어댑터 오류: " + terminalCause(cause) + ")"
	if errors.Is(cause, errTokenExpired) {
		result = "token expired"
	}
	payload, err := json.Marshal(gen.DonePayload{
		Status: status,
		Result: result,
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
		{errApprovalHandshake, "승인 handshake 실패"},
		{errDuplicateApproval, "중복 approval_response"},
		{errAuthenticationFailed, "Claude 인증 실패"},
		{errTokenExpired, "token expired"},
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

func monitorCommands(scanner *bufio.Scanner, vals *validate.Validators, parser *Parser, native *procgroup.Process, approvals approvalTransport, adapterDone <-chan struct{}, result chan<- error) {
	fail := func(err error) {
		// Publish the cause before kill: DrainLines may finish immediately after
		// the signal, and the adapter must not lose the command-side error.
		approvals.denyAll("어댑터 command 오류", true)
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
			approvals.denyAll("중단 요청", true)
			parser.NoteStop()
			result <- nil
			native.Kill()
			return
		case gen.CommandCmdMessage:
			fail(fmt.Errorf("claudecode: %w — Claude 단발 print 세션에서 지원되지 않음", errMessageUnsupported))
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
