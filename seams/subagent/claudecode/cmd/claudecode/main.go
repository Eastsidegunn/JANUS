// claudecode is the Claude Code adapter executable (FR-ADP-01). It speaks
// §5.2 NDJSON on stdin/stdout and keeps diagnostics on stderr.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Eastsidegunn/JANUS/core/buildinfo"
	"github.com/Eastsidegunn/JANUS/seams/subagent/claudecode"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.Line("claudecode"))
		return
	}
	if err := claudecode.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr, claudecode.ConfigFromEnv()); err != nil {
		fmt.Fprintln(os.Stderr, "claudecode:", err)
		os.Exit(1)
	}
}
