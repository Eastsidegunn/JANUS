package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/seams/approvalrelay"
)

func leaveStaleUnixSocket(t *testing.T, endpoint string) {
	t.Helper()
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		_ = listener.Close()
		t.Fatalf("listener type = %T", listener)
	}
	unixListener.SetUnlinkOnClose(false)
	if err := unixListener.Close(); err != nil {
		t.Fatal(err)
	}
}

func shortApprovalSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hxt31-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "approval.sock")
}

func TestApprovalEndpointStaleSocketIsRemovedAndRelayServes(t *testing.T) {
	endpoint := shortApprovalSocket(t)
	leaveStaleUnixSocket(t, endpoint)
	var stderr bytes.Buffer
	decider, closer, _, err := selectApprovalDecider(endpoint, "trace", "policy", 1000, approvalrelay.SessionControl{}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if got := stderr.String(); got != "hx: removed stale approval endpoint socket\n" {
		t.Fatalf("stale diagnostic = %q", got)
	}

	decisionCh := make(chan policy.ApprovalDecision, 1)
	go func() {
		decision, _ := decider.Decide(context.Background(), policy.ApprovalRequest{
			RequestID: "request", SpanID: "span", Args: []byte(`{"command":"true"}`),
		})
		decisionCh <- decision
	}()
	deadline := time.Now().Add(time.Second)
	for {
		result := relayRoundTrip(t, endpoint, approvalrelay.Message{
			Op: "query", TraceID: "trace", SpanID: "span", RequestID: "request",
		})
		if result.Status == "pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("approval request did not become pending: %+v", result)
		}
		time.Sleep(time.Millisecond)
	}
	result := relayRoundTrip(t, endpoint, approvalrelay.Message{
		Op: "submit", TraceID: "trace", SpanID: "span", RequestID: "request",
		ResponseID: "response", Decision: "allow",
	})
	if result.Status != "decided" || !(<-decisionCh).Allow {
		t.Fatalf("approval relay did not serve after stale cleanup: %+v", result)
	}
}

func TestApprovalEndpointLiveListenerRejectedBeforeClaim(t *testing.T) {
	endpoint := shortApprovalSocket(t)
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	if _, _, _, err := selectApprovalDecider(endpoint, "trace", "policy", 1000, approvalrelay.SessionControl{}, io.Discard); err == nil || !strings.Contains(err.Error(), "another hx is serving this endpoint") {
		t.Fatalf("live endpoint was not rejected: %v", err)
	}

	fixture := newProductionFixture(t)
	requestBytes, err := json.Marshal(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err = runProduction(context.Background(), productionRun{
		RequestBytes: requestBytes, ProfilePath: fixture.profilePath, AcceptRoot: fixture.acceptRoot,
		Launcher: fixture.launcher, Stdout: &stdout, ApprovalEndpoint: endpoint,
	})
	if err == nil || !strings.Contains(err.Error(), "another hx is serving this endpoint") {
		t.Fatalf("production live endpoint rejection = %v", err)
	}
	if fixture.launcher.calls.Load() != 0 {
		t.Fatalf("launcher called %d times", fixture.launcher.calls.Load())
	}
	if _, statErr := os.Stat(fixture.acceptRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("claim root was created before endpoint rejection: %v", statErr)
	}
}

func TestApprovalEndpointBindRaceReturnsListenError(t *testing.T) {
	endpoint := shortApprovalSocket(t)
	server, err := approvalrelay.NewServer(endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := startApprovalServer(server); err == nil {
		t.Fatal("endpoint path existence hid the Listen failure")
	}
}

func TestApprovalEndpointRegularFileRejected(t *testing.T) {
	endpoint := shortApprovalSocket(t)
	if err := os.WriteFile(endpoint, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := prepareApprovalEndpoint(endpoint, io.Discard)
	if err == nil || err.Error() != "endpoint path exists and is not a socket" {
		t.Fatalf("regular endpoint error = %v", err)
	}
}

func TestApprovalEndpointLockRejectsSecondStartup(t *testing.T) {
	endpoint := shortApprovalSocket(t)
	lock, err := lockAndPrepareApprovalEndpoint(endpoint, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	_, _, _, err = selectApprovalDecider(endpoint, "trace", "policy", 1000, approvalrelay.SessionControl{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "lock is held by another hx") {
		t.Fatalf("second startup lock error = %v", err)
	}
}

func TestApprovalEndpointUnsafeDirectoryRejectedBeforeClaim(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(dir, "approval.sock")
	fixture := newProductionFixture(t)
	requestBytes, err := json.Marshal(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	err = runProduction(context.Background(), productionRun{
		RequestBytes: requestBytes, ProfilePath: fixture.profilePath, AcceptRoot: fixture.acceptRoot,
		Launcher: fixture.launcher, Stdout: io.Discard, ApprovalEndpoint: endpoint,
	})
	if err == nil || !strings.Contains(err.Error(), "socket directory must be mode 0700") {
		t.Fatalf("unsafe endpoint directory error = %v", err)
	}
	if fixture.launcher.calls.Load() != 0 {
		t.Fatalf("launcher called %d times", fixture.launcher.calls.Load())
	}
	if _, statErr := os.Stat(fixture.acceptRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("claim root was created before directory rejection: %v", statErr)
	}
	if _, statErr := os.Lstat(endpoint + ".lock"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("lock file was created before directory rejection: %v", statErr)
	}
}

func TestApprovalEndpointLockRejectsSymlink(t *testing.T) {
	endpoint := shortApprovalSocket(t)
	target := filepath.Join(filepath.Dir(endpoint), "target.lock")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, endpoint+".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := lockAndPrepareApprovalEndpoint(endpoint, io.Discard); err == nil {
		t.Fatal("symlink lock file was followed")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("symlink target changed: %q", data)
	}
}

func relayRoundTrip(t *testing.T, endpoint string, message approvalrelay.Message) approvalrelay.Result {
	t.Helper()
	conn, err := net.DialTimeout("unix", endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(message); err != nil {
		t.Fatal(err)
	}
	var result approvalrelay.Result
	if err := json.NewDecoder(conn).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
