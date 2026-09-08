//go:build codexsmoke

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

func TestCodexSmoke(t *testing.T) {
	bin := os.Getenv("HX_CODEX_BIN")
	if bin == "" {
		bin = "codex"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		t.Fatalf("codex not found: %v", err)
	}
	ver, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("codex version: %s", strings.TrimSpace(string(ver)))
	workspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "exec", "--json", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "-s", "workspace-write", "-C", workspace, "현재 디렉토리에 codex-smoke.txt 파일을 만들고 내용은 ok로 해라")
	native, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("codex execution failed: %v\n%s", err, tail(native))
	}
	input := append([]byte(`{"v":1,"cmd":"task","payload":{}}`+"\n"), native...)
	var normalized, diag bytes.Buffer
	if err := Run(ctx, bytes.NewReader(input), &normalized, &diag, policy.ApprovalAuto); err != nil {
		t.Fatalf("adapter failed: %v\n%s", err, diag.String())
	}
	vals, _ := validate.New()
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(normalized.String()), "\n") {
		if err := vals.ValidateEvent([]byte(line)); err != nil {
			t.Fatal(err)
		}
		var e struct {
			Kind gen.EventKind `json:"kind"`
		}
		_ = json.Unmarshal([]byte(line), &e)
		kinds = append(kinds, string(e.Kind))
	}
	if len(kinds) < 2 || kinds[0] != string(gen.EventKindSubagentReady) || kinds[len(kinds)-1] != string(gen.EventKindSubagentDone) {
		t.Fatalf("invalid boundary order: %v", kinds)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "codex-smoke.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "ok" {
		t.Fatalf("marker missing or incorrect: %v %q", err, data)
	}
	t.Logf("native tail:\n%s\nnormalized kinds: %s", tail(native), strings.Join(kinds, " -> "))
}

func tail(b []byte) string {
	const n = 4096
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return fmt.Sprintf("%s", b)
}
