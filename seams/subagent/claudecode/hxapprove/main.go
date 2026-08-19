// hxapprove is the synchronous Claude PreToolUse hook helper. It transports
// the exact hook stdin bytes to claudecode over a Unix socket, waits for the
// parent decision, and prints Claude's hookSpecificOutput response.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
)

const (
	approvalSocketEnv = "HX_APPROVAL_SOCKET"
	maxHookBytes      = 4 << 20
)

type socketRequest struct {
	Raw []byte `json:"raw"`
}

type socketDecision struct {
	Decision string  `json:"decision"`
	Reason   *string `json:"reason,omitempty"`
}

type socketAck struct {
	Delivered bool `json:"delivered"`
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
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hxapprove:", err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer) error {
	path := os.Getenv(approvalSocketEnv)
	if path == "" {
		return fmt.Errorf("%s 없음", approvalSocketEnv)
	}
	raw, err := io.ReadAll(io.LimitReader(in, maxHookBytes+1))
	if err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > maxHookBytes {
		return fmt.Errorf("hook 입력 크기 위반: %d", len(raw))
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()
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
	if err := json.NewEncoder(out).Encode(response); err != nil {
		return err
	}
	return json.NewEncoder(conn).Encode(socketAck{Delivered: true})
}
