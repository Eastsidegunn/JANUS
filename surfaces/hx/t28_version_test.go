package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if t28Binaries.binDir != "" {
		_ = os.RemoveAll(t28Binaries.binDir)
	}
	os.Exit(code)
}

// T28 builds the command binaries once for this package. Keeping the builds in
// one sync.Once makes the CLI assertions cheap when the tests are run together.
var t28Binaries = struct {
	sync.Once
	root       string
	binDir     string
	hx         string
	hxInjected string
	adapters   map[string]string
	err        error
}{}

func t28BuildBinaries(t *testing.T) {
	t.Helper()
	t28Binaries.Do(func() {
		t28Binaries.root = filepath.Clean(filepath.Join("..", ".."))
		t28Binaries.binDir, t28Binaries.err = os.MkdirTemp("", "janus-t28-version-")
		if t28Binaries.err != nil {
			return
		}
		t28Binaries.adapters = make(map[string]string)

		build := func(output, pkg string, ldflags ...string) error {
			args := []string{"build"}
			if len(ldflags) != 0 {
				args = append(args, "-ldflags", strings.Join(ldflags, " "))
			}
			args = append(args, "-o", output, pkg)
			cmd := exec.Command("go", args...)
			cmd.Dir = t28Binaries.root
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("go build %s: %w\n%s", pkg, err, out)
			}
			return nil
		}

		t28Binaries.hx = filepath.Join(t28Binaries.binDir, "hx")
		t28Binaries.hxInjected = filepath.Join(t28Binaries.binDir, "hx-injected")
		if err := build(t28Binaries.hx, "./surfaces/hx"); err != nil {
			t28Binaries.err = err
			return
		}
		if err := build(t28Binaries.hxInjected, "./surfaces/hx",
			"-X github.com/Eastsidegunn/JANUS/core/buildinfo.version=9.9.9-test",
			"-X github.com/Eastsidegunn/JANUS/core/buildinfo.commit=deadbeef",
		); err != nil {
			t28Binaries.err = err
			return
		}
		packages := map[string]string{
			"claudecode":    "./seams/subagent/claudecode/cmd/claudecode",
			"codex":         "./seams/subagent/codex/cmd/codex",
			"worldadapter":  "./seams/subagent/worldadapter/cmd/worldadapter",
			"hxapprove":     "./seams/subagent/claudecode/hxapprove",
			"hxegressproxy": "./seams/world/local/egressproxy/cmd/hxegressproxy",
		}
		for name, pkg := range packages {
			path := filepath.Join(t28Binaries.binDir, name)
			if err := build(path, pkg); err != nil {
				t28Binaries.err = err
				return
			}
			t28Binaries.adapters[name] = path
		}
	})
	if t28Binaries.err != nil {
		t.Fatal(t28Binaries.err)
	}
}

func t28Run(t *testing.T, binary string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	var out, diag bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diag
	err = cmd.Run()
	return out.String(), diag.String(), err
}

func TestT28InjectedHXVersion(t *testing.T) {
	t28BuildBinaries(t)
	want := regexp.MustCompile(`^hx 9\.9\.9-test \(commit deadbeef, go\S+\)\n$`)
	for _, arg := range []string{"--version", "version"} {
		t.Run(arg, func(t *testing.T) {
			stdout, stderr, err := t28Run(t, t28Binaries.hxInjected, arg)
			if err != nil {
				t.Fatalf("%s: exit: %v", arg, err)
			}
			if !want.MatchString(stdout) {
				t.Fatalf("%s: stdout = %q, want injected identity line", arg, stdout)
			}
			if stderr != "" {
				t.Fatalf("%s: stderr = %q, want empty", arg, stderr)
			}
		})
	}
}

func TestT28UninjectedHXVersionUsesDevel(t *testing.T) {
	t28BuildBinaries(t)
	stdout, stderr, err := t28Run(t, t28Binaries.hx, "--version")
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	want := regexp.MustCompile(`^hx devel \(commit ([0-9a-f]{7,40}(-dirty)?|unknown), go\S+\)\n$`)
	if !want.MatchString(stdout) {
		t.Fatalf("stdout = %q, want complete devel identity line", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestT28HXUnknownCommandPreservesCLIStreams(t *testing.T) {
	t28BuildBinaries(t)
	stdout, stderr, err := t28Run(t, t28Binaries.hx, "no-such-cmd")
	if err == nil {
		t.Fatal("unknown command unexpectedly exited successfully")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if stderr == "" {
		t.Fatal("stderr is empty, want an error")
	}
}

func TestT28AdapterVersions(t *testing.T) {
	t28BuildBinaries(t)
	for name, binary := range t28Binaries.adapters {
		name, binary := name, binary
		t.Run(name, func(t *testing.T) {
			stdout, stderr, err := t28Run(t, binary, "--version")
			if err != nil {
				t.Fatalf("exit: %v", err)
			}
			pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(name) + ` (\S+) \(commit (\S+), go\S+\)\n$`)
			if !pattern.MatchString(stdout) {
				t.Fatalf("stdout = %q, want complete %s identity line", stdout, name)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestT28HXApproveNoInputRetainsExistingErrorPath(t *testing.T) {
	t28BuildBinaries(t)
	cmd := exec.Command(t28Binaries.adapters["hxapprove"])
	cmd.Stdin = strings.NewReader("")
	cmd.Env = envWithout("HX_APPROVAL_SOCKET")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("empty hook input unexpectedly exited successfully")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "HX_APPROVAL_SOCKET 없음") {
		t.Fatalf("stderr = %q, want missing socket diagnostic", stderr.String())
	}
}

func envWithout(key string) []string {
	prefix := key + "="
	env := os.Environ()[:0]
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, prefix) {
			env = append(env, value)
		}
	}
	return env
}
