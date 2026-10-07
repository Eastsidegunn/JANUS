package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/runtimedir"
)

// T27 (f) defect 2: begin's readiness gate must not be a Go select coin flip.
// The two tests below are a pair: the first pins that an already-ready state
// always wins over a closed done; the second pins that this is determinism,
// not a widening — without ready, a closed done still always denies.

const beginRepetitions = 1000

type countingLines struct {
	mu    sync.Mutex
	lines [][]byte
}

func (c *countingLines) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, append([]byte(nil), p...))
	return len(p), nil
}

func (c *countingLines) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.lines...)
}

func newBeginState(t *testing.T) (*approvalServer, *countingLines) {
	t.Helper()
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	out := &countingLines{}
	return newApprovalState(&wireWriter{out: out, vals: vals}), out
}

func beginHookInput(i int) []byte {
	return []byte(fmt.Sprintf(`{"hook_event_name":"PreToolUse","tool_use_id":"toolu_begin_%04d","tool_name":"Bash","tool_input":{"command":"true"}}`, i))
}

func TestApprovalBeginReadyAndDoneClosedAlwaysProceeds(t *testing.T) {
	s, out := newBeginState(t)
	s.markReady()
	s.doneOnce.Do(func() { close(s.done) })
	for i := 0; i < beginRepetitions; i++ {
		requestID, pending, err := s.begin(beginHookInput(i), "", nil)
		if err != nil {
			t.Fatalf("iteration %d: ready already observed but begin failed: %v", i, err)
		}
		if requestID == "" || pending == nil {
			t.Fatalf("iteration %d: begin returned request=%q pending=%v", i, requestID, pending)
		}
		s.complete(requestID)
	}
	lines := out.snapshot()
	if len(lines) != beginRepetitions {
		t.Fatalf("approval_request count=%d want %d", len(lines), beginRepetitions)
	}
	for i, line := range lines {
		var event gen.Event
		if err := json.Unmarshal(bytes.TrimSpace(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind != gen.EventKindSubagentApprovalRequest {
			t.Fatalf("line %d kind=%s", i, event.Kind)
		}
	}
}

func TestApprovalBeginDoneWithoutReadyAlwaysDenies(t *testing.T) {
	s, out := newBeginState(t)
	s.doneOnce.Do(func() { close(s.done) })
	for i := 0; i < beginRepetitions; i++ {
		requestID, pending, err := s.begin(beginHookInput(i), "", nil)
		if err == nil || !strings.Contains(err.Error(), "ready 전 Claude 종료") {
			t.Fatalf("iteration %d: done without ready must deny, got request=%q pending=%v err=%v", i, requestID, pending, err)
		}
		if requestID != "" || pending != nil {
			t.Fatalf("iteration %d: denied begin leaked request=%q pending=%v", i, requestID, pending)
		}
	}
	if lines := out.snapshot(); len(lines) != 0 {
		t.Fatalf("denied begin emitted %d approval_request events", len(lines))
	}
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	if pending != 0 {
		t.Fatalf("denied begin registered %d pending approvals", pending)
	}
}

func TestApprovalServerUsesHXRuntimeDir(t *testing.T) {
	runtimeRoot, err := os.MkdirTemp("/tmp", "hxd6-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	t.Setenv("HX_RUNTIME_DIR", runtimeRoot)
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	server, err := newApprovalServer(&wireWriter{out: io.Discard, vals: vals})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if !strings.HasPrefix(server.dir, runtimeRoot+string(os.PathSeparator)) {
		t.Fatalf("approval socket root=%q is outside HX_RUNTIME_DIR=%q", server.dir, runtimeRoot)
	}
	const prefix = "hxapprove-"
	if got := len(server.path) - len(runtimeRoot) - 1; got-len(filepath.Base(server.dir))+len(prefix)+10 > runtimedir.MaxSocketSuffixBytes {
		t.Fatalf("approval socket suffix=%d exceeds %d", got, runtimedir.MaxSocketSuffixBytes)
	}
}
