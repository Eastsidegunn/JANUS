package claudecode

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/world"
)

// T27 (f)-3: worldApprovalClient.Close and a concurrent poll re-dial. A dial
// that succeeds before Close cancels the context but registers its connection
// after Close's teardown snapshot escaped the snapshot; the poller then waited
// on the broker forever and Close hung in wg.Wait (CI PR #94).

const worldCloseBound = 2 * time.Second

func newWorldCloseClient(t *testing.T, broker *fakeWorldApprovalBroker, dial func(context.Context, string, string) (net.Conn, error)) *worldApprovalClient {
	t.Helper()
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ApprovalEndpoint: world.NewApprovalEndpoint("unix", broker.listener.Addr().String(), "capability"),
		WorldSpanID:      "2222222222222222",
	}
	return newWorldApprovalClient(&wireWriter{out: &countingLines{}, vals: vals}, cfg, dial)
}

func closeWithin(t *testing.T, c *worldApprovalClient, what string) {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		c.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(worldCloseBound):
		t.Fatalf("%s: world approval client Close did not return", what)
	}
}

// Deterministic order: dial succeeded -> Close took its teardown snapshot ->
// the dial registers. Close must still return.
func TestWorldApprovalClientCloseAfterDialBeforeRegisterReturns(t *testing.T) {
	broker := newFakeWorldApprovalBroker(t)
	t.Cleanup(broker.Close)
	dialed := make(chan struct{}, worldApprovalWorkers)
	release := make(chan struct{})
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		dialed <- struct{}{}
		<-release
		return conn, nil
	}
	c := newWorldCloseClient(t, broker, dial)
	for i := 0; i < worldApprovalWorkers; i++ {
		select {
		case <-dialed:
		case <-time.After(worldCloseBound):
			t.Fatalf("poller %d did not dial", i)
		}
	}
	closed := make(chan struct{})
	go func() {
		c.Close()
		close(closed)
	}()
	deadline := time.Now().Add(worldCloseBound)
	for {
		c.mu.Lock()
		snapshotTaken := c.closing
		c.mu.Unlock()
		if snapshotTaken {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not take its teardown snapshot")
		}
		time.Sleep(time.Millisecond)
	}
	// Every dial has succeeded and Close's snapshot holds none of them.
	close(release)
	select {
	case <-closed:
	case <-time.After(worldCloseBound):
		t.Fatal("dial registered after the teardown snapshot; Close hung")
	}
	c.mu.Lock()
	leaked := len(c.conns)
	c.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("%d connections registered after teardown", leaked)
	}
	if err := c.failure(); err != nil {
		t.Fatalf("Close teardown reported as broker failure: %v", err)
	}
}

// Stress: Close races the pollers' dial and registration with no ordering
// help. Run with -race -count=50; every Close must return.
func TestWorldApprovalClientCloseRacingPollDialNeverHangs(t *testing.T) {
	for i := 0; i < 200; i++ {
		broker := newFakeWorldApprovalBroker(t)
		c := newWorldCloseClient(t, broker, (&net.Dialer{}).DialContext)
		if i%2 == 1 {
			// Let some pollers reach the broker before Close starts.
			time.Sleep(time.Duration(i%7) * 50 * time.Microsecond)
		}
		closeWithin(t, c, "racing poll dial")
		if err := c.failure(); err != nil {
			t.Fatalf("iteration %d: Close teardown reported as broker failure: %v", i, err)
		}
		broker.Close()
	}
}
