package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOverlayCleanupWorkUsesExactPrivateTargetAndPreservesUpper(t *testing.T) {
	cleanup, runner, upper, outside := newOverlayCleanupFixture(t)
	if err := cleanup.remove(context.Background(), overlayCleanupWork); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(cleanup.workDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("work target가 남음: %v", err)
	}
	if _, err := os.Lstat(upper); err != nil {
		t.Fatalf("T11 ACK 전 upper가 삭제됨: %v", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "keep\n" {
		t.Fatalf("state root 밖 변경: got=%q err=%v", got, err)
	}
	want := []string{"unshare", "rm", "-rf", "--", cleanup.workDir}
	calls := runner.snapshot()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("cleanup argv=%v want=%v", calls, want)
	}
}

func TestOverlayCleanupFailureCategoriesAreDistinct(t *testing.T) {
	t.Run("path validation", func(t *testing.T) {
		cleanup, runner, _, outside := newOverlayCleanupFixture(t)
		cleanup.workDir = filepath.Dir(outside)
		err := cleanup.remove(context.Background(), overlayCleanupWork)
		if !errors.Is(err, errOverlayCleanupPath) || errors.Is(err, errOverlayCleanupPodman) || errors.Is(err, errOverlayCleanupResidual) {
			t.Fatalf("path category=%v", err)
		}
		if len(runner.snapshot()) != 0 {
			t.Fatalf("path 검증 실패가 Podman에 도달함: %v", runner.snapshot())
		}
	})

	t.Run("podman", func(t *testing.T) {
		cleanup, runner, _, _ := newOverlayCleanupFixture(t)
		injected := errors.New("unshare unavailable")
		runner.failures["unshare"] = injected
		err := cleanup.remove(context.Background(), overlayCleanupWork)
		if !errors.Is(err, errOverlayCleanupPodman) || errors.Is(err, errOverlayCleanupPath) || errors.Is(err, errOverlayCleanupResidual) {
			t.Fatalf("podman category=%v", err)
		}
		if _, statErr := os.Lstat(cleanup.workDir); statErr != nil {
			t.Fatalf("Podman 실패인데 target이 사라짐: %v", statErr)
		}
	})

	t.Run("postcondition", func(t *testing.T) {
		cleanup, runner, _, _ := newOverlayCleanupFixture(t)
		runner.hook = func(args []string) ([]byte, error, bool) {
			if commandKey(args) == "unshare" {
				return nil, nil, true
			}
			return nil, nil, false
		}
		err := cleanup.remove(context.Background(), overlayCleanupWork)
		if !errors.Is(err, errOverlayCleanupResidual) || errors.Is(err, errOverlayCleanupPath) || errors.Is(err, errOverlayCleanupPodman) {
			t.Fatalf("postcondition category=%v", err)
		}
	})
}

func TestOverlayCleanupRejectsSymlinkAncestorBeforePodman(t *testing.T) {
	cleanup, runner, _, outside := newOverlayCleanupFixture(t)
	overlay := filepath.Dir(cleanup.workDir)
	escapedOverlay := filepath.Join(filepath.Dir(cleanup.stateRoot), "escaped-overlay")
	if err := os.Rename(overlay, escapedOverlay); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escapedOverlay, overlay); err != nil {
		t.Fatal(err)
	}
	err := cleanup.remove(context.Background(), overlayCleanupWork)
	if !errors.Is(err, errOverlayCleanupPath) || !strings.Contains(err.Error(), "실제 디렉터리가 아님") {
		t.Fatalf("symlink ancestor err=%v", err)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatalf("symlink 탈출이 Podman에 도달함: %v", runner.snapshot())
	}
	if got, readErr := os.ReadFile(outside); readErr != nil || string(got) != "keep\n" {
		t.Fatalf("state root 밖 sentinel 변경: got=%q err=%v", got, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(escapedOverlay, "work")); statErr != nil {
		t.Fatalf("탈출 대상이 변경됨: %v", statErr)
	}
}

func TestOverlayCleanupRejectsUnknownCapability(t *testing.T) {
	cleanup, runner, _, _ := newOverlayCleanupFixture(t)
	err := cleanup.remove(context.Background(), overlayCleanupTarget(255))
	if !errors.Is(err, errOverlayCleanupPath) {
		t.Fatalf("unknown target err=%v", err)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatalf("unknown target이 Podman에 도달함: %v", runner.snapshot())
	}
}

func newOverlayCleanupFixture(t *testing.T) (overlayCleanupCapability, *fakePodman, string, string) {
	t.Helper()
	base := t.TempDir()
	stateRoot := filepath.Join(base, "state")
	traceID, spanID := strings.Repeat("1", 32), strings.Repeat("2", 16)
	stateDir := filepath.Join(stateRoot, "world", traceID, spanID)
	upper := filepath.Join(stateDir, "overlay", "upper")
	work := filepath.Join(stateDir, "overlay", "work")
	for _, path := range []string{stateRoot, upper, work} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(base, "outside-sentinel")
	if err := os.WriteFile(outside, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newFakePodman("sha256:" + strings.Repeat("a", 64))
	layout := overlayLayout{stateDir: stateDir, upper: upper, work: work, traceID: traceID, spanID: spanID}
	return newOverlayCleanupCapability(stateRoot, layout, runner), runner, upper, outside
}
