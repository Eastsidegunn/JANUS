// fakeclaude replays a real T8 Claude fixture for adapter process tests. It
// never invents stream-json lines; optional modes only omit the native result,
// replay the fixture's first line twice, hold the process open, or choose an
// exit code.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func main() {
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
		cmd := exec.Command("hxapprove")
		cmd.Env = os.Environ()
		cmd.Stdin = bytes.NewReader(raw)
		var stdout, stderr bytes.Buffer
		var hookFile *os.File
		if path := os.Getenv("HX_CLAUDE_HOOK_OUT"); path != "" {
			var err error
			hookFile, err = os.Create(path)
			if err != nil {
				return fmt.Errorf("hook output: %w", err)
			}
			cmd.Stdout = hookFile
		} else {
			cmd.Stdout = &stdout
		}
		cmd.Stderr = &stderr
		err := cmd.Run()
		if hookFile != nil {
			_ = hookFile.Close()
		}
		if err != nil {
			return fmt.Errorf("hook: %w: %s", err, stderr.String())
		}
		return nil
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
