// hxapprove is the synchronous Claude PreToolUse hook helper. It transports
// the exact hook stdin bytes to claudecode over a Unix socket, waits for the
// parent decision, and prints Claude's hookSpecificOutput response.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/Eastsidegunn/JANUS/core/buildinfo"
	"github.com/Eastsidegunn/JANUS/core/world/approvalrelaywire"
	"github.com/Eastsidegunn/JANUS/core/world/approvaltiming"
)

const (
	approvalSocketEnv = "HX_APPROVAL_SOCKET"
	maxHookBytes      = approvalrelaywire.MaxLineBytes
)

type socketRequest = approvalrelaywire.Request
type socketDecision = approvalrelaywire.Decision
type socketAck = approvalrelaywire.Ack

var dialApproval = func(ctx context.Context, path string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", path)
}

type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string  `json:"hookEventName"`
	PermissionDecision       string  `json:"permissionDecision"`
	PermissionDecisionReason *string `json:"permissionDecisionReason,omitempty"`
}

func main() {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintln(os.Stderr, "hxapprove: approval hook blocked: internal error")
			os.Exit(2)
		}
	}()
	if len(os.Args) >= 2 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.Line("hxapprove"))
		return
	}
	if err := run(os.Stdin, os.Stdout); err != nil {
		// Claude Code blocks a command hook only for exit 2. Keep this
		// diagnostic deliberately generic: stderr is shown to the model.
		fmt.Fprintln(os.Stderr, "hxapprove: approval hook blocked:", safeReason(err))
		os.Exit(2)
	}
}

func safeReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, approvalSocketEnv):
		return approvalSocketEnv + " 없음"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded):
		return "approval deadline exceeded"
	case strings.Contains(message, "hook 입력 크기"):
		return "invalid hook input"
	case strings.Contains(message, "미지 decision") || strings.Contains(message, "deny reason"):
		return "invalid approval decision"
	default:
		return "relay unavailable"
	}
}

func run(in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), approvaltiming.HxapproveDeadline)
	defer cancel()
	return runContext(ctx, in, out)
}

func runContext(ctx context.Context, in io.Reader, out io.Writer) error {
	path := os.Getenv(approvalSocketEnv)
	if path == "" {
		return fmt.Errorf("%s 없음", approvalSocketEnv)
	}
	raw, err := readInput(ctx, in)
	if err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > maxHookBytes {
		return fmt.Errorf("hook 입력 크기 위반: %d", len(raw))
	}
	conn, err := dialApproval(ctx, path)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	if err := json.NewEncoder(conn).Encode(socketRequest{Raw: raw}); err != nil {
		return err
	}
	var decision socketDecision
	if err := json.NewDecoder(conn).Decode(&decision); err != nil {
		return err
	}
	if decision.Decision != "allow" && decision.Decision != "deny" {
		return fmt.Errorf("미지 decision %q", decision.Decision)
	}
	if decision.Decision == "deny" && (decision.Reason == nil || *decision.Reason == "") {
		return fmt.Errorf("deny reason 없음")
	}
	response := hookOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName: "PreToolUse", PermissionDecision: decision.Decision,
		PermissionDecisionReason: decision.Reason,
	}}
	// Prepare the complete stdout response before acknowledging it. Denials
	// are written first so a stop path can observe the fail-closed decision
	// before the native process is terminated; an ACK failure still returns an
	// error (exit 2). Allows use the opposite order: an unacknowledged allow is
	// never emitted to Claude.
	var responseBytes []byte
	responseBytes, err = json.Marshal(response)
	if err != nil {
		return err
	}
	responseBytes = append(responseBytes, '\n')
	if decision.Decision == "deny" {
		if _, err := out.Write(responseBytes); err != nil {
			return err
		}
		if err := json.NewEncoder(conn).Encode(socketAck{Delivered: true}); err != nil {
			return err
		}
		return nil
	}
	if err := json.NewEncoder(conn).Encode(socketAck{Delivered: true}); err != nil {
		return err
	}
	_, err = out.Write(responseBytes)
	return err
}

func readInput(ctx context.Context, in io.Reader) ([]byte, error) {
	result := make(chan struct {
		data []byte
		err  error
	}, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(in, maxHookBytes+1))
		result <- struct {
			data []byte
			err  error
		}{data: data, err: err}
	}()
	select {
	case got := <-result:
		return got.data, got.err
	case <-ctx.Done():
		return nil, fmt.Errorf("hook deadline exceeded: %w", ctx.Err())
	}
}
