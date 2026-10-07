package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

func dumpTestProfile(id string) policy.Profile {
	return policy.Profile{
		ID:                id,
		FSScope:           []string{"/workspace", "/data"},
		Egress:            []string{"z.example", "a.example", "token=super-secret"},
		AllowedExtensions: []string{"z-ext", "a-ext"},
		AllowedRegistries: []string{"Registry.Example."},
		Budget:            policyBudget(100, 200, 3),
		Approval:          policy.ApprovalManual,
	}
}

func policyBudget(tokens, timeMs, depth int64) gen.Budget {
	return gen.Budget{Tokens: tokens, TimeMs: timeMs, MaxDepth: depth}
}

func TestDumpConfigRedactsSensitiveValues(t *testing.T) {
	base := dumpTestProfile("token=super-secret")
	got, err := renderDumpConfig(base, nil, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("<redacted>")) {
		t.Fatalf("redaction marker missing: %s", got)
	}
	if bytes.Contains(got, []byte("super-secret")) {
		t.Fatalf("secret value leaked: %s", got)
	}
	for _, hostOnly := range []string{"/host/upper", "/var/cache", "/run/hx/approve.sock", "Podman"} {
		if bytes.Contains(got, []byte(hostOnly)) {
			t.Fatalf("host-only value %q leaked: %s", hostOnly, got)
		}
	}
}

func TestDumpConfigIsDeterministicAndUsesPolicyMerge(t *testing.T) {
	base := dumpTestProfile("base")
	overlay := dumpTestProfile("overlay")
	overlay.FSScope = []string{"/workspace"}
	first, err := renderDumpConfig(base, []policy.Profile{overlay}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := renderDumpConfig(base, []policy.Profile{overlay}, "/workspace")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("반복 %d에서 dump-config 바이트가 달라짐", i)
		}
	}
	if !bytes.Contains(first, []byte(`"profile_id": "base+overlay"`)) {
		t.Fatalf("policy.Merge 계보가 출력되지 않음: %s", first)
	}
	if !bytes.Contains(first, []byte(`"fs_scope": [
    "/workspace"
  ]`)) {
		t.Fatalf("교집합 fs scope가 출력되지 않음: %s", first)
	}
	if strings.Index(string(first), "a.example") > strings.Index(string(first), "z.example") {
		t.Fatalf("egress가 결정적으로 정렬되지 않음: %s", first)
	}
}

func TestDumpConfigRendererSortsLists(t *testing.T) {
	got := sortedRedacted([]string{"z.example", "a.example", "m.example"})
	want := []string{"a.example", "m.example", "z.example"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("renderer list order=%v, want %v", got, want)
	}
}

func TestDumpConfigEvaluationFailureHasNoOutput(t *testing.T) {
	base := dumpTestProfile("invalid-workspace")
	got, err := renderDumpConfig(base, nil, "/outside")
	if err == nil {
		t.Fatal("policy.Evaluate denial이 성공으로 처리됨")
	}
	if len(got) != 0 {
		t.Fatalf("오류 평가가 stdout 후보를 생성함: %s", got)
	}
}

func TestDumpConfigCommandErrorsBeforeStdout(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(profile, []byte("id: p\nunknown: leaked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = w
	cmdErr := dumpConfigCmd([]string{"--profile", profile})
	_ = w.Close()
	os.Stdout = original
	out, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if cmdErr == nil {
		t.Fatal("invalid profile unexpectedly succeeded")
	}
	if len(out) != 0 {
		t.Fatalf("invalid profile wrote stdout: %q", out)
	}
}
