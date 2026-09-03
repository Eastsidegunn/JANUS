package observe

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	testTrace = "11111111111111111111111111111111"
	testRoot  = "2222222222222222"
	testChild = "3333333333333333"
)

func fixtureSnapshot() []gen.EventRecord {
	parent, profile, image, raw := testRoot, "sandbox", "sha256:"+strings.Repeat("a", 64), ""
	spawn, _ := json.Marshal(gen.SubagentSpawnPayload{
		Adapter: "codex", Instruction: "test", Budget: gen.SpawnBudget{Tokens: 10, TimeMs: 20, MaxDepth: 2},
		WorldBackend: gen.SubagentSpawnPayloadWorldBackendLocalPodman, ProfileID: &profile, ImageDigest: &image,
		Mounts: []gen.SubagentSpawnMount{{SourcePath: "/tmp/work", TargetPath: gen.SubagentSpawnMountTargetPathWorkspace, Mode: gen.SubagentSpawnMountModeOverlay, UpperRef: "u"}},
	})
	done, _ := json.Marshal(gen.SubagentDonePayload{Status: gen.SubagentDonePayloadStatusOk, Result: "done"})
	in, out := int64(7), int64(11)
	return []gen.EventRecord{
		{Seq: 1, Ts: 1000, TraceID: testTrace, SpanID: testRoot, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`), Raw: &raw},
		{Seq: 2, Ts: 1010, TraceID: testTrace, SpanID: testChild, ParentSpanID: &parent, Kind: gen.KindSubagentSpawn, Actor: "parent", Payload: spawn, Raw: &raw},
		{Seq: 3, Ts: 1020, TraceID: testTrace, SpanID: testChild, ParentSpanID: &parent, Kind: gen.KindSubagentUsage, Actor: "subagent:codex:1", Payload: json.RawMessage(`{"input_tokens":7,"output_tokens":11}`), Raw: &raw, UsageIn: &in, UsageOut: &out},
		{Seq: 4, Ts: 1030, TraceID: testTrace, SpanID: testChild, ParentSpanID: &parent, Kind: gen.KindSubagentDone, Actor: "subagent:codex:1", Payload: done, Raw: &raw},
		{Seq: 5, Ts: 1040, TraceID: testTrace, SpanID: testRoot, Kind: gen.KindSessionEnd, Actor: "parent", Payload: json.RawMessage(`{}`), Raw: &raw},
	}
}

func TestProjectPreservesIDsParentAttributesAndIsDeterministic(t *testing.T) {
	events := fixtureSnapshot()
	want, err := Project(events, AdapterVersions{"codex": "0.147.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 {
		t.Fatalf("spans=%d", len(want))
	}
	var child SpanData
	for _, span := range want {
		if span.Context.SpanID().String() == testChild {
			child = span
		}
	}
	if child.Context.TraceID().String() != testTrace || child.Context.SpanID().String() != testChild || child.Parent.SpanID().String() != testRoot {
		t.Fatalf("ID/parent transformed: %+v", child)
	}
	got := map[string]any{}
	for _, kv := range child.Attributes {
		got[string(kv.Key)] = kv.Value.AsInterface()
	}
	for key, value := range map[string]any{"hx.adapter.name": "codex", "hx.adapter.version": "0.147.0", "hx.profile.id": "sandbox", "hx.status": "ok", "hx.usage.input_tokens": int64(7), "hx.usage.output_tokens": int64(11)} {
		if !reflect.DeepEqual(got[key], value) {
			t.Fatalf("attribute %s=%v want=%v", key, got[key], value)
		}
	}
	for i := 0; i < 100; i++ {
		again, err := Project(events, AdapterVersions{"codex": "0.147.0"})
		if err != nil || !reflect.DeepEqual(again, want) {
			t.Fatalf("iteration %d non-deterministic: %v", i, err)
		}
	}
}

func TestProjectRejectsMissingVersionProfileAndUsageOverflow(t *testing.T) {
	if _, err := Project(fixtureSnapshot(), nil); err == nil || !strings.Contains(err.Error(), "version 누락") {
		t.Fatalf("missing version=%v", err)
	}
	events := fixtureSnapshot()
	var payload gen.SubagentSpawnPayload
	_ = json.Unmarshal(events[1].Payload, &payload)
	payload.ProfileID = nil
	events[1].Payload, _ = json.Marshal(payload)
	if _, err := Project(events, AdapterVersions{"codex": "0.147.0"}); err == nil || !strings.Contains(err.Error(), "profile_id 누락") {
		t.Fatalf("missing profile=%v", err)
	}
	events = fixtureSnapshot()
	max, one := int64(math.MaxInt64), int64(1)
	events[2].UsageIn = &max
	extra := events[2]
	extra.Seq = 4
	extra.Ts = 1025
	extra.UsageIn = &one
	events = append(events[:3], append([]gen.EventRecord{extra}, events[3:]...)...)
	events[4].Seq = 5
	events[5].Seq = 6
	if _, err := Project(events, AdapterVersions{"codex": "0.147.0"}); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("usage overflow=%v", err)
	}
}

type failingExporter struct {
	err   error
	calls int
	seen  []exportedIdentity
}

type exportedIdentity struct {
	traceID, spanID, parentID string
}

func (e *failingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, span := range spans {
		e.calls++
		e.seen = append(e.seen, exportedIdentity{
			traceID:  span.SpanContext().TraceID().String(),
			spanID:   span.SpanContext().SpanID().String(),
			parentID: span.Parent().SpanID().String(),
		})
	}
	return e.err
}
func (e *failingExporter) Shutdown(context.Context) error { return nil }

type memoryStore struct {
	mu     sync.Mutex
	events []gen.EventRecord
}

func (s *memoryStore) LastSeq(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return 0, nil
	}
	return s.events[len(s.events)-1].Seq, nil
}

func (s *memoryStore) ReadFrom(_ context.Context, from int64) ([]gen.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var events []gen.EventRecord
	for _, event := range s.events {
		if event.Seq >= from {
			events = append(events, event)
		}
	}
	return events, nil
}

func (s *memoryStore) Append(_ context.Context, event gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *memoryStore) AppendBatch(_ context.Context, events []gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

func (*memoryStore) Close() error { return nil }

func TestExportFailureDoesNotBlockAppendOrReplayAndNilIsNoop(t *testing.T) {
	events := fixtureSnapshot()
	spans, err := Project(events, AdapterVersions{"codex": "0.147.0"})
	if err != nil {
		t.Fatal(err)
	}
	ExportBestEffort(context.Background(), nil, spans, func(error) { t.Fatal("noop reported") })
	wantErr := errors.New("receiver unavailable")
	exporter := &failingExporter{err: wantErr}
	var reported error
	ExportBestEffort(context.Background(), exporter, spans, func(err error) { reported = err })
	if exporter.calls != len(spans) || !errors.Is(reported, wantErr) {
		t.Fatalf("calls=%d report=%v", exporter.calls, reported)
	}
	if !reflect.DeepEqual(exporter.seen, []exportedIdentity{
		{traceID: testTrace, spanID: testRoot, parentID: "0000000000000000"},
		{traceID: testTrace, spanID: testChild, parentID: testRoot},
	}) {
		t.Fatalf("exported IDs transformed: %+v", exporter.seen)
	}

	// Exercise the real writer admission/ACK and Replay path after the failed
	// export. Export is not allowed to retain a writer lock or turn its error
	// into a log terminal state.
	store := &memoryStore{}
	writer, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.InitBatch(context.Background(), events[:len(events)-1]); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Submit(context.Background(), events[len(events)-1]); err != nil {
		t.Fatalf("append after export failure: %v", err)
	}
	if _, err := writer.Replay(context.Background()); err != nil {
		t.Fatalf("replay after export failure: %v", err)
	}
}
