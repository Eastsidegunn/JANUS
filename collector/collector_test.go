package collector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

func testLimits() Limits {
	l := DefaultLimits()
	l.MaxNodes = 100
	l.MaxFileBytes = 1 << 20
	l.MaxTotalHashBytes = 4 << 20
	l.MaxManifestBytes = 1 << 20
	l.MaxPayloadBytes = 1 << 20
	l.MaxDepth = 16
	return l
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildBaselineAndDiffDeterministic(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()
	writeFile(t, lower, "a.txt", "old")
	writeFile(t, lower, "nested/delete.txt", "gone")
	if err := os.Symlink("a.txt", filepath.Join(lower, "link")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, upper, "z.txt", "new")
	writeFile(t, upper, "a.txt", "changed")
	if err := os.MkdirAll(filepath.Join(upper, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Portable representation used by unit tests; Linux native overlay xattr is
	// handled by opaqueDirectory and covered by the Linux integration gate.
	if err := os.WriteFile(filepath.Join(upper, "nested", ".wh..wh..opq"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	base, err := BuildBaseline(context.Background(), lower, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Diff(context.Background(), base, upper, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) != 3 {
		t.Fatalf("changes=%+v, want modified/added/deleted", got.Changes)
	}
	paths := make([]string, len(got.Changes))
	for i, c := range got.Changes {
		paths[i] = c.Path
	}
	if want := []string{"a.txt", "nested/delete.txt", "z.txt"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths=%v, want %v", paths, want)
	}
	if got.Changes[0].ChangeType != gen.FsChangedPayloadChangesItemChangeTypeModified || got.Changes[2].ChangeType != gen.FsChangedPayloadChangesItemChangeTypeAdded {
		t.Fatalf("change ordering/types=%+v", got.Changes)
	}
	if got.Changes[1].Hash != base.entries["nested/delete.txt"].hash {
		t.Fatalf("deleted hash=%q, baseline=%q", got.Changes[1].Hash, base.entries["nested/delete.txt"].hash)
	}
	if got.Changes[0].Hash == got.Changes[2].Hash {
		t.Fatalf("unexpected equal hashes for changed and added files")
	}

	again, err := Diff(context.Background(), base, upper, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("diff is not deterministic:\n%+v\n%+v", got, again)
	}
}

func TestDiffRejectsLowerMutationBeforeUpperAndReturnsNoPartial(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	writeFile(t, lower, "a", "before")
	writeFile(t, upper, "new", "upper")
	base, err := BuildBaseline(context.Background(), lower, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, lower, "a", "after")
	got, err := Diff(context.Background(), base, upper, testLimits())
	if err == nil || !strings.Contains(err.Error(), "lower workspace changed") {
		t.Fatalf("err=%v, want lower mutation failure", err)
	}
	if len(got.Changes) != 0 {
		t.Fatalf("partial changes leaked: %+v", got.Changes)
	}
}

func TestDiffExpandsDirectoryReplacementAndOpaqueLeaves(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	writeFile(t, lower, "dir/a.txt", "a")
	writeFile(t, lower, "dir/link-target.txt", "target")
	if err := os.Symlink("link-target.txt", filepath.Join(lower, "dir", "link.txt")); err != nil {
		t.Fatal(err)
	}
	base, err := BuildBaseline(context.Background(), lower, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	// A lower directory replaced by a file has no directory event. Its
	// baseline leaves are deleted and the replacement leaf is added.
	if err := os.WriteFile(filepath.Join(upper, "dir"), []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Diff(context.Background(), base, upper, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) != 4 {
		t.Fatalf("changes=%+v, want replacement plus three deleted leaves", got.Changes)
	}
	byPath := make(map[string]gen.FsChangedPayloadChangesItem)
	for _, change := range got.Changes {
		byPath[change.Path] = change
	}
	if byPath["dir"].ChangeType != gen.FsChangedPayloadChangesItemChangeTypeAdded {
		t.Fatalf("replacement=%+v, want added", byPath["dir"])
	}
	for _, path := range []string{"dir/a.txt", "dir/link-target.txt", "dir/link.txt"} {
		change, ok := byPath[path]
		if !ok || change.ChangeType != gen.FsChangedPayloadChangesItemChangeTypeDeleted {
			t.Fatalf("path %q change=%+v, want deleted", path, change)
		}
		if change.Hash != base.entries[path].hash {
			t.Fatalf("path %q hash=%q, want baseline %q", path, change.Hash, base.entries[path].hash)
		}
	}
}

func TestExpandOpaqueDeduplicatesDirectoryWhiteoutLeaves(t *testing.T) {
	s := upperScanState{
		baseline: map[string]manifestEntry{
			"dir":       {kind: "dir"},
			"dir/a.txt": {kind: "regular", hash: "sha256:a"},
			"dir/link":  {kind: "symlink", hash: "sha256:b"},
			"dir/sub":   {kind: "dir"},
		},
		seen:      map[string]bool{},
		emitted:   map[string]bool{"dir/link": true},
		changes:   []gen.FsChangedPayloadChangesItem{{Path: "dir/link", Hash: "sha256:b", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeDeleted}},
		opaque:    []string{"dir", "dir"}, // native directory whiteout may be observed once per path
		scanState: scanState{limits: testLimits()},
	}
	if err := s.expandOpaque(); err != nil {
		t.Fatal(err)
	}
	if len(s.changes) != 2 || s.changes[1].Path != "dir/a.txt" {
		t.Fatalf("changes=%+v, want one new deleted leaf without duplicates", s.changes)
	}
}

func TestCollectorLimitsFailClosed(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	writeFile(t, lower, "base", "x")
	writeFile(t, upper, "a", "a")
	writeFile(t, upper, "b", "b")
	base, err := BuildBaseline(context.Background(), lower, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	l := testLimits()
	l.MaxChanges = 1
	got, err := Diff(context.Background(), base, upper, l)
	if err == nil || !strings.Contains(err.Error(), "changed-file limit") {
		t.Fatalf("err=%v, want changed-file limit", err)
	}
	if len(got.Changes) != 0 {
		t.Fatalf("limit failure returned partial result: %+v", got.Changes)
	}

	l = testLimits()
	l.MaxPayloadBytes = 1
	got, err = Diff(context.Background(), base, upper, l)
	if err == nil || !strings.Contains(err.Error(), "payload exceeds") {
		t.Fatalf("err=%v, want payload limit", err)
	}
	if len(got.Changes) != 0 {
		t.Fatalf("payload failure returned partial result: %+v", got.Changes)
	}
}

func TestBuildBaselineRejectsRelativeAndUnsupportedRoots(t *testing.T) {
	if _, err := BuildBaseline(context.Background(), "relative", testLimits()); err == nil {
		t.Fatal("relative root accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildBaseline(context.Background(), file, testLimits()); err == nil {
		t.Fatal("regular file accepted as root")
	}
}

func TestEgressPayloadAndRecords(t *testing.T) {
	p, err := EgressPayload(EgressAttempt{Domain: "example.com", Method: "GET", SizeBytes: 4, AtMs: 9, Decision: "allow"})
	if err != nil || p.Decision != gen.EgressPayloadDecisionAllow || p.Reason != nil {
		t.Fatalf("allow payload=%+v err=%v", p, err)
	}
	p, err = EgressPayload(EgressAttempt{Domain: "blocked.example", Method: "CONNECT", Decision: "deny", Reason: "not allowed"})
	if err != nil || p.Reason == nil || *p.Reason != "not allowed" {
		t.Fatalf("deny payload=%+v err=%v", p, err)
	}
	for name, attempt := range map[string]EgressAttempt{
		"allow reason":        {Domain: "x", Method: "GET", Decision: "allow", Reason: "why"},
		"deny missing reason": {Domain: "x", Method: "GET", Decision: "deny"},
		"unknown decision":    {Domain: "x", Method: "GET", Decision: "maybe"},
		"negative size":       {Domain: "x", Method: "GET", Decision: "allow", SizeBytes: -1},
		"sensitive reason":    {Domain: "x", Method: "GET", Decision: "deny", Reason: "header leaked"},
	} {
		if _, err := EgressPayload(attempt); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	rec, err := NewEgressRecord("a trace", "child", EgressAttempt{Domain: "example.com", Method: "GET", Decision: "allow"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Actor != "collector" || rec.Kind != gen.KindCollectorEgress || rec.Raw == nil || *rec.Raw != "" {
		t.Fatalf("record envelope=%+v", rec)
	}
	var decoded gen.EgressPayload
	if err := json.Unmarshal(rec.Payload, &decoded); err != nil || decoded.Decision != gen.EgressPayloadDecisionAllow {
		t.Fatalf("record payload=%s err=%v", rec.Payload, err)
	}
}
