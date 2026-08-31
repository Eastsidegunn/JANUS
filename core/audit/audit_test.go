package audit

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

func TestCanonicalPath(t *testing.T) {
	tests := []struct {
		name, raw, want string
		ok              bool
	}{
		{name: "absolute workspace", raw: "/workspace/a/../b.txt", want: "b.txt", ok: true},
		{name: "relative", raw: "a/./b.txt", want: "a/b.txt", ok: true},
		{name: "workspace boundary", raw: "/workspace-evil/x", ok: false},
		{name: "outside", raw: "/workspace/../../etc/passwd", ok: false},
		{name: "absolute outside", raw: "/etc/passwd", ok: false},
		{name: "root", raw: "/workspace", ok: false},
		{name: "empty", raw: "", ok: false},
		{name: "nul", raw: "a\x00b", ok: false},
		{name: "non POSIX separator", raw: `a\\b`, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CanonicalPath(tt.raw, "/workspace")
			if tt.ok {
				if err != nil || got != tt.want {
					t.Fatalf("CanonicalPath=%q, %v; want %q", got, err, tt.want)
				}
			} else if err == nil {
				t.Fatalf("CanonicalPath(%q) unexpectedly succeeded as %q", tt.raw, got)
			}
		})
	}
}

func TestDecodeEventsAndMatchUsesSpanAndCanonicalPath(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "/workspace/approved-marker.txt"})
	fs, _ := json.Marshal(gen.FsChangedPayload{Changes: []gen.FsChangedPayloadChangesItem{
		{Path: "approved-marker.txt", Hash: "sha256:a", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded},
		{Path: "hidden.txt", Hash: "sha256:b", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded},
	}})
	events := []gen.EventRecord{
		{Seq: 2, TraceID: "trace", SpanID: "child", Kind: gen.KindSubagentToolCall, Payload: mustJSON(t, gen.SubagentToolCallPayload{CallID: "call-1", Name: "Write", Args: args})},
		{Seq: 3, TraceID: "trace", SpanID: "child", Kind: gen.KindCollectorFsChanged, Actor: "collector", Payload: fs},
	}
	intents, effects, report, err := DecodeEvents(events, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || len(effects) != 2 || !report.EffectComplete {
		t.Fatalf("decode=%+v %+v %+v", intents, effects, report)
	}
	if len(report.Rows) != 2 || report.Rows[0].Classification != Matched || report.Rows[1].Classification != ObservedUnreported {
		t.Fatalf("rows=%+v", report.Rows)
	}
	if report.Rows[0].Path != "approved-marker.txt" || report.Rows[0].IntentSeq != 2 || report.Rows[0].EffectSeq != 3 {
		t.Fatalf("matched row=%+v", report.Rows[0])
	}
}

func TestMatchDeterministicForRepeatedPath(t *testing.T) {
	intents := []IntentAction{
		{TraceID: "t", SpanID: "s", CallID: "c1", Name: "Write", Path: "same.txt", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Seq: 2},
		{TraceID: "t", SpanID: "s", CallID: "c2", Name: "Write", Path: "same.txt", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Seq: 4},
	}
	effects := []EffectAction{
		{TraceID: "t", SpanID: "s", Path: "same.txt", Seq: 3, ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded},
		{TraceID: "t", SpanID: "s", Path: "same.txt", Seq: 5, ChangeType: gen.FsChangedPayloadChangesItemChangeTypeModified},
	}
	rows, err := Match(intents, effects, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{Classification: Matched, TraceID: "t", SpanID: "s", Path: "same.txt", IntentSeq: 2, EffectSeq: 3, CallID: "c1", Name: "Write", ReportedType: gen.FsChangedPayloadChangesItemChangeTypeAdded, ObservedType: gen.FsChangedPayloadChangesItemChangeTypeAdded},
		{Classification: Matched, TraceID: "t", SpanID: "s", Path: "same.txt", IntentSeq: 4, EffectSeq: 5, CallID: "c2", Name: "Write", ReportedType: gen.FsChangedPayloadChangesItemChangeTypeAdded, ObservedType: gen.FsChangedPayloadChangesItemChangeTypeModified},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows=%+v, want %+v", rows, want)
	}
}

func TestMatchDeterministicAcrossMultiplePaths(t *testing.T) {
	intents := []IntentAction{
		{TraceID: "t", SpanID: "s", CallID: "c-z", Name: "Write", Path: "z.txt", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Seq: 6},
		{TraceID: "t", SpanID: "s", CallID: "c-a", Name: "Write", Path: "a.txt", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Seq: 2},
		{TraceID: "t", SpanID: "s", CallID: "c-m", Name: "Write", Path: "m.txt", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Seq: 4},
	}
	effects := []EffectAction{
		{TraceID: "t", SpanID: "s", Path: "m.txt", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Seq: 5},
		{TraceID: "t", SpanID: "s", Path: "z.txt", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Seq: 7},
		{TraceID: "t", SpanID: "s", Path: "a.txt", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Seq: 3},
	}
	var baseline []Row
	for i := 0; i < 100; i++ {
		rows, err := Match(intents, effects, true)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			baseline = rows
			continue
		}
		if !reflect.DeepEqual(rows, baseline) {
			t.Fatalf("반복 실행 결과가 달라짐: run=%d got=%+v baseline=%+v", i, rows, baseline)
		}
	}
	if got := []string{baseline[0].Path, baseline[1].Path, baseline[2].Path}; !reflect.DeepEqual(got, []string{"a.txt", "m.txt", "z.txt"}) {
		t.Fatalf("경로 정렬=%v", got)
	}
}

func TestMatchNeverCrossesSpan(t *testing.T) {
	rows, err := Match(
		[]IntentAction{{TraceID: "t", SpanID: "child-a", CallID: "c", Name: "Write", Path: "same.txt", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Seq: 2}},
		[]EffectAction{{TraceID: "t", SpanID: "child-b", Path: "same.txt", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Seq: 3}},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Classification == Matched || rows[1].Classification == Matched {
		t.Fatalf("cross-span false match: %+v", rows)
	}
}

func TestMatchRejectsAmbiguousDuplicateIdentity(t *testing.T) {
	duplicate := []EffectAction{{SpanID: "s", Path: "x", Seq: 3}, {SpanID: "s", Path: "x", Seq: 3}}
	if _, err := Match(nil, duplicate, true); !errors.Is(err, ErrAmbiguousMatch) {
		t.Fatalf("duplicate effect err=%v, want ErrAmbiguousMatch", err)
	}
	intents := []IntentAction{{SpanID: "s", Path: "x", Seq: 2}, {SpanID: "s", Path: "x", Seq: 2}}
	if _, err := Match(intents, nil, true); !errors.Is(err, ErrAmbiguousMatch) {
		t.Fatalf("duplicate intent err=%v, want ErrAmbiguousMatch", err)
	}
}

func TestEmptyChangesNeverBecomeReportedUnobserved(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "/workspace/ephemeral.txt"})
	fs, _ := json.Marshal(gen.FsChangedPayload{Changes: []gen.FsChangedPayloadChangesItem{}})
	events := []gen.EventRecord{
		{Seq: 1, TraceID: "t", SpanID: "s", Kind: gen.KindSubagentToolCall, Payload: mustJSON(t, gen.SubagentToolCallPayload{CallID: "c", Name: "Write", Args: args})},
		{Seq: 2, TraceID: "t", SpanID: "s", Kind: gen.KindCollectorFsChanged, Actor: "collector", Payload: fs},
	}
	_, _, _, err := DecodeEvents(events, "/workspace")
	if !errors.Is(err, ErrIncompleteObservation) {
		t.Fatalf("empty changes err=%v, want ErrIncompleteObservation", err)
	}
}

func TestDecodeRequiresCollectorObservation(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "/workspace/x"})
	tests := []struct {
		name   string
		events []gen.EventRecord
	}{
		{name: "intent without collector", events: []gen.EventRecord{{Seq: 1, TraceID: "t", SpanID: "s", Kind: gen.KindSubagentToolCall, Payload: mustJSON(t, gen.SubagentToolCallPayload{CallID: "c", Name: "Write", Args: args})}}},
		{name: "no intent and no collector", events: []gen.EventRecord{{Seq: 1, TraceID: "t", SpanID: "s", Kind: gen.KindSessionStart, Payload: json.RawMessage(`{}`)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := DecodeEvents(tt.events, "/workspace")
			if !errors.Is(err, ErrIncompleteObservation) {
				t.Fatalf("collector 부재 err=%v, want ErrIncompleteObservation", err)
			}
		})
	}
}

func TestDecodeRejectsUnmatchableIntentInsteadOfMatching(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "/etc/passwd"})
	events := []gen.EventRecord{{Seq: 1, TraceID: "t", SpanID: "s", Kind: gen.KindSubagentToolCall, Payload: mustJSON(t, gen.SubagentToolCallPayload{CallID: "c", Name: "Write", Args: args})}}
	_, _, _, err := DecodeEvents(events, "/workspace")
	if !errors.Is(err, ErrUnmatchableIntent) {
		t.Fatalf("outside path err=%v, want ErrUnmatchableIntent", err)
	}
}

func TestDecodeRejectsAbsoluteEffectPath(t *testing.T) {
	fs, _ := json.Marshal(gen.FsChangedPayload{Changes: []gen.FsChangedPayloadChangesItem{{Path: "/workspace/x", Hash: "sha256:a", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded}}})
	events := []gen.EventRecord{{Seq: 1, TraceID: "t", SpanID: "s", Kind: gen.KindCollectorFsChanged, Actor: "collector", Payload: fs}}
	_, _, _, err := DecodeEvents(events, "/workspace")
	if !errors.Is(err, ErrUnmatchableIntent) {
		t.Fatalf("absolute effect path err=%v, want ErrUnmatchableIntent", err)
	}
}

func TestDecodeRejectsMultipleTraces(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "/workspace/x"})
	events := []gen.EventRecord{
		{Seq: 1, TraceID: "trace-a", SpanID: "s", Kind: gen.KindSubagentToolCall, Payload: mustJSON(t, gen.SubagentToolCallPayload{CallID: "c", Name: "Write", Args: args})},
		{Seq: 2, TraceID: "trace-b", SpanID: "s", Kind: gen.KindCollectorFsChanged, Actor: "collector", Payload: mustJSON(t, gen.FsChangedPayload{Changes: []gen.FsChangedPayloadChangesItem{{Path: "x", Hash: "sha256:a", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded}}})},
	}
	_, _, _, err := DecodeEvents(events, "/workspace")
	if err == nil || !strings.Contains(err.Error(), "복수 trace_id") {
		t.Fatalf("multiple trace IDs err=%v", err)
	}
}

func TestRenderIsDeterministicAndSummaryMatchesRows(t *testing.T) {
	r := Report{
		EffectComplete:  true,
		NetChangesKnown: true,
		Rows: []Row{
			{Classification: ObservedUnreported, SpanID: "child-b", ParentSpanID: "root", Path: "z.txt", EffectSeq: 8, ObservedType: gen.FsChangedPayloadChangesItemChangeTypeAdded, Reason: "intent not reported"},
			{Classification: Matched, SpanID: "child-a", ParentSpanID: "root", Path: "m.txt", IntentSeq: 2, EffectSeq: 3, ReportedType: gen.FsChangedPayloadChangesItemChangeTypeAdded, ObservedType: gen.FsChangedPayloadChangesItemChangeTypeModified},
			{Classification: ReportedUnobserved, SpanID: "child-a", ParentSpanID: "root", Path: "a.txt", IntentSeq: 1, ReportedType: gen.FsChangedPayloadChangesItemChangeTypeDeleted, Reason: "effect not observed"},
		},
	}
	var baseline []byte
	for i := 0; i < 100; i++ {
		got, err := Render(r)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			baseline = got
		} else if string(got) != string(baseline) {
			t.Fatalf("renderer output changed on repeat %d:\n%s\nwant:\n%s", i, got, baseline)
		}
	}
	text := string(baseline)
	if !strings.Contains(text, "effect_observation: complete\n") {
		t.Fatalf("missing complete observation header: %q", text)
	}
	if !strings.Contains(text, "a.txt") || !strings.Contains(text, "m.txt") || !strings.Contains(text, "z.txt") {
		t.Fatalf("missing paths: %q", text)
	}
	if !strings.Contains(text, "total\t3\nmatched\t1\nreported_unobserved\t1\nobserved_unreported\t1\n") {
		t.Fatalf("summary disagrees with rows: %q", text)
	}
	if strings.Index(text, "a.txt") > strings.Index(text, "m.txt") || strings.Index(text, "m.txt") > strings.Index(text, "z.txt") {
		t.Fatalf("rows are not path-deterministic: %q", text)
	}
}

func TestRenderRejectsIncompleteObservationWithoutTable(t *testing.T) {
	for _, report := range []Report{
		{EffectComplete: false, NetChangesKnown: true},
		{EffectComplete: true, NetChangesKnown: false},
	} {
		got, err := Render(report)
		if !errors.Is(err, ErrIncompleteObservation) {
			t.Fatalf("Render error=%v, want ErrIncompleteObservation", err)
		}
		if len(got) != 0 {
			t.Fatalf("incomplete report emitted bytes: %q", got)
		}
	}
}

func TestDecodePreservesParentSpanOnAuditRows(t *testing.T) {
	parent := "root"
	args, _ := json.Marshal(map[string]string{"path": "/workspace/a.txt"})
	fs, _ := json.Marshal(gen.FsChangedPayload{Changes: []gen.FsChangedPayloadChangesItem{{Path: "a.txt", Hash: "sha256:a", ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded}}})
	_, _, report, err := DecodeEvents([]gen.EventRecord{
		{Seq: 1, TraceID: "t", SpanID: "child", ParentSpanID: &parent, Kind: gen.KindSubagentToolCall, Payload: mustJSON(t, gen.SubagentToolCallPayload{CallID: "c", Name: "Write", Args: args})},
		{Seq: 2, TraceID: "t", SpanID: "child", ParentSpanID: &parent, Actor: "collector", Kind: gen.KindCollectorFsChanged, Payload: fs},
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rows) != 1 || report.Rows[0].ParentSpanID != parent {
		t.Fatalf("parent span not preserved: %+v", report.Rows)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
