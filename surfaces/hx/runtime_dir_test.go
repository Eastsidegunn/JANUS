package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCmdRejectsInvalidRuntimeDirBeforeSessionStart(t *testing.T) {
	t.Setenv("HX_RUNTIME_DIR", filepath.Join(t.TempDir(), "missing"))
	session := filepath.Join(t.TempDir(), "session.db")
	err := runCmd([]string{"--session", session, "--adapter", "/does/not/exist", "hello"})
	if err == nil || !strings.Contains(err.Error(), "HX_RUNTIME_DIR") {
		t.Fatalf("invalid runtime directory error=%v", err)
	}
	if _, statErr := os.Stat(session); !os.IsNotExist(statErr) {
		t.Fatalf("session was created before runtime validation: stat=%v", statErr)
	}
}

func TestRunProductionCmdRejectsInvalidRuntimeDirBeforeClaim(t *testing.T) {
	root := t.TempDir()
	acceptRoot := filepath.Join(root, "accept")
	t.Setenv("HX_RUNTIME_DIR", filepath.Join(root, "missing-runtime"))
	err := runProductionCmd(
		filepath.Join(root, "missing-request.json"),
		filepath.Join(root, "missing-profile.yaml"),
		nil,
		acceptRoot,
		filepath.Join(root, "missing-world.json"),
		"", "",
	)
	if err == nil || !strings.Contains(err.Error(), "HX_RUNTIME_DIR") {
		t.Fatalf("invalid runtime directory error=%v", err)
	}
	if _, statErr := os.Stat(acceptRoot); !os.IsNotExist(statErr) {
		t.Fatalf("accept root was created before runtime validation: stat=%v", statErr)
	}
}
