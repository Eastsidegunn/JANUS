// Package buildinfo provides the binary self-identification line shared by
// every JANUS executable. It is a lowest-level shared utility, outside the
// core domain, and intentionally depends on the standard library only.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// version and commit are populated by release builds with -ldflags -X.
var version = "devel"
var commit string

// Line returns the one-line identity string for name.
func Line(name string) string {
	v, c, dirty := version, commit, false
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	return formatLine(name, v, c, dirty && commit == "")
}

func formatLine(name, v, c string, dirty bool) string {
	if c == "" {
		c = "unknown"
	} else if dirty {
		c += "-dirty"
	}
	return fmt.Sprintf("%s %s (commit %s, %s)", name, v, c, runtime.Version())
}
