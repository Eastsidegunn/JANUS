package fixturecheck

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runScript(t *testing.T, name string, env []string, args ...string) (exitCode int, output string) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", name))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("스크립트 실행 실패: %v", err)
	return -1, ""
}

func asExitError(err error, dst **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*dst = ee
		return true
	}
	return false
}

func secretCheck(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return runScript(t, "check-fixture-secrets.sh", nil, args...)
}

// fail-closed 3분기: 무검출 0 / 검출 1 / 대상 부재·사용법 오류·grep 오류 2.
func TestSecretCheckGate(t *testing.T) {
	t.Run("무검출", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "clean.ndjson"), []byte(`{"v":1,"kind":"subagent/ready"}`), 0o644)
		code, out := secretCheck(t, dir)
		if code != 0 {
			t.Fatalf("exit %d (0 기대): %s", code, out)
		}
	})
	t.Run("검출", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "leak.ndjson"),
			[]byte(`{"token":"sk-ant-api03-abcdefghijklmnop"}`), 0o644)
		code, out := secretCheck(t, dir)
		if code != 1 {
			t.Fatalf("exit %d (1 기대): %s", code, out)
		}
		if !strings.Contains(out, "leak.ndjson") {
			t.Fatalf("검출 위치가 출력에 없음: %s", out)
		}
	})
	t.Run("빈 디렉토리도 무검출", func(t *testing.T) {
		code, out := secretCheck(t, t.TempDir())
		if code != 0 {
			t.Fatalf("exit %d (0 기대): %s", code, out)
		}
	})
	t.Run("대상 부재", func(t *testing.T) {
		code, _ := secretCheck(t, filepath.Join(t.TempDir(), "no-such-dir"))
		if code != 2 {
			t.Fatalf("exit %d (2 기대)", code)
		}
	})
	t.Run("인자 누락", func(t *testing.T) {
		code, _ := secretCheck(t)
		if code != 2 {
			t.Fatalf("exit %d (2 기대)", code)
		}
	})
	t.Run("AWS·GitHub 패턴", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "x.ndjson"),
			[]byte("AKIAIOSFODNN7EXAMPLE ghp_0123456789abcdefghij0123456789"), 0o644)
		if code, _ := secretCheck(t, dir); code != 1 {
			t.Fatalf("exit %d (1 기대)", code)
		}
	})

	// grep 실행 오류(2 이상)는 통과로 간주되지 않고 exit 2로 변환된다.
	// PATH에 exit 7로 끝나는 가짜 grep을 심어 실제 오류 분기를 만든다.
	t.Run("grep 실행 오류", func(t *testing.T) {
		fakeBin := t.TempDir()
		fakeGrep := filepath.Join(fakeBin, "grep")
		if err := os.WriteFile(fakeGrep, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "x.ndjson"), []byte("clean"), 0o644)
		env := append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
		code, out := runScript(t, "check-fixture-secrets.sh", env, dir)
		if code != 2 {
			t.Fatalf("grep exit 7 → 스크립트 exit %d (2 기대): %s", code, out)
		}
		if !strings.Contains(out, "통과로 간주하지 않음") {
			t.Fatalf("오류 진단 문구 없음: %s", out)
		}
	})
}

func manifestCheck(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return runScript(t, "check-fixture-manifest.sh", nil, args...)
}

// 매니페스트 검사: README 존재, NDJSON↔meta 대응, skip 별도 보고, 최소 개수.
func TestManifestGate(t *testing.T) {
	// 완전한 픽스처 루트를 만든다 (녹화 n건 + README).
	build := func(t *testing.T, n int, withReadme bool) string {
		t.Helper()
		root := t.TempDir()
		dir := filepath.Join(root, "claude-code")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if withReadme {
			os.WriteFile(filepath.Join(root, "README.md"), []byte("# 녹화 목록\n"), 0o644)
		}
		for i := 1; i <= n; i++ {
			base := filepath.Join(dir, fmt.Sprintf("%02d-case", i))
			os.WriteFile(base+".ndjson", []byte("{}\n"), 0o644)
			os.WriteFile(base+".meta.txt", []byte("meta\n"), 0o644)
		}
		return root
	}

	t.Run("완전 충족", func(t *testing.T) {
		code, out := manifestCheck(t, build(t, 15, true))
		if code != 0 {
			t.Fatalf("exit %d (0 기대): %s", code, out)
		}
	})
	t.Run("README 없음", func(t *testing.T) {
		code, out := manifestCheck(t, build(t, 15, false))
		if code != 1 || !strings.Contains(out, "README.md 없음") {
			t.Fatalf("exit %d: %s", code, out)
		}
	})
	t.Run("meta 누락", func(t *testing.T) {
		root := build(t, 15, true)
		os.Remove(filepath.Join(root, "claude-code", "03-case.meta.txt"))
		code, out := manifestCheck(t, root)
		if code != 1 || !strings.Contains(out, "meta 누락") {
			t.Fatalf("exit %d: %s", code, out)
		}
	})
	t.Run("최소 개수 미달", func(t *testing.T) {
		code, out := manifestCheck(t, build(t, 14, true))
		if code != 1 || !strings.Contains(out, "최소 15건 필요") {
			t.Fatalf("exit %d: %s", code, out)
		}
	})
	t.Run("meta-only skip은 개수에 미포함", func(t *testing.T) {
		root := build(t, 15, true)
		// 녹화 없이 meta만 있는 skip 항목 추가 — 보고되지만 15건 계수에는 불참
		os.WriteFile(filepath.Join(root, "claude-code", "99-skipped.meta.txt"),
			[]byte("skip 사유: 도구 미지원\n"), 0o644)
		code, out := manifestCheck(t, root)
		if code != 0 {
			t.Fatalf("exit %d (0 기대): %s", code, out)
		}
		if !strings.Contains(out, "meta-only skip") || !strings.Contains(out, "99-skipped") {
			t.Fatalf("skip이 별도 보고되지 않음: %s", out)
		}
		if !strings.Contains(out, "녹화 15건") {
			t.Fatalf("skip이 녹화 수에 포함됨: %s", out)
		}
		// skip만 있고 녹화가 부족하면 실패한다
		short := build(t, 14, true)
		os.WriteFile(filepath.Join(short, "claude-code", "99-skipped.meta.txt"), []byte("skip\n"), 0o644)
		if code, out := manifestCheck(t, short); code != 1 {
			t.Fatalf("skip으로 개수를 채워 통과함: exit %d %s", code, out)
		}
	})
	t.Run("사용법·대상 오류", func(t *testing.T) {
		if code, _ := manifestCheck(t); code != 2 {
			t.Fatalf("인자 누락 exit %d (2 기대)", code)
		}
		if code, _ := manifestCheck(t, filepath.Join(t.TempDir(), "nope")); code != 2 {
			t.Fatalf("대상 부재 exit %d (2 기대)", code)
		}
		if code, _ := manifestCheck(t, t.TempDir(), "열다섯"); code != 2 {
			t.Fatalf("비정수 인자 exit %d (2 기대)", code)
		}
	})
}
