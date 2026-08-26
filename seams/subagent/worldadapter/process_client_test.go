package worldadapter

import (
	"context"
	"errors"
	"io"
	"testing"
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
