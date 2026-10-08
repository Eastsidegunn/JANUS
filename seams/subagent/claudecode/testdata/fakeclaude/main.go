// fakeclaude replays a real T8 Claude fixture for adapter process tests. It
// never invents stream-json lines; optional modes only omit the native result,
// replay the fixture's first line twice, hold the process open, choose an
// exit code, or (HX_CLAUDE_HOOK_ORDER) anchor the hook on the tool_use line.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Eastsidegunn/JANUS/core/world/approvaltiming"
)

var errHookBlocked = errors.New("hook exit 2: tool call blocked")

type hookSettings struct {
	Hooks struct {
		PreToolUse []struct {
			Hooks []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"PreToolUse"`
	} `json:"hooks"`
}

type hookResult struct {
	Decision string
	Reason   string
}

func main() {
	// T27(b) fail-closed guards run before any mode branch so the legacy and
	// multiturn paths stay byte-identical for their own inputs.
	if err := noShellGuard(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude:", err)
		os.Exit(2)
	}
	if os.Getenv("HX_CLAUDE_HOOK_ORDER") != "" && os.Getenv("HX_CLAUDE_MULTITURN_FIXTURES") != "" {
		fmt.Fprintln(os.Stderr, "fakeclaude: HX_CLAUDE_HOOK_ORDER와 HX_CLAUDE_MULTITURN_FIXTURES는 함께 쓸 수 없음")
		os.Exit(2)
	}
	if os.Getenv("HX_CLAUDE_HOOK_ORDER") == "" {
		for _, key := range []string{"HX_CLAUDE_HOLD_UNTIL_SIGUSR1", "HX_CLAUDE_HOOK_EXPECT_DECISION", "HX_CLAUDE_HOOK_FAILURE"} {
			if os.Getenv(key) != "" {
				fmt.Fprintf(os.Stderr, "fakeclaude: %s는 HX_CLAUDE_HOOK_ORDER 없이 쓸 수 없음\n", key)
				os.Exit(2)
			}
		}
	}
	if turns := os.Getenv("HX_CLAUDE_MULTITURN_FIXTURES"); turns != "" {
		multiturn(strings.Split(turns, ","))
		return
	}
	fixture := os.Getenv("HX_CLAUDE_FIXTURE")
	if fixture == "" {
		fmt.Fprintln(os.Stderr, "fakeclaude: HX_CLAUDE_FIXTURE 없음")
		os.Exit(2)
	}
	if argsPath := os.Getenv("HX_CLAUDE_ARGS_OUT"); argsPath != "" {
		b, _ := json.Marshal(os.Args[1:])
		if err := os.WriteFile(argsPath, b, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "fakeclaude: args 기록:", err)
			os.Exit(2)
		}
	}
	if expected := os.Getenv("HX_CLAUDE_EXPECT_ARGS"); expected != "" {
		var want []string
		if err := json.Unmarshal([]byte(expected), &want); err != nil || !reflect.DeepEqual(os.Args[1:], want) {
			fmt.Fprintln(os.Stderr, "fakeclaude: argv mismatch", os.Args[1:])
			os.Exit(2)
		}
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil || len(stdin) != 0 {
		fmt.Fprintln(os.Stderr, "fakeclaude: expected EOF with zero stdin bytes")
		os.Exit(2)
	}
	if order := os.Getenv("HX_CLAUDE_HOOK_ORDER"); order != "" {
		ordered(fixture, order)
		return
	}
	f, err := os.Open(fixture)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude:", err)
		os.Exit(2)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	first := true
	hookRan := false
	var hookDone chan error
	runHook := func() error {
		raw := []byte(os.Getenv("HX_CLAUDE_HOOK_INPUT"))
		if len(raw) == 0 {
			raw = []byte(`{"hook_event_name":"PreToolUse","tool_use_id":"call-1","tool_name":"Bash","tool_input":{"command":"true"}}`)
		}
		_, err := executeHook(os.Args[1:], raw)
		return err
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if first && os.Getenv("HX_CLAUDE_SKIP_FIRST") == "1" {
			first = false
			continue
		}
		if os.Getenv("HX_CLAUDE_DROP_RESULT") == "1" {
			var header struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &header) == nil && header.Type == "result" {
				continue
			}
		}
		if _, err := os.Stdout.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			os.Exit(2)
		}
		var header struct {
			Type string `json:"type"`
		}
		if path := os.Getenv("HX_CLAUDE_RESULT_READY"); path != "" && json.Unmarshal(line, &header) == nil && header.Type == "result" {
			if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, "fakeclaude: result marker:", err)
				os.Exit(2)
			}
		}
		if first && os.Getenv("HX_CLAUDE_DUPLICATE_FIRST") == "1" {
			if _, err := os.Stdout.Write(append(append([]byte(nil), line...), '\n')); err != nil {
				os.Exit(2)
			}
		}
		first = false
		if !hookRan && os.Getenv("HX_CLAUDE_RUN_HOOK") == "1" {
			hookRan = true
			if os.Getenv("HX_CLAUDE_HOOK_ASYNC") == "1" {
				hookDone = make(chan error, 1)
				go func() { hookDone <- runHook() }()
			} else if err := runHook(); err != nil {
				fmt.Fprintln(os.Stderr, "fakeclaude:", err)
				os.Exit(3)
			}
			if path := os.Getenv("HX_CLAUDE_HOLD_AFTER_HOOK"); path != "" {
				for {
					if _, err := os.Stat(path); err == nil {
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
		}
	}
	f.Close()
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude:", err)
		os.Exit(2)
	}
	if hookDone != nil {
		if err := <-hookDone; err != nil {
			fmt.Fprintln(os.Stderr, "fakeclaude:", err)
			os.Exit(3)
		}
	}
	if os.Getenv("HX_CLAUDE_HOLD") == "1" {
		time.Sleep(30 * time.Second)
	}
	if text := os.Getenv("HX_CLAUDE_EXIT_CODE"); text != "" {
		code, err := strconv.Atoi(text)
		if err != nil {
			os.Exit(2)
		}
		os.Exit(code)
	}
}

// multiturn replays one recorded fixture per stream-json user turn read from
// stdin (SCP-T25-001). It never invents native lines: turns after the first
// only omit the fixture's system/init line, because a live session announces
// init once. Each received user text is appended to HX_CLAUDE_TURNS_OUT. After
// the last fixture it waits for stdin EOF (exit 0) or termination.
func multiturn(fixtures []string) {
	if expected := os.Getenv("HX_CLAUDE_EXPECT_ARGS"); expected != "" {
		var want []string
		if err := json.Unmarshal([]byte(expected), &want); err != nil || !reflect.DeepEqual(os.Args[1:], want) {
			fmt.Fprintln(os.Stderr, "fakeclaude: argv mismatch", os.Args[1:])
			os.Exit(2)
		}
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for turn := 0; in.Scan(); turn++ {
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(in.Bytes(), &msg); err != nil || msg.Type != "user" || msg.Message.Role != "user" ||
			len(msg.Message.Content) != 1 || msg.Message.Content[0].Type != "text" {
			fmt.Fprintf(os.Stderr, "fakeclaude: stream-json user 메시지 아님: %q\n", in.Bytes())
			os.Exit(2)
		}
		if turn >= len(fixtures) {
			fmt.Fprintln(os.Stderr, "fakeclaude: 준비된 턴 fixture 초과")
			os.Exit(2)
		}
		if path := os.Getenv("HX_CLAUDE_TURNS_OUT"); path != "" {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				b, _ := json.Marshal(msg.Message.Content[0].Text)
				_, err = f.Write(append(b, '\n'))
				_ = f.Close()
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "fakeclaude: turns 기록:", err)
				os.Exit(2)
			}
		}
		f, err := os.Open(fixtures[turn])
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakeclaude:", err)
			os.Exit(2)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			var header struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
			}
			if turn > 0 && json.Unmarshal(line, &header) == nil && header.Type == "system" && header.Subtype == "init" {
				continue
			}
			if _, err := os.Stdout.Write(append(append([]byte(nil), line...), '\n')); err != nil {
				os.Exit(2)
			}
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "fakeclaude:", err)
			os.Exit(2)
		}
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude: stdin:", err)
		os.Exit(2)
	}
}

// ordered replays the fixture with a PreToolUse hook anchored on every native
// assistant tool_use line (T27(b) container gate; T33 traceability requires
// every replayed tool_use, rather than only the first, to exercise the hook). It still never
// invents stream-json lines; it only fixes where the hook runs relative to
// that line:
//
//   - native-first: write the tool_use line, then run hxapprove to completion.
//   - hook-first:   run hxapprove to completion (blocking, as a real
//     PreToolUse hook would), then write the tool_use line.
//
// Lines after the anchor are written only after the hook returned, mirroring
// a tool call that cannot execute before its permission decision. When
// HX_CLAUDE_HOOK_INPUT is empty the hook input is the anchor's own
// (tool_use_id, name, input), so the world broker can correlate it.
// HX_CLAUDE_HOOK_EXPECT_DECISION makes a different decision fatal (exit 3),
// and HX_CLAUDE_HOLD_UNTIL_SIGUSR1=1 keeps the process alive after the last
// line until SIGUSR1 so a host test can inspect the live container.
func ordered(fixture, order string) {
	if order != "native-first" && order != "hook-first" {
		fmt.Fprintf(os.Stderr, "fakeclaude: 미지 HX_CLAUDE_HOOK_ORDER %q\n", order)
		os.Exit(2)
	}
	for _, key := range []string{
		"HX_CLAUDE_RUN_HOOK", "HX_CLAUDE_HOOK_ASYNC", "HX_CLAUDE_HOLD_AFTER_HOOK", "HX_CLAUDE_HOOK_OUT",
		"HX_CLAUDE_SKIP_FIRST", "HX_CLAUDE_DROP_RESULT", "HX_CLAUDE_DUPLICATE_FIRST",
		"HX_CLAUDE_RESULT_READY", "HX_CLAUDE_HOLD", "HX_CLAUDE_EXIT_CODE",
	} {
		if os.Getenv(key) != "" {
			fmt.Fprintf(os.Stderr, "fakeclaude: HX_CLAUDE_HOOK_ORDER와 %s는 함께 쓸 수 없음\n", key)
			os.Exit(2)
		}
	}
	var release chan os.Signal
	if os.Getenv("HX_CLAUDE_HOLD_UNTIL_SIGUSR1") == "1" {
		// Registered before any output: PID 1 in a container ignores signals
		// without a handler, so a late registration could lose the release.
		release = make(chan os.Signal, 1)
		signal.Notify(release, syscall.SIGUSR1)
	}
	f, err := os.Open(fixture)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude:", err)
		os.Exit(2)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	write := func(line []byte) {
		if _, err := os.Stdout.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			os.Exit(2)
		}
	}
	anchored := false
	for scanner.Scan() {
		line := scanner.Bytes()
		hookInput, isToolUse := toolUseHookInput(line)
		if !isToolUse {
			write(line)
			continue
		}
		anchored = true
		if raw := os.Getenv("HX_CLAUDE_HOOK_INPUT"); raw != "" {
			hookInput = []byte(raw)
		}
		if order == "native-first" {
			write(line)
		}
		switch failure := os.Getenv("HX_CLAUDE_HOOK_FAILURE"); failure {
		case "":
			if err := runOrderedHook(hookInput); err != nil {
				fmt.Fprintln(os.Stderr, "fakeclaude:", err)
				os.Exit(3)
			}
		case "exit-1":
			// Claude treats a command-hook exit other than 0 or 2 as
			// non-blocking. This mode models that runner outcome without
			// weakening the production `hxapprove || exit 2` wrapper.
			fmt.Fprintln(os.Stderr, "fakeclaude: forced hook exit=1 (non-blocking)")
		case "not-run":
			// Models hook process start failure (/bin/sh or hook executable
			// absent), which Claude also treats as non-blocking.
			fmt.Fprintln(os.Stderr, "fakeclaude: forced hook not-run (non-blocking)")
		default:
			fmt.Fprintf(os.Stderr, "fakeclaude: unknown HX_CLAUDE_HOOK_FAILURE %q\n", failure)
			os.Exit(2)
		}
		if order == "hook-first" {
			write(line)
		}
	}
	f.Close()
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude:", err)
		os.Exit(2)
	}
	if !anchored {
		fmt.Fprintln(os.Stderr, "fakeclaude: fixture에 assistant tool_use 줄이 없음")
		os.Exit(2)
	}
	if release != nil {
		select {
		case <-release:
		case <-time.After(2 * time.Minute):
			fmt.Fprintln(os.Stderr, "fakeclaude: SIGUSR1 release 대기 timeout")
			os.Exit(3)
		}
	}
}

// toolUseHookInput reports whether line is a native assistant message carrying
// a tool_use block and, if so, returns the PreToolUse hook input for the first
// such block.
func toolUseHookInput(line []byte) ([]byte, bool) {
	var native struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &native) != nil || native.Type != "assistant" {
		return nil, false
	}
	for _, block := range native.Message.Content {
		if block.Type != "tool_use" {
			continue
		}
		raw, err := json.Marshal(struct {
			HookEventName string          `json:"hook_event_name"`
			ToolUseID     string          `json:"tool_use_id"`
			ToolName      string          `json:"tool_name"`
			ToolInput     json.RawMessage `json:"tool_input"`
		}{"PreToolUse", block.ID, block.Name, block.Input})
		if err != nil {
			return nil, false
		}
		return raw, true
	}
	return nil, false
}

func configuredHook(argv []string) (string, time.Duration, error) {
	command := "hxapprove || exit 2"
	timeout := approvaltiming.HookTimeout
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] != "--settings" {
			continue
		}
		var settings hookSettings
		if err := json.Unmarshal([]byte(argv[i+1]), &settings); err != nil {
			return "", 0, fmt.Errorf("hook settings JSON: %w", err)
		}
		entries := settings.Hooks.PreToolUse
		if len(entries) == 0 || len(entries[0].Hooks) == 0 {
			return "", 0, fmt.Errorf("PreToolUse hook missing")
		}
		command = entries[0].Hooks[0].Command
		if seconds := entries[0].Hooks[0].Timeout; seconds > 0 {
			timeout = time.Duration(seconds) * time.Second
		}
		return command, timeout, nil
	}
	return command, timeout, nil
}

// executeHook mirrors Claude Code's command-hook semantics: command is run by
// sh -c; exit 0 applies its JSON output, exit 2 blocks, and other non-zero
// exits or timeout errors are non-blocking. The latter behavior is deliberate
// so the fake catches the production wrapper's `|| exit 2` fail-closed guard.
func executeHook(argv []string, input []byte) (hookResult, error) {
	if err := noShellGuard(); err != nil {
		return hookResult{}, err
	}
	command, timeout, err := configuredHook(argv)
	if err != nil {
		return hookResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	var hookFile *os.File
	if path := os.Getenv("HX_CLAUDE_HOOK_OUT"); path != "" {
		var err error
		hookFile, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return hookResult{}, fmt.Errorf("hook output: %w", err)
		}
		defer hookFile.Close()
	}
	stdoutWriter := io.Writer(&stdout)
	if hookFile != nil {
		stdoutWriter = hookFile
	}
	var runErr error
	noShellProduction := false
	if os.Getenv("HX_CLAUDE_NO_SHELL") == "1" {
		// FROM-scratch images have no shell. Only the exact production wrapper
		// is emulated; arbitrary commands represent hook-start failure, which
		// Claude treats as non-blocking.
		// 이 모드에서 훅 timeout은 차단으로 매핑되지만 실제 Claude는 훅 timeout을 비차단으로 처리한다 — t27은 600s timeout이라 도달하지 않음
		if command != "hxapprove || exit 2" {
			return hookResult{}, nil
		}
		noShellProduction = true
		direct := exec.CommandContext(ctx, "hxapprove")
		direct.Env, direct.Stdin = os.Environ(), bytes.NewReader(input)
		direct.Stdout, direct.Stderr = stdoutWriter, &stderr
		runErr = direct.Run()
	} else {
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Env = os.Environ()
		cmd.Stdin = bytes.NewReader(input)
		cmd.Stdout, cmd.Stderr = stdoutWriter, &stderr
		runErr = cmd.Run()
	}
	if hookFile != nil {
		if err := hookFile.Close(); err != nil {
			return hookResult{}, fmt.Errorf("hook output close: %w", err)
		}
		hookFile = nil
		path := os.Getenv("HX_CLAUDE_HOOK_OUT")
		data, err := os.ReadFile(path)
		if err != nil {
			return hookResult{}, fmt.Errorf("hook output: %w", err)
		}
		stdout.Reset()
		_, _ = stdout.Write(data)
	}
	// Check exit 2 before ctx.Err: CommandContext may report cancellation
	// alongside the helper's explicit fail-closed result.
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 2 {
			return hookResult{}, errHookBlocked
		}
		// In the no-shell image, `hxapprove || exit 2` is the production
		// fail-closed wrapper: every helper non-zero or exec failure maps to
		// exit 2.
		if noShellProduction {
			return hookResult{}, errHookBlocked
		}
	}
	if ctx.Err() != nil {
		// A shell hook timeout is non-blocking under Claude's semantics.
		return hookResult{}, nil // Claude treats hook timeout as non-blocking.
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 2 {
			return hookResult{}, errHookBlocked
		}
		return hookResult{}, nil
	}
	var output struct {
		HookSpecificOutput struct {
			PermissionDecision       string  `json:"permissionDecision"`
			PermissionDecisionReason *string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return hookResult{}, fmt.Errorf("hook output JSON: %w", err)
	}
	result := hookResult{Decision: output.HookSpecificOutput.PermissionDecision}
	if output.HookSpecificOutput.PermissionDecisionReason != nil {
		result.Reason = *output.HookSpecificOutput.PermissionDecisionReason
	}
	return result, nil
}

func noShellGuard() error {
	if os.Getenv("HX_CLAUDE_NO_SHELL") != "1" {
		return nil
	}
	if _, err := exec.LookPath("sh"); err == nil {
		return errors.New("HX_CLAUDE_NO_SHELL=1 requires sh to be absent from PATH")
	}
	return nil
}

func runOrderedHook(input []byte) error {
	result, err := executeHook(os.Args[1:], input)
	if err != nil {
		return err
	}
	// Diagnostic trace on stderr only (never stdout, which is the native
	// stream): the container gate reads it from adapter stderr / podman logs
	// to tell which decision the hook actually received.
	fmt.Fprintf(os.Stderr, "fakeclaude: hook exit=0 decision=%q reason=%q parse_err=<nil>\n",
		result.Decision, result.Reason)
	want := os.Getenv("HX_CLAUDE_HOOK_EXPECT_DECISION")
	if want == "" {
		return nil
	}
	if got := result.Decision; got != want {
		return fmt.Errorf("hook decision=%q, want %q", got, want)
	}
	return nil
}
