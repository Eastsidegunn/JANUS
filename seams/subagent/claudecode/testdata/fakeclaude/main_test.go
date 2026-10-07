package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func hookArgs(command string) []string {
	settings := map[string]any{
		"hooks": map[string]any{"PreToolUse": []any{map[string]any{
			"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 1}},
		}}},
	}
	raw, _ := json.Marshal(settings)
	return []string{"--settings", string(raw)}
}

func TestNoShellGuardRejectsShellInPath(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal("test requires sh on the host")
	}
	dir := t.TempDir()
	if err := os.Symlink(sh, filepath.Join(dir, "sh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HX_CLAUDE_NO_SHELL", "1")
	if err := noShellGuard(); err == nil {
		t.Fatal("no-shell guard accepted sh in PATH")
	}
}

func TestExecuteHookNoShellEmulatesOnlyProductionWrapper(t *testing.T) {
	dir := t.TempDir()
	approve := filepath.Join(dir, "hxapprove")
	if err := os.WriteFile(approve, []byte("#!/bin/sh\nprintf '%s\\n' '{\"hookSpecificOutput\":{\"permissionDecision\":\"deny\"}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HX_CLAUDE_NO_SHELL", "1")
	result, err := executeHook(hookArgs("hxapprove || exit 2"), []byte(`{"hook_event_name":"PreToolUse"}`))
	if err != nil || result.Decision != "deny" {
		t.Fatalf("no-shell production wrapper result=%+v err=%v", result, err)
	}
	if _, err := executeHook(hookArgs("printf hacked"), []byte(`{"hook_event_name":"PreToolUse"}`)); err != nil {
		t.Fatalf("non-production command should be treated as non-blocking start failure: %v", err)
	}
}

func TestExecuteHookNoShellHxapproveExitTwoBlocks(t *testing.T) {
	dir := t.TempDir()
	approve := filepath.Join(dir, "hxapprove")
	if err := os.WriteFile(approve, []byte("#!/bin/sh\necho blocked >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HX_CLAUDE_NO_SHELL", "1")
	_, err := executeHook(hookArgs("hxapprove || exit 2"), []byte(`{"hook_event_name":"PreToolUse"}`))
	if err != errHookBlocked {
		t.Fatalf("hxapprove exit 2 err=%v, want %v", err, errHookBlocked)
	}
}

func TestExecuteHookNoShellHxapproveAnyFailureMapsToBlock(t *testing.T) {
	dir := t.TempDir()
	approve := filepath.Join(dir, "hxapprove")
	if err := os.WriteFile(approve, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HX_CLAUDE_NO_SHELL", "1")
	_, err := executeHook(hookArgs("hxapprove || exit 2"), []byte(`{"hook_event_name":"PreToolUse"}`))
	if err != errHookBlocked {
		t.Fatalf("no-shell helper exit 1 err=%v, want %v", err, errHookBlocked)
	}
}

func TestExecuteHookStreamsConfiguredOutputFileBeforeCommandExit(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "hook-output.json")
	t.Setenv("HX_CLAUDE_HOOK_OUT", outPath)
	args := hookArgs(`printf '{"hookSpecificOutput":{"permissionDecision":"deny"}}'; sleep 0.2`)
	result := make(chan hookResult, 1)
	errCh := make(chan error, 1)
	go func() {
		got, err := executeHook(args, []byte(`{"hook_event_name":"PreToolUse"}`))
		result <- got
		errCh <- err
	}()
	deadline := time.Now().Add(time.Second)
	streamed := false
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(outPath)
		if err == nil && len(data) > 0 {
			streamed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !streamed {
		t.Fatal("hook output file remained empty until command completion")
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.Decision != "deny" {
		t.Fatalf("decision=%+v", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal(mustReadFile(t, outPath), &decoded); err != nil {
		t.Fatal(err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
