package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

func TestRunAllowAndDenyEmitDecisionJSON(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision string
		reason   *string
	}{
		{name: "allow", decision: "allow"},
		{name: "deny", decision: "deny", reason: stringPtr("operator denied")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "test-approval.sock"
			oldDial := dialApproval
			dialApproval = func(context.Context, string) (net.Conn, error) {
				client, server := net.Pipe()
				go func() {
					conn := server
					defer conn.Close()
					var req socketRequest
					if json.NewDecoder(conn).Decode(&req) != nil || len(req.Raw) == 0 {
						return
					}
					_ = json.NewEncoder(conn).Encode(socketDecision{Decision: tc.decision, Reason: tc.reason})
					var ack socketAck
					_ = json.NewDecoder(conn).Decode(&ack)
				}()
				return client, nil
			}
			t.Cleanup(func() { dialApproval = oldDial })
			old := os.Getenv(approvalSocketEnv)
			if err := os.Setenv(approvalSocketEnv, path); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Setenv(approvalSocketEnv, old) })
			var out bytes.Buffer
			if err := run(strings.NewReader(`{"hook_event_name":"PreToolUse"}`), &out); err != nil {
				t.Fatal(err)
			}
			var got hookOutput
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.HookSpecificOutput.PermissionDecision != tc.decision {
				t.Fatalf("decision=%q, want %q", got.HookSpecificOutput.PermissionDecision, tc.decision)
			}
		})
	}
}

func TestRunErrorsAreExit2Inputs(t *testing.T) {
	old := os.Getenv(approvalSocketEnv)
	defer os.Setenv(approvalSocketEnv, old)
	_ = os.Unsetenv(approvalSocketEnv)
	var out bytes.Buffer
	if err := run(strings.NewReader(`{}`), &out); err == nil {
		t.Fatal("missing socket unexpectedly succeeded")
	}
	if out.Len() != 0 {
		t.Fatalf("stdout=%q, want empty", out.String())
	}
}

func TestRunContextDeadlineBlocksWithoutOutput(t *testing.T) {
	path := "test-approval.sock"
	oldDial := dialApproval
	dialApproval = func(context.Context, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() { defer server.Close(); time.Sleep(100 * time.Millisecond) }()
		return client, nil
	}
	defer func() { dialApproval = oldDial }()
	old := os.Getenv(approvalSocketEnv)
	if err := os.Setenv(approvalSocketEnv, path); err != nil {
		t.Fatal(err)
	}
	defer os.Setenv(approvalSocketEnv, old)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := runContext(ctx, strings.NewReader(`{}`), &out); err == nil {
		t.Fatal("deadline unexpectedly succeeded")
	}
	if out.Len() != 0 {
		t.Fatalf("stdout=%q, want empty", out.String())
	}
}

func TestDenyWritesBeforeAckButAckFailureStillBlocks(t *testing.T) {
	oldDial := dialApproval
	dialApproval = func(context.Context, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var req socketRequest
			if json.NewDecoder(server).Decode(&req) != nil {
				return
			}
			_ = json.NewEncoder(server).Encode(socketDecision{Decision: "deny", Reason: stringPtr("operator denied")})
			// Closing before reading the ACK simulates a relay that disappears
			// after delivering a deny decision.
		}()
		return client, nil
	}
	defer func() { dialApproval = oldDial }()
	t.Setenv(approvalSocketEnv, "test-approval.sock")
	var out bytes.Buffer
	if err := runContext(context.Background(), strings.NewReader(`{"hook_event_name":"PreToolUse"}`), &out); err == nil {
		t.Fatal("deny ACK failure unexpectedly succeeded")
	}
	if out.Len() == 0 {
		t.Fatal("deny decision was not written before ACK failure")
	}
}

func TestAllowAckFailureDoesNotWriteDecision(t *testing.T) {
	oldDial := dialApproval
	dialApproval = func(context.Context, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var req socketRequest
			if json.NewDecoder(server).Decode(&req) != nil {
				return
			}
			_ = json.NewEncoder(server).Encode(socketDecision{Decision: "allow"})
		}()
		return client, nil
	}
	defer func() { dialApproval = oldDial }()
	t.Setenv(approvalSocketEnv, "test-approval.sock")
	var out bytes.Buffer
	if err := runContext(context.Background(), strings.NewReader(`{"hook_event_name":"PreToolUse"}`), &out); err == nil {
		t.Fatal("allow ACK failure unexpectedly succeeded")
	}
	if out.Len() != 0 {
		t.Fatalf("unacknowledged allow leaked stdout: %q", out.String())
	}
}

func TestAckFailureOrderingProperty(t *testing.T) {
	oldDial := dialApproval
	defer func() { dialApproval = oldDial }()
	t.Setenv(approvalSocketEnv, "test-approval.sock")
	property := func(allow bool) bool {
		dialApproval = func(context.Context, string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				var req socketRequest
				if json.NewDecoder(server).Decode(&req) != nil {
					return
				}
				decision := "deny"
				response := socketDecision{Decision: decision, Reason: stringPtr("operator denied")}
				if allow {
					response = socketDecision{Decision: "allow"}
				}
				_ = json.NewEncoder(server).Encode(response)
			}()
			return client, nil
		}
		var out bytes.Buffer
		err := runContext(context.Background(), strings.NewReader(`{"hook_event_name":"PreToolUse"}`), &out)
		if err == nil {
			return false
		}
		if allow {
			return out.Len() == 0
		}
		return out.Len() > 0
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 32}); err != nil {
		t.Fatal(err)
	}
}

func TestBinaryHookErrorExitsTwoAndWritesNoDecision(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "hxapprove")
	build := exec.Command("go", "build", "-o", bin, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hxapprove: %v\n%s", err, output)
	}
	cmd := exec.Command(bin)
	cmd.Env = envWithoutApprovalSocket()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("missing relay unexpectedly succeeded")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("exit=%v, want 2", err)
	}
	if stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func envWithoutApprovalSocket() []string {
	prefix := approvalSocketEnv + "="
	env := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, prefix) {
			env = append(env, item)
		}
	}
	return env
}

func stringPtr(s string) *string { return &s }
