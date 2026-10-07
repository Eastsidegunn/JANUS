// codex is the independent Codex §5.2 adapter executable (FR-ADP-09).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Eastsidegunn/JANUS/core/buildinfo"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/seams/subagent/codex"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.Line("codex"))
		return
	}
	mode := policy.ApprovalManual
	if os.Getenv("HX_APPROVAL_MODE") == string(policy.ApprovalAuto) {
		mode = policy.ApprovalAuto
	}
	if err := codex.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr, mode); err != nil {
		fmt.Fprintln(os.Stderr, "codex:", err)
		os.Exit(1)
	}
}
