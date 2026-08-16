package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// codegen drift 게이트의 회귀 테스트 ([H] 리뷰 요구):
// (1) 깨끗한 트리에서는 통과하고,
// (2) 커밋되지 않은 미추적 생성 파일이 있으면 실패해야 한다 —
// git diff --exit-code 방식이 놓치던 케이스.
func TestCodegenDriftGate(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make 없음")
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".git")); err != nil {
		t.Skip("git 저장소 아님")
	}

	runDrift := func() error {
		cmd := exec.Command("make", "codegen-drift")
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			return &driftErr{err: err, out: string(out)}
		}
		return nil
	}

	if err := runDrift(); err != nil {
		t.Fatalf("깨끗한 트리에서 drift 게이트가 실패: %v", err)
	}

	stray := filepath.Join(repoRoot, "contracts", "gen", "stray_test_artifact.gen.go")
	if err := os.WriteFile(stray, []byte("package gen\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(stray) })

	if err := runDrift(); err == nil {
		t.Fatal("미추적 신규 생성 파일이 있는데 drift 게이트가 통과함")
	}
}

type driftErr struct {
	err error
	out string
}

func (e *driftErr) Error() string { return e.err.Error() + "\n" + e.out }
