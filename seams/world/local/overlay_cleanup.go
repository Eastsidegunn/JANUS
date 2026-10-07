package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	errOverlayCleanupPath     = errors.New("world/local: overlay cleanup path validation")
	errOverlayCleanupPodman   = errors.New("world/local: overlay cleanup podman")
	errOverlayCleanupResidual = errors.New("world/local: overlay cleanup target remains")
)

// overlayCleanupTarget is a closed, package-private capability. Work cleanup
// is unconditional at lease close; upper cleanup is unlocked only by a
// lease-bound durable collector receipt.
type overlayCleanupTarget uint8

const overlayCleanupWork overlayCleanupTarget = iota + 1
const overlayCleanupUpper overlayCleanupTarget = iota + 2

type overlayCleanupCapability struct {
	runner                      commandRunner
	stateRoot, traceID, spanID  string
	stateDir, workDir, upperDir string
}

func newOverlayCleanupCapability(stateRoot string, layout overlayLayout, runner commandRunner) overlayCleanupCapability {
	return overlayCleanupCapability{
		runner: runner, stateRoot: stateRoot, traceID: layout.traceID, spanID: layout.spanID,
		stateDir: layout.stateDir, workDir: layout.work, upperDir: layout.upper,
	}
}

func (c overlayCleanupCapability) remove(ctx context.Context, target overlayCleanupTarget) error {
	path, err := c.validateTarget(target)
	if err != nil {
		return err
	}
	if _, err := c.runner.Run(ctx, "unshare", "rm", "-rf", "--", path); err != nil {
		return fmt.Errorf("%w: work: %w", errOverlayCleanupPodman, err)
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%w: %q", errOverlayCleanupResidual, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: lstat %q: %v", errOverlayCleanupResidual, path, err)
	}
	return nil
}

func (c overlayCleanupCapability) validateTarget(target overlayCleanupTarget) (string, error) {
	if c.runner == nil {
		return "", fmt.Errorf("%w: runner 없음", errOverlayCleanupPath)
	}
	if !filepath.IsAbs(c.stateRoot) || filepath.Clean(c.stateRoot) != c.stateRoot {
		return "", fmt.Errorf("%w: state root canonical 형식 위반: %q", errOverlayCleanupPath, c.stateRoot)
	}
	if !tracePattern.MatchString(c.traceID) || !spanPattern.MatchString(c.spanID) {
		return "", fmt.Errorf("%w: trace/span 형식 위반", errOverlayCleanupPath)
	}
	expectedState := filepath.Join(c.stateRoot, "world", c.traceID, c.spanID)
	if c.stateDir != expectedState || filepath.Clean(c.stateDir) != c.stateDir {
		return "", fmt.Errorf("%w: state dir 불일치: %q", errOverlayCleanupPath, c.stateDir)
	}
	var path string
	switch target {
	case overlayCleanupWork:
		path = c.workDir
	case overlayCleanupUpper:
		path = c.upperDir
	default:
		return "", fmt.Errorf("%w: 허용되지 않은 target=%d", errOverlayCleanupPath, target)
	}
	expectedTarget := filepath.Join(expectedState, "overlay", "work")
	if target == overlayCleanupUpper {
		expectedTarget = filepath.Join(expectedState, "overlay", "upper")
	}
	if path != expectedTarget || filepath.Clean(path) != path || !pathWithin(c.stateRoot, path) {
		return "", fmt.Errorf("%w: work target 불일치: %q", errOverlayCleanupPath, path)
	}
	if err := validateCleanupAncestors(c.stateRoot, path); err != nil {
		return "", fmt.Errorf("%w: %v", errOverlayCleanupPath, err)
	}
	return path, nil
}

func validateCleanupAncestors(stateRoot, target string) error {
	rel, err := filepath.Rel(stateRoot, target)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("target containment 위반: %q", target)
	}
	paths := []string{stateRoot}
	current := stateRoot
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("target component 위반: %q", component)
		}
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	for i, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("ancestor lstat %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("ancestor가 실제 디렉터리가 아님: %q (%s)", path, info.Mode())
		}
		if i == 0 && info.Mode().Perm() != 0o700 {
			return fmt.Errorf("state root mode는 0700이어야 함: %04o", info.Mode().Perm())
		}
	}
	return nil
}
