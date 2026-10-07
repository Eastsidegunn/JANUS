// worldadapter is the host-side §5.2 adapter for a world-owned container
// process (FR-ADP-01, FR-ADP-10).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Eastsidegunn/JANUS/core/buildinfo"
	"github.com/Eastsidegunn/JANUS/seams/subagent/worldadapter"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.Line("worldadapter"))
		return
	}
	if err := worldadapter.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr, worldadapter.ConfigFromEnv()); err != nil {
		fmt.Fprintln(os.Stderr, "worldadapter:", err)
		os.Exit(1)
	}
}
