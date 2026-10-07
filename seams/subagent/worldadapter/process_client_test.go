package worldadapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/Eastsidegunn/JANUS/core/world/processwire"
	"io"
	"net"
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

func TestStartPreservesTaskStdinAndNoStdinStartOmitsData(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			client, broker := net.Pipe()
			defer client.Close()
			defer broker.Close()
			c := &processClient{control: client, enc: processwire.NewEncoder(client), pending: map[uint64]chan error{}, done: make(chan struct{})}
			go c.readControl(processwire.NewDecoder(client))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = broker.SetDeadline(time.Now().Add(time.Second))
			task := []byte(`{"cmd":"task","payload":{"instruction":"hello"}}`)
			result := make(chan error, 1)
			go func() {
				if empty {
					result <- c.StartWithoutStdin(ctx)
				} else {
					result <- c.Start(ctx, task)
				}
			}()
			kinds := []processwire.Kind{processwire.KindStart, processwire.KindStdinData, processwire.KindStdinClose, processwire.KindWait}
			if empty {
				kinds = []processwire.Kind{processwire.KindStart, processwire.KindStdinClose, processwire.KindWait}
			}
			dec, enc := processwire.NewDecoder(broker), processwire.NewEncoder(broker)
			for _, kind := range kinds {
				frame, err := dec.Read()
				if err != nil {
					t.Fatal(err)
				}
				var want []byte
				if kind == processwire.KindStdinData {
					want = append(append([]byte(nil), task...), '\n')
				}
				if frame.Kind != kind || !bytes.Equal(frame.Payload, want) {
					t.Fatalf("frame=%+v want kind=%d payload=%q", frame, kind, want)
				}
				ack, _ := processwire.Marshal(processwire.Ack{RequestSeq: frame.Seq})
				if _, err := enc.Write(processwire.KindAck, processwire.StreamControl, 0, ack); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}
