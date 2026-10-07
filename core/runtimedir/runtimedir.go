// Package runtimedir resolves and validates the host directory used for
// short-lived Unix sockets and other runtime artifacts.
package runtimedir

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const (
	envName    = "HX_RUNTIME_DIR"
	defaultDir = "/tmp"

	// os.MkdirTemp appends strconv.FormatUint(uint32(runtime_rand()), 10) when
	// the pattern has no '*'. A uint32 in decimal is at most ten bytes.
	maxMkdirTempRandomBytes = 10

	// MaxSocketSuffixBytes is the largest relative suffix used by every runtime
	// socket layout (hxapprove approval, hxa adapter/relay, hxe audit, and hxp
	// process), including the temporary directory name (34 bytes today).
	// Keep this single source of truth when adding or renaming a socket.
	MaxSocketSuffixBytes = len("hxapprove-") + maxMkdirTempRandomBytes + len("/") + len("approval.sock")

	// Unix sun_path includes its terminating NUL on the supported platforms.
	// Keep the platform limits explicit so the validation remains auditable.
	darwinUnixSocketLimit = 104
	linuxUnixSocketLimit  = 108
)

// Dir returns the validated runtime directory. An unset or empty
// HX_RUNTIME_DIR deliberately falls back to /tmp; TMPDIR is not consulted
// because the Unix socket path limits make long temporary roots unsafe.
func Dir() (string, error) {
	dir := os.Getenv(envName)
	if dir == "" {
		dir = defaultDir
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s must be an absolute path: %q", envName, dir)
	}
	clean := filepath.Clean(dir)
	if strings.ContainsRune(clean, ':') {
		return "", fmt.Errorf("%s must not contain ':' (Podman volume syntax): %q", envName, clean)
	}

	// Symlinks are allowed for operator-managed runtime roots. Resolve them to
	// reject dangling links/loops and to judge the target's type, but retain the
	// cleaned lexical path: Unix bind calls enforce sun_path against the path
	// supplied by the caller, not an expanded target spelling.
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("%s runtime directory cannot be resolved: %q: %w", envName, clean, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%s runtime directory cannot be inspected: %q: %w", envName, clean, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s must name an existing directory: %q", envName, clean)
	}
	if info.Mode().Perm()&0o002 != 0 && info.Mode()&os.ModeSticky == 0 {
		return "", fmt.Errorf("%s runtime directory must not be world-writable without sticky bit: %q", envName, clean)
	}
	if err := syscall.Access(resolved, 0x2|0x1); err != nil {
		return "", fmt.Errorf("%s runtime directory must be writable and searchable: %q: %w", envName, clean, err)
	}

	limit := unixSocketLimit()
	longest := longestSocketPathLength(clean)
	// The limit includes the trailing NUL in sockaddr_un.sun_path. Reject only
	// when the path plus that terminator exceeds the platform limit.
	if longest+1 > limit {
		return "", fmt.Errorf("%s runtime directory %q is too long: longest Unix socket path is %d bytes (including NUL %d), platform limit %d; maximum socket suffix %d", envName, clean, longest, longest+1, limit, MaxSocketSuffixBytes)
	}
	return clean, nil
}

func unixSocketLimit() int {
	switch runtime.GOOS {
	case "darwin":
		return darwinUnixSocketLimit
	case "linux":
		return linuxUnixSocketLimit
	default:
		// Runtime sockets are supported only on Unix in this repository. Keep
		// validation conservative for other build targets rather than silently
		// allowing a path that would fail on Linux deployment.
		return linuxUnixSocketLimit
	}
}

func longestSocketPathLength(dir string) int {
	separator := 0
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		separator = 1
	}
	return len(dir) + separator + MaxSocketSuffixBytes
}
