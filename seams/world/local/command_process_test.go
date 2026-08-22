package local

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestCommandProcessDoneDoesNotDependOnDescendantHeldOutput(t *testing.T) {
	process, err := startCommand(context.Background(), "/bin/sh", "-c", "sleep 5 & printf ready; exit 0")
	if err != nil {
		t.Fatal(err)
	}
	defer process.ClosePipes()
	select {
	case <-process.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("leader exit가 descendant-held output EOF에 인질 잡힘")
	}
	data, err := io.ReadAll(process.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ready" {
		t.Fatalf("drained output=%q", data)
	}
}

func TestCommandProcessPipesClosePrecisely(t *testing.T) {
	process, err := startCommand(context.Background(), "/bin/sh", "-c", "exit 0")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("process done timeout")
	}
	process.ClosePipes()
	if _, err := process.Stdin().Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stdin err=%v", err)
	}
	if _, err := process.Stdout().Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stdout err=%v", err)
	}
	process.ClosePipes() // idempotent
}
