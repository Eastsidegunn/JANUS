package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// Limits bounds every potentially unbounded operation in the collector. A
// caller must use DefaultLimits or provide strictly positive values; zero and
// negative limits fail closed rather than silently disabling a guard.
type Limits struct {
	MaxNodes          int
	MaxChanges        int
	MaxFileBytes      int64
	MaxTotalHashBytes int64
	MaxManifestBytes  int64
	MaxDepth          int
	MaxPayloadBytes   int64
}

// DefaultLimits is the v0.1 resource policy from docs/t11-collector-proposal.md.
func DefaultLimits() Limits {
	return Limits{
		MaxNodes: 100_000, MaxChanges: 10_000, MaxFileBytes: 256 << 20,
		MaxTotalHashBytes: 2 << 30, MaxManifestBytes: 64 << 20,
		MaxDepth: 256, MaxPayloadBytes: 4 << 20,
	}
}

func (l Limits) validate() error {
	if l.MaxNodes <= 0 || l.MaxChanges <= 0 || l.MaxFileBytes <= 0 ||
		l.MaxTotalHashBytes <= 0 || l.MaxManifestBytes <= 0 || l.MaxDepth <= 0 || l.MaxPayloadBytes <= 0 {
		return errors.New("collector: all resource limits must be positive")
	}
	return nil
}

type manifestEntry struct {
	kind string
	hash string
}

// Manifest is an opaque spawn-time lower snapshot. It intentionally exposes
// no filesystem path or mutable map to callers; the final scanner consumes it
// only through Diff.
type Manifest struct {
	root    string
	entries map[string]manifestEntry
	bytes   int64
}

// BuildBaseline hashes the resolved lower workspace before the process starts.
// Directories are recorded for structural comparison, while regular files and
// symlinks receive semantic hashes. Special nodes are rejected because the
// v0.1 fs_changed schema cannot represent them safely.
func BuildBaseline(ctx context.Context, lower string, limits Limits) (Manifest, error) {
	if err := limits.validate(); err != nil {
		return Manifest{}, err
	}
	root, err := validatedRoot(lower)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{root: root, entries: make(map[string]manifestEntry)}
	state := scanState{ctx: ctx, limits: limits, entries: m.entries}
	if err := state.walk(root, "", 0, false); err != nil {
		return Manifest{}, err
	}
	m.bytes = state.manifestBytes
	return m, nil
}

// Diff verifies that lower is unchanged since BuildBaseline and then maps the
// host-only upper tree to a deterministic fs_changed payload. On any failure
// no partial payload is returned, allowing the caller to preserve upper as
// evidence.
func Diff(ctx context.Context, baseline Manifest, upper string, limits Limits) (gen.FsChangedPayload, error) {
	if err := limits.validate(); err != nil {
		return gen.FsChangedPayload{}, err
	}
	if baseline.root == "" || baseline.entries == nil {
		return gen.FsChangedPayload{}, errors.New("collector: invalid baseline")
	}
	upperRoot, err := validatedRoot(upper)
	if err != nil {
		return gen.FsChangedPayload{}, err
	}
	if samePath(baseline.root, upperRoot) {
		return gen.FsChangedPayload{}, errors.New("collector: lower and upper must differ")
	}

	// Re-scan lower first. A lower mutation is a collection failure, not a
	// normal diff: using the changed lower would corrupt deleted hashes.
	lowerState := scanState{ctx: ctx, limits: limits, entries: make(map[string]manifestEntry)}
	if err := lowerState.walk(baseline.root, "", 0, false); err != nil {
		return gen.FsChangedPayload{}, err
	}
	if !manifestsEqual(baseline.entries, lowerState.entries) {
		return gen.FsChangedPayload{}, errors.New("collector: lower workspace changed since baseline")
	}

	upperState := upperScanState{
		scanState: scanState{ctx: ctx, limits: limits},
		baseline:  baseline.entries,
		seen:      make(map[string]bool),
		changes:   make([]gen.FsChangedPayloadChangesItem, 0),
		opaque:    make([]string, 0),
	}
	if err := upperState.walkUpper(upperRoot, "", 0); err != nil {
		return gen.FsChangedPayload{}, err
	}
	// Native overlay may represent an opaque directory with an xattr rather
	// than explicit whiteouts. Expand absent baseline leaves deterministically.
	for _, dir := range upperState.opaque {
		prefix := dir + "/"
		for path, old := range baseline.entries {
			if !strings.HasPrefix(path, prefix) || upperState.seen[path] {
				continue
			}
			if old.kind != "regular" && old.kind != "symlink" {
				continue
			}
			if err := upperState.addChange(path, old.hash, gen.FsChangedPayloadChangesItemChangeTypeDeleted); err != nil {
				return gen.FsChangedPayload{}, err
			}
		}
	}

	sort.Slice(upperState.changes, func(i, j int) bool {
		return upperState.changes[i].Path < upperState.changes[j].Path
	})
	payload := gen.FsChangedPayload{Changes: upperState.changes}
	b, err := json.Marshal(payload)
	if err != nil {
		return gen.FsChangedPayload{}, fmt.Errorf("collector: marshal fs diff: %w", err)
	}
	if int64(len(b)) > limits.MaxPayloadBytes {
		return gen.FsChangedPayload{}, fmt.Errorf("collector: fs payload exceeds limit (%d bytes)", len(b))
	}
	return payload, nil
}

func validatedRoot(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("collector: root must be an absolute directory")
	}
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return "", fmt.Errorf("collector: root lstat: %w", withoutPath(err))
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("collector: root must be a non-symlink directory")
	}
	return clean, nil
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

type scanState struct {
	ctx           context.Context
	limits        Limits
	entries       map[string]manifestEntry
	nodes         int
	hashBytes     int64
	manifestBytes int64
}

func (s *scanState) check() error {
	select {
	case <-s.ctx.Done():
		return fmt.Errorf("collector: scan canceled: %w", s.ctx.Err())
	default:
	}
	if s.nodes >= s.limits.MaxNodes {
		return errors.New("collector: scan node limit exceeded")
	}
	s.nodes++
	return nil
}

func (s *scanState) walk(root, rel string, depth int, includeRoot bool) error {
	if depth > s.limits.MaxDepth {
		return errors.New("collector: directory depth limit exceeded")
	}
	if includeRoot {
		if err := s.check(); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("collector: read %s: %w", relOrDot(rel), withoutPath(err))
	}
	for _, dirent := range entries {
		if err := s.check(); err != nil {
			return err
		}
		name := dirent.Name()
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		full := filepath.Join(root, filepath.FromSlash(childRel))
		info, err := os.Lstat(full)
		if err != nil {
			return fmt.Errorf("collector: lstat %s: %w", childRel, withoutPath(err))
		}
		if info.IsDir() {
			s.entries[childRel] = manifestEntry{kind: "dir"}
			s.manifestBytes += int64(len(childRel) + 4)
			if s.manifestBytes > s.limits.MaxManifestBytes {
				return errors.New("collector: baseline manifest limit exceeded")
			}
			if err := s.walk(root, childRel, depth+1, false); err != nil {
				return err
			}
			continue
		}
		entry := manifestEntry{}
		switch {
		case info.Mode().IsRegular():
			entry.kind = "regular"
			entry.hash, err = s.hashRegular(full, childRel, info)
		case info.Mode()&os.ModeSymlink != 0:
			entry.kind = "symlink"
			entry.hash, err = hashSymlink(full)
		default:
			return fmt.Errorf("collector: unsupported lower node %s", childRel)
		}
		if err != nil {
			return err
		}
		s.entries[childRel] = entry
		s.manifestBytes += int64(len(childRel) + len(entry.hash) + 8)
		if s.manifestBytes > s.limits.MaxManifestBytes {
			return errors.New("collector: baseline manifest limit exceeded")
		}
	}
	return nil
}

func (s *scanState) hashRegular(full, rel string, before os.FileInfo) (string, error) {
	if before.Size() > s.limits.MaxFileBytes {
		return "", fmt.Errorf("collector: file %s exceeds size limit", rel)
	}
	f, err := os.Open(full)
	if err != nil {
		return "", fmt.Errorf("collector: open %s: %w", rel, withoutPath(err))
	}
	h := sha256.New()
	buf := make([]byte, 32*1024)
	var n int64
	for {
		select {
		case <-s.ctx.Done():
			_ = f.Close()
			return "", fmt.Errorf("collector: hash canceled: %w", s.ctx.Err())
		default:
		}
		read, readErr := f.Read(buf)
		if read > 0 {
			n += int64(read)
			s.hashBytes += int64(read)
			if n > s.limits.MaxFileBytes || s.hashBytes > s.limits.MaxTotalHashBytes {
				_ = f.Close()
				return "", errors.New("collector: hash byte limit exceeded")
			}
			_, _ = h.Write(buf[:read])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = f.Close()
			return "", fmt.Errorf("collector: read %s: %w", rel, readErr)
		}
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("collector: close %s: %w", rel, err)
	}
	after, err := os.Lstat(full)
	if err != nil {
		return "", fmt.Errorf("collector: stat after hash %s: %w", rel, withoutPath(err))
	}
	if !sameFile(before, after) {
		return "", fmt.Errorf("collector: file changed while hashing %s", rel)
	}
	sum := h.Sum(nil)
	return "sha256:" + hex.EncodeToString(sum), nil
}

func sameFile(a, b os.FileInfo) bool {
	if a.Size() != b.Size() || a.Mode() != b.Mode() || !a.ModTime().Equal(b.ModTime()) {
		return false
	}
	aa, aok := a.Sys().(*syscall.Stat_t)
	bb, bok := b.Sys().(*syscall.Stat_t)
	if aok && bok {
		return aa.Ino == bb.Ino && aa.Dev == bb.Dev
	}
	return true
}

func hashSymlink(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("collector: read symlink: %w", withoutPath(err))
	}
	b := append([]byte("symlink\x00"), []byte(target)...)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func relOrDot(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

func manifestsEqual(a, b map[string]manifestEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for path, left := range a {
		if right, ok := b[path]; !ok || left != right {
			return false
		}
	}
	return true
}

type upperScanState struct {
	scanState
	baseline map[string]manifestEntry
	seen     map[string]bool
	changes  []gen.FsChangedPayloadChangesItem
	opaque   []string
}

func (s *upperScanState) walkUpper(root, rel string, depth int) error {
	if depth > s.limits.MaxDepth {
		return errors.New("collector: upper directory depth limit exceeded")
	}
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("collector: read upper %s: %w", relOrDot(rel), withoutPath(err))
	}
	if rel != "" && opaqueDirectory(filepath.Join(root, filepath.FromSlash(rel))) {
		s.opaque = append(s.opaque, rel)
	}
	for _, dirent := range entries {
		if err := s.check(); err != nil {
			return err
		}
		name := dirent.Name()
		if name == ".wh..wh..opq" {
			if rel == "" {
				return errors.New("collector: opaque marker at upper root")
			}
			s.opaque = append(s.opaque, rel)
			continue
		}
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		full := filepath.Join(root, filepath.FromSlash(childRel))
		info, err := os.Lstat(full)
		if err != nil {
			return fmt.Errorf("collector: lstat upper %s: %w", childRel, withoutPath(err))
		}
		s.seen[childRel] = true
		if isWhiteout(info) {
			old, ok := s.baseline[childRel]
			if !ok || (old.kind != "regular" && old.kind != "symlink") {
				return fmt.Errorf("collector: invalid whiteout target %s", childRel)
			}
			if err := s.addChange(childRel, old.hash, gen.FsChangedPayloadChangesItemChangeTypeDeleted); err != nil {
				return err
			}
			continue
		}
		if info.IsDir() {
			if old, ok := s.baseline[childRel]; ok && old.kind != "dir" {
				return fmt.Errorf("collector: node kind conflict %s", childRel)
			}
			if err := s.walkUpper(root, childRel, depth+1); err != nil {
				return err
			}
			continue
		}
		var hash, kind string
		switch {
		case info.Mode().IsRegular():
			kind = "regular"
			hash, err = s.hashRegular(full, childRel, info)
		case info.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
			hash, err = hashSymlink(full)
		default:
			return fmt.Errorf("collector: unsupported upper node %s", childRel)
		}
		if err != nil {
			return err
		}
		old, exists := s.baseline[childRel]
		if exists && old.kind != kind {
			return fmt.Errorf("collector: node kind conflict %s", childRel)
		}
		change := gen.FsChangedPayloadChangesItemChangeTypeAdded
		if exists {
			change = gen.FsChangedPayloadChangesItemChangeTypeModified
		}
		if err := s.addChange(childRel, hash, change); err != nil {
			return err
		}
	}
	return nil
}

func (s *upperScanState) addChange(path, hash string, change gen.FsChangedPayloadChangesItemChangeType) error {
	if len(s.changes) >= s.limits.MaxChanges {
		return errors.New("collector: changed-file limit exceeded")
	}
	s.changes = append(s.changes, gen.FsChangedPayloadChangesItem{Path: path, Hash: hash, ChangeType: change})
	return nil
}

func isWhiteout(info os.FileInfo) bool {
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Rdev == 0
}

// withoutPath strips os.PathError's absolute path from diagnostics. Collector
// errors are allowed to identify a relative workspace entry, never the host
// filesystem location or a symlink target.
func withoutPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
