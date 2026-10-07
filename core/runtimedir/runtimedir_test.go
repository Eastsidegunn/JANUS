package runtimedir

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDirDefaultsToTmpAndIgnoresTMPDIR(t *testing.T) {
	t.Setenv(envName, "")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "ignored"))
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultDir {
		t.Fatalf("Dir()=%q, want %q", got, defaultDir)
	}
}

func TestDirRejectsRelativePath(t *testing.T) {
	t.Setenv(envName, "runtime")
	if _, err := Dir(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative path error=%v", err)
	}
}

func TestDirRejectsColon(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "colon:runtime")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envName, path)
	if _, err := Dir(); err == nil || !strings.Contains(err.Error(), ":") {
		t.Fatalf("colon path error=%v", err)
	}
}

func TestDirRejectsWorldWritableWithoutSticky(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "hxrd-perm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	path := filepath.Join(base, "shared")
	if err := os.Mkdir(path, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envName, path)
	if _, err := Dir(); err == nil || !strings.Contains(err.Error(), "sticky") {
		t.Fatalf("world-writable path error=%v", err)
	}
}

func TestDirRejectsNotWritableOrSearchable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses syscall.Access permission checks")
	}
	path := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(path, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envName, path)
	if _, err := Dir(); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("0500 path error=%v", err)
	}
}

func TestDirRejectsMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	t.Setenv(envName, path)
	if _, err := Dir(); err == nil || !strings.Contains(err.Error(), envName) {
		t.Fatalf("missing path error=%v", err)
	}
}

func TestDirRejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envName, path)
	if _, err := Dir(); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("file path error=%v", err)
	}
}

func TestDirAcceptsNormalDirectoryAndSymlink(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "hxrd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envName, target)
	if got, err := Dir(); err != nil || got != target {
		t.Fatalf("normal directory: got=%q err=%v", got, err)
	}
	link := filepath.Join(root, "runtime-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv(envName, link)
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(link) {
		t.Fatalf("Dir()=%q, want cleaned lexical symlink %q", got, filepath.Clean(link))
	}
}

func TestDirRejectsTooLongSocketPath(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "hxrd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	long := filepath.Join(base, strings.Repeat("r", unixSocketLimit()-len(base)-MaxSocketSuffixBytes+2))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envName, long)
	if _, err := Dir(); err == nil || !strings.Contains(err.Error(), "platform limit") {
		t.Fatalf("too-long path error=%v", err)
	}
}

func TestDirSocketLengthBoundary(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "hxrd-b-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	nameLen := unixSocketLimit() - 1 - len(base) - 1 - MaxSocketSuffixBytes - 1
	path := filepath.Join(base, strings.Repeat("r", nameLen))
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envName, path)
	if _, err := Dir(); err != nil {
		t.Fatalf("boundary should pass: %v", err)
	}
	tooLong := filepath.Join(base, strings.Repeat("s", nameLen+1))
	if err := os.Mkdir(tooLong, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envName, tooLong)
	if _, err := Dir(); err == nil {
		t.Fatal("boundary+1 should fail")
	}
}

func TestMaxSocketPathBindsAtBoundary(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "hxrd-b-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	nameLen := unixSocketLimit() - 1 - len(base) - 3
	dir := filepath.Join(base, strings.Repeat("b", nameLen))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "x")
	if len(path)+1 != unixSocketLimit() {
		t.Fatalf("path length=%d limit=%d", len(path)+1, unixSocketLimit())
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("boundary bind failed on %s: %v", runtime.GOOS, err)
	}
	_ = l.Close()
}
