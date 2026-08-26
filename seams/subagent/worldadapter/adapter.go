// Package worldadapter implements the host-side §5.2 adapter for an agent
// process owned by a world backend. The adapter never enters the container: it
// holds host-only process/approval endpoints and forwards only bounded stdio
// and correlated approval decisions across the sandbox boundary.
package worldadapter

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/processwire"
)

const (
	processNetworkEnv     = "HX_WORLD_PROCESS_NETWORK"
	processAddressEnv     = "HX_WORLD_PROCESS_ADDRESS"
	processLeaseEnv       = "HX_WORLD_PROCESS_LEASE_ID"
	processControlEnv     = "HX_WORLD_PROCESS_CONTROL_CAPABILITY"
	processOutputEnv      = "HX_WORLD_PROCESS_OUTPUT_CAPABILITY"
	approvalNetworkEnv    = "HX_WORLD_APPROVAL_NETWORK"
	approvalAddressEnv    = "HX_WORLD_APPROVAL_ADDRESS"
	approvalCapabilityEnv = "HX_WORLD_APPROVAL_CAPABILITY"
	worldSpanEnv          = "HX_WORLD_SPAN_ID"
	maxCommandBytes       = 4 << 20
)

var errAdapterStopped = errors.New("worldadapter: stopped")

type Config struct {
	Process  world.ProcessEndpoint
	Approval world.ApprovalEndpoint
	SpanID   string
}

func ConfigFromEnv() Config {
	return Config{
		Process: world.NewProcessEndpoint(
			os.Getenv(processNetworkEnv), os.Getenv(processAddressEnv), os.Getenv(processLeaseEnv),
			os.Getenv(processControlEnv), os.Getenv(processOutputEnv),
		),
		Approval: world.NewApprovalEndpoint(
			os.Getenv(approvalNetworkEnv), os.Getenv(approvalAddressEnv), os.Getenv(approvalCapabilityEnv),
		),
		SpanID: os.Getenv(worldSpanEnv),
	}
}

// Environment serializes a host-only descriptor into an adapter process
// environment. The surface passes this only to the host adapter process; the
// world backend never copies these values into the container environment.
func Environment(base []string, descriptor world.AgentDescriptor) []string {
	keys := []string{
		processNetworkEnv, processAddressEnv, processLeaseEnv, processControlEnv, processOutputEnv,
		approvalNetworkEnv, approvalAddressEnv, approvalCapabilityEnv, worldSpanEnv,
	}
	out := append([]string(nil), base...)
	for _, key := range keys {
		prefix := key + "="
		filtered := out[:0]
		for _, item := range out {
			if !strings.HasPrefix(item, prefix) {
				filtered = append(filtered, item)
			}
		}
		out = filtered
	}
	p, a := descriptor.ProcessEndpoint(), descriptor.ApprovalEndpoint()
	return append(out,
		processNetworkEnv+"="+p.Network(), processAddressEnv+"="+p.Address(), processLeaseEnv+"="+p.LeaseID(),
		processControlEnv+"="+p.ControlCapability(), processOutputEnv+"="+p.OutputCapability(),
		approvalNetworkEnv+"="+a.Network(), approvalAddressEnv+"="+a.Address(),
		approvalCapabilityEnv+"="+a.Capability(), worldSpanEnv+"="+descriptor.SpanID(),
	)
}

type wireWriter struct {
	mu   sync.Mutex
	out  io.Writer
	vals *validate.Validators
}

func (w *wireWriter) emit(kind gen.EventKind, payload json.RawMessage, raw []byte) error {
	line, err := json.Marshal(gen.Event{V: 1, Kind: kind, Payload: payload, Raw: base64.StdEncoding.EncodeToString(raw)})
	if err != nil {
		return err
	}
	if err := w.vals.ValidateEvent(line); err != nil {
		return fmt.Errorf("worldadapter: 발신 이벤트 계약 위반: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	line = append(line, '\n')
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

func Run(ctx context.Context, in io.ReadCloser, out, stderr io.Writer, cfg Config) error {
	vals, err := validate.New()
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxCommandBytes)
	if !scanner.Scan() {
		return fmt.Errorf("worldadapter: task 전에 stdin 종료: %w", scanner.Err())
	}
	taskLine := append([]byte(nil), scanner.Bytes()...)
	if err := vals.ValidateCommand(taskLine); err != nil {
		return fmt.Errorf("worldadapter: task 계약 위반: %w", err)
	}
	var first gen.Command
	if err := json.Unmarshal(taskLine, &first); err != nil || first.Cmd != gen.CommandCmdTask {
		return fmt.Errorf("worldadapter: 첫 command는 task여야 함")
	}

	process, err := connectProcess(ctx, cfg.Process, cfg.SpanID)
	if err != nil {
		return err
	}
	defer process.Close()
	w := &wireWriter{out: out, vals: vals}
	approvals, err := newApprovalClient(ctx, cfg.Approval, cfg.SpanID, w.emit)
	if err != nil {
		return err
	}
	defer approvals.Close()

	stdoutR, stdoutW := io.Pipe()
	type outputResult struct {
		err              error
		attachDiagnostic string
	}
	outputDone := make(chan outputResult, 1)
	go func() {
		var attachDiagnostic string
		err := process.DrainOutput(func(frame processwire.Frame) error {
			switch frame.Kind {
			case processwire.KindStdoutData:
				_, err := stdoutW.Write(frame.Payload)
				return err
			case processwire.KindStderrData:
				_, err := stderr.Write(frame.Payload)
				return err
			case processwire.KindStreamEnd:
				var end processwire.StreamEnd
				if err := processwire.Unmarshal(frame.Payload, &end); err != nil {
					return err
				}
				if end.AttachError != "" {
					// podman start --attach may return the container exit code.
					// It is transport diagnostics only; the independent podman wait
					// result below remains the sole exit authority.
					attachDiagnostic = end.AttachError
				}
				return nil
			default:
				return fmt.Errorf("worldadapter: output frame kind=%d", frame.Kind)
			}
		})
		_ = stdoutW.CloseWithError(err)
		outputDone <- outputResult{err: err, attachDiagnostic: attachDiagnostic}
	}()
	if err := process.Start(ctx, taskLine); err != nil {
		return err
	}

	commandErr := make(chan error, 1)
	stopRequested := make(chan struct{})
	stopped := make(chan struct{})
	stopScanDone := make(chan struct{})
	defer close(stopScanDone)
	go func() {
		select {
		case <-stopRequested:
			// Unblock the native scanner without closing the broker output socket.
			// The main goroutine still waits for the stop ACK before emitting its
			// synthetic stopped/done event and closing the process endpoint.
			_ = stdoutR.CloseWithError(errAdapterStopped)
		case <-stopScanDone:
		}
	}()
	go monitorCommands(ctx, scanner, vals, process, approvals, stopRequested, stopped, commandErr)

	native := bufio.NewScanner(stdoutR)
	native.Buffer(make([]byte, 0, 64*1024), maxCommandBytes)
	ready := false
	var done *gen.DonePayload
	for native.Scan() {
		line := append([]byte(nil), native.Bytes()...)
		if len(strings.TrimSpace(string(line))) == 0 {
			process.Stop(context.Background(), "blank output")
			return fmt.Errorf("worldadapter: container agent 공백 줄")
		}
		if done != nil {
			return fmt.Errorf("worldadapter: done 이후 container 출력")
		}
		if err := vals.ValidateEvent(line); err != nil {
			process.Stop(context.Background(), "event contract")
			return fmt.Errorf("worldadapter: container event 계약 위반: %w", err)
		}
		var event gen.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return err
		}
		if event.Kind == gen.EventKindSubagentReady {
			if ready {
				return fmt.Errorf("worldadapter: ready 중복")
			}
			ready = true
		} else if !ready {
			return fmt.Errorf("worldadapter: ready 전 %s", event.Kind)
		}
		if event.Kind == gen.EventKindSubagentToolCall {
			var intent gen.AgentToolCallPayload
			if err := json.Unmarshal(event.Payload, &intent); err != nil {
				return err
			}
			if err := approvals.RegisterIntent(intent); err != nil {
				return err
			}
		}
		if event.Kind == gen.EventKindSubagentDone {
			var terminal gen.DonePayload
			if err := json.Unmarshal(event.Payload, &terminal); err != nil {
				return err
			}
			done = &terminal
			continue
		}
		if err := w.emit(event.Kind, event.Payload, decodeRaw(event.Raw)); err != nil {
			return err
		}
	}
	stopWasRequested := false
	select {
	case <-stopRequested:
		stopWasRequested = true
	default:
	}
	if err := native.Err(); err != nil && !stopWasRequested {
		return fmt.Errorf("worldadapter: container stdout scan: %w", err)
	}
	wasStopped := false
	if stopWasRequested {
		select {
		case <-stopped:
			wasStopped = true
		case err := <-commandErr:
			if err != nil {
				return err
			}
			return fmt.Errorf("worldadapter: stop ACK 없이 command 종료")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	doneEmitted := false
	if wasStopped && !doneEmitted {
		if done == nil {
			done = &gen.DonePayload{Status: gen.DonePayloadStatusStopped, Result: "(중단됨: world process stop)"}
		} else {
			done.Status = gen.DonePayloadStatusStopped
		}
		payload, err := json.Marshal(done)
		if err != nil {
			return err
		}
		if err := w.emit(gen.EventKindSubagentDone, payload, nil); err != nil {
			return err
		}
		doneEmitted = true
		// The terminal event is now durable on the adapter stdout contract. The
		// output peer may close; the broker classifies this post-done close as
		// expected rather than as an ordinary stream failure.
		process.CloseOutput()
	}
	output := <-outputDone
	exit, exitErr := process.Wait(ctx)
	if approvalErr := approvals.Err(); approvalErr != nil {
		return approvalErr
	}
	select {
	case err := <-commandErr:
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	default:
	}
	if output.err != nil && !errors.Is(output.err, io.EOF) && !(wasStopped && process.OutputClosed()) {
		return output.err
	}
	if !ready {
		return fmt.Errorf("worldadapter: container가 ready 전에 종료")
	}
	if done == nil {
		status := gen.DonePayloadStatusError
		result := fmt.Sprintf("(결과 없음: container exit=%d reason=%s)", exit.Code, exit.Reason)
		if wasStopped {
			status = gen.DonePayloadStatusStopped
			result = "(중단됨: world process stop)"
		}
		done = &gen.DonePayload{Status: status, Result: result}
	} else if exitErr != nil || exit.Code != 0 {
		done.Status = gen.DonePayloadStatusError
	}
	_ = output.attachDiagnostic // retained only as lower-priority diagnostics.
	payload, err := json.Marshal(done)
	if err != nil {
		return err
	}
	if !doneEmitted {
		if err := w.emit(gen.EventKindSubagentDone, payload, nil); err != nil {
			return err
		}
	}
	_ = in.Close()
	return exitErr
}

func monitorCommands(ctx context.Context, scanner *bufio.Scanner, vals *validate.Validators, process *processClient, approvals *approvalClient, stopRequested, stopped chan<- struct{}, result chan<- error) {
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if err := vals.ValidateCommand(line); err != nil {
			result <- fmt.Errorf("worldadapter: 수신 command 계약 위반: %w", err)
			_ = process.Stop(context.Background(), "command contract")
			return
		}
		var cmd gen.Command
		if err := json.Unmarshal(line, &cmd); err != nil {
			result <- err
			return
		}
		switch cmd.Cmd {
		case gen.CommandCmdApprovalResponse:
			var response gen.ApprovalResponsePayload
			if err := json.Unmarshal(cmd.Payload, &response); err != nil {
				result <- err
				return
			}
			if err := approvals.Resolve(response); err != nil {
				result <- err
				return
			}
		case gen.CommandCmdStop:
			var stop gen.StopPayload
			if err := json.Unmarshal(cmd.Payload, &stop); err != nil {
				result <- err
				return
			}
			close(stopRequested)
			if err := process.Stop(ctx, string(stop.Reason)); err != nil {
				result <- err
				return
			}
			close(stopped)
			result <- nil
			return
		case gen.CommandCmdMessage:
			if err := process.SendLine(ctx, line); err != nil {
				result <- err
				return
			}
		case gen.CommandCmdTask:
			result <- fmt.Errorf("worldadapter: task 중복")
			return
		}
	}
	result <- scanner.Err()
}

func decodeRaw(value string) []byte {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil
	}
	return raw
}
