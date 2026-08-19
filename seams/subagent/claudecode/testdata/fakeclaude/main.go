// fakeclaude replays a real T8 Claude fixture for adapter process tests. It
// never invents stream-json lines; optional modes only omit the native result,
// replay the fixture's first line twice, hold the process open, or choose an
// exit code.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
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
	for scanner.Scan() {
		line := scanner.Bytes()
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
		if first && os.Getenv("HX_CLAUDE_DUPLICATE_FIRST") == "1" {
			if _, err := os.Stdout.Write(append(append([]byte(nil), line...), '\n')); err != nil {
				os.Exit(2)
			}
		}
		first = false
	}
	f.Close()
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude:", err)
		os.Exit(2)
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
