package procgroup

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoneDoesNotDependOnStdoutEOF(t *testing.T) {
	p, err := Start(context.Background(), Options{
		Command: []string{"/bin/sh", "-c", "sleep 5 & exit 0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(time.Second):
		t.Fatal("leader exit observation waited for descendant-held stdout EOF")
	}
	if err := p.ExitErr(); err != nil {
		t.Fatalf("leader exit: %v", err)
	}
	result := p.DrainLines(4*1024*1024, func([]byte) error { return nil })
	if result.HandlerErr != nil || result.ScanErr != nil || result.ExitErr != nil {
		t.Fatalf("drain result: %+v", result)
	}
}

type overlapWriter struct {
	active  atomic.Int32
	overlap atomic.Bool
	mu      sync.Mutex
	writes  []string
}

func (w *overlapWriter) Write(b []byte) (int, error) {
	if w.active.Add(1) != 1 {
		w.overlap.Store(true)
	}
	time.Sleep(time.Millisecond)
	w.mu.Lock()
	w.writes = append(w.writes, string(b))
	w.mu.Unlock()
	w.active.Add(-1)
	return len(b), nil
}

func (w *overlapWriter) Close() error { return nil }

func TestWriteLineSerializesSingleStdin(t *testing.T) {
	w := &overlapWriter{}
	p := &Process{stdin: w}
	const count = 20
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.WriteLine([]byte(`{"cmd":"message"}`)); err != nil {
				t.Errorf("WriteLine: %v", err)
			}
		}()
	}
	wg.Wait()
	if w.overlap.Load() {
		t.Fatal("concurrent writes reached the same stdin writer")
	}
	if len(w.writes) != count {
		t.Fatalf("writes = %d, want %d", len(w.writes), count)
	}
	for _, got := range w.writes {
		if !strings.HasSuffix(got, "\n") || strings.Count(got, "\n") != 1 {
			t.Fatalf("line delimiter mismatch: %q", got)
		}
	}
}
