package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// codegen drift 게이트의 회귀 테스트 ([H] 리뷰 요구 케이스 포함):
// (1) 깨끗한 상태에서는 통과한다.
// (2) 재생성 대상이 아닌 stale 생성물이 있으면 실패한다 — 게이트가 파일
//
//	집합을 비교하므로 git 추적 여부와 무관하게 잡힌다(추적된 stale
//	파일을 놓치던 git status 방식의 회귀 고정).
//
// (3) 커밋된 생성물이 삭제되면 실패한다.
//
// 케이스 (2)(3)은 실제 contracts/gen이 아니라 사본(GEN_DIR 오버라이드)을
// 훼손한다 — 병렬 패키지 빌드와의 경합을 피하기 위해.
func TestCodegenDriftGate(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make 없음")
	}

	runDrift := func(genDir string) (string, error) {
		args := []string{"codegen-drift"}
		if genDir != "" {
			args = append(args, "GEN_DIR="+genDir)
		}
		cmd := exec.Command("make", args...)
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := runDrift(""); err != nil {
		t.Fatalf("깨끗한 상태에서 drift 게이트가 실패: %v\n%s", err, out)
	}

	copyGen := func(t *testing.T) string {
		t.Helper()
		src := filepath.Join(repoRoot, "contracts", "gen")
		dst := t.TempDir()
		entries, err := os.ReadDir(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(src, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dst
	}

	t.Run("사본 무결 통과", func(t *testing.T) {
		if out, err := runDrift(copyGen(t)); err != nil {
			t.Fatalf("무결 사본에서 drift 게이트가 실패: %v\n%s", err, out)
		}
	})

	t.Run("stale 생성물", func(t *testing.T) {
		dir := copyGen(t)
		if err := os.WriteFile(filepath.Join(dir, "stale.gen.go"), []byte("package gen\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runDrift(dir); err == nil {
			t.Fatal("stale 생성물이 있는데 drift 게이트가 통과함")
		}
	})

	t.Run("생성물 삭제", func(t *testing.T) {
		dir := copyGen(t)
		if err := os.Remove(filepath.Join(dir, "wire.gen.go")); err != nil {
			t.Fatal(err)
		}
		if _, err := runDrift(dir); err == nil {
			t.Fatal("생성물이 삭제됐는데 drift 게이트가 통과함")
		}
	})

	t.Run("내용 변조", func(t *testing.T) {
		dir := copyGen(t)
		target := filepath.Join(dir, "events.gen.go")
		b, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, append(b, []byte("\n// 손수정\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runDrift(dir); err == nil {
			t.Fatal("생성물이 손수정됐는데 drift 게이트가 통과함")
		}
	})
}
