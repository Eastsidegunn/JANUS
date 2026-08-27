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

func TestCommandProcessReadDeadlineInterruptsBlockedPipe(t *testing.T) {
	process, err := startCommand(context.Background(), "/bin/sh", "-c", "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		process.Kill()
		process.ClosePipes()
	}()
	deadline, ok := process.Stdout().(interface{ SetReadDeadline(time.Time) error })
	if !ok {
		t.Fatalf("stdout가 read deadline을 지원하지 않음: %T", process.Stdout())
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := process.Stdout().Read(make([]byte, 1))
		readDone <- err
	}()
	if err := deadline.SetReadDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		var netErr interface{ Timeout() bool }
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("blocked read deadline err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked pipe read가 deadline 뒤에도 종료되지 않음")
	}
}
