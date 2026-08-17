package fixturecheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runScript(t *testing.T, args ...string) (exitCode int, output string) {
	t.Helper()
	script, err := filepath.Abs("../check-fixture-secrets.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("스크립트 실행 실패: %v", err)
	return -1, ""
}

// fail-closed 3분기: 무검출 0 / 검출 1 / 대상 부재·사용법 오류 2.
func TestSecretCheckGate(t *testing.T) {
	t.Run("무검출", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "clean.ndjson"), []byte(`{"v":1,"kind":"subagent/ready"}`), 0o644)
		code, out := runScript(t, dir)
		if code != 0 {
			t.Fatalf("exit %d (0 기대): %s", code, out)
		}
	})
	t.Run("검출", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "leak.ndjson"),
			[]byte(`{"token":"sk-ant-api03-abcdefghijklmnop"}`), 0o644)
		code, out := runScript(t, dir)
		if code != 1 {
			t.Fatalf("exit %d (1 기대): %s", code, out)
		}
		if !strings.Contains(out, "leak.ndjson") {
			t.Fatalf("검출 위치가 출력에 없음: %s", out)
		}
	})
	t.Run("빈 디렉토리도 무검출", func(t *testing.T) {
		code, out := runScript(t, t.TempDir())
		if code != 0 {
			t.Fatalf("exit %d (0 기대): %s", code, out)
		}
	})
	t.Run("대상 부재", func(t *testing.T) {
		code, _ := runScript(t, filepath.Join(t.TempDir(), "no-such-dir"))
		if code != 2 {
			t.Fatalf("exit %d (2 기대)", code)
		}
	})
	t.Run("인자 누락", func(t *testing.T) {
		code, _ := runScript(t)
		if code != 2 {
			t.Fatalf("exit %d (2 기대)", code)
		}
	})
	t.Run("AWS·GitHub 패턴", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "x.ndjson"),
			[]byte("AKIAIOSFODNN7EXAMPLE ghp_0123456789abcdefghij0123456789"), 0o644)
		if code, _ := runScript(t, dir); code != 1 {
			t.Fatalf("exit %d (1 기대)", code)
		}
	})
}
