package worldadapter

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestAwaitResponsePreservesAckBeforeTerminalClose(t *testing.T) {
	c := &processClient{done: make(chan struct{})}
	close(c.done)
	for range 100 {
		if err := c.awaitResponse(context.Background(), responseWith(nil)); err != nil {
			t.Fatalf("terminal close overtook ACK: %v", err)
		}
	}
}

func TestAwaitResponseWaitsForAckAfterExitObserved(t *testing.T) {
	c := &processClient{done: make(chan struct{}), exitSet: true}
	response := make(chan error, 1)
	close(c.done)
	go func() {
		time.Sleep(10 * time.Millisecond)
		response <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.awaitResponse(ctx, response); err != nil {
		t.Fatalf("exit_observed overtook later ACK: %v", err)
	}
}

func TestAwaitResponseReturnsTerminalFailureWithoutAck(t *testing.T) {
	c := &processClient{done: make(chan struct{}), err: io.EOF}
	close(c.done)
	err := c.awaitResponse(context.Background(), make(chan error))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("terminal failure=%v", err)
	}
}

func responseWith(err error) <-chan error {
	response := make(chan error, 1)
	response <- err
	return response
}
