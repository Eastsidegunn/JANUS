package buildinfo

import (
	"regexp"
	"runtime"
	"testing"
)

func TestLineDefaults(t *testing.T) {
	oldV, oldC := version, commit
	defer func() { version, commit = oldV, oldC }()
	version, commit = "devel", ""
	got := Line("hx")
	pattern := `^hx devel \(commit ([0-9a-f]{7,40}(-dirty)?|unknown), ` + regexp.QuoteMeta(runtimeVersion()) + `\)$`
	if !regexp.MustCompile(pattern).MatchString(got) {
		t.Fatalf("unexpected line %q", got)
	}
}

func TestFormatLineInjectedCommitNeverDirty(t *testing.T) {
	got := formatLine("hx", "devel", "deadbeef", false)
	if got != "hx devel (commit deadbeef, "+runtimeVersion()+")" {
		t.Fatalf("%q", got)
	}
}

func TestFormatLineVCSModifiedAddsDirty(t *testing.T) {
	got := formatLine("hx", "devel", "deadbeef", true)
	if got != "hx devel (commit deadbeef-dirty, "+runtimeVersion()+")" {
		t.Fatalf("%q", got)
	}
}

func TestLineInjected(t *testing.T) {
	oldV, oldC := version, commit
	defer func() { version, commit = oldV, oldC }()
	version, commit = "9.9.9-test", "deadbeef"
	if got := Line("hx"); got != "hx 9.9.9-test (commit deadbeef, "+runtimeVersion()+")" {
		t.Fatalf("%q", got)
	}
}

func runtimeVersion() string { return runtime.Version() }
