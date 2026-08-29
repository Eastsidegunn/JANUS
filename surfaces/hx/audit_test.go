package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/audit"
	"github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

func TestAuditSessionUsesOneSnapshotForPrefixCostAndContext(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "session.db")
	seedAuditSession(t, db)

	var out bytes.Buffer
	err := auditSession(ctx, auditQuery{Session: db, Cost: true, AtSeq: 6}, &out)
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "effect_observation: complete\n") || !strings.Contains(text, "usage_in\t7\nusage_out\t3\n") {
		t.Fatalf("감사 표/비용 누락: %q", text)
	}
	if !strings.Contains(text, "parent_context\n") || !strings.Contains(text, "\tuser\t"+strings.Repeat("b", 16)+"\tuser context\n") {
		t.Fatalf("부모 컨텍스트 누락: %q", text)
	}
	if strings.Contains(text, "child-only secret") {
		t.Fatalf("자식 중간 이벤트가 부모 컨텍스트에 유입됨: %q", text)
	}

	var events []gen.EventRecord
	log, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	events, err = log.Reader.ReadFrom(ctx, 1)
	if closeErr := log.Close(); err != nil || closeErr != nil {
		t.Fatalf("snapshot read/close: read=%v close=%v", err, closeErr)
	}
	prefix := prefixEvents(events, 6)
	if len(prefix) != 6 || prefix[len(prefix)-1].Seq != 6 {
		t.Fatalf("--at-seq 경계가 포함되지 않음: len=%d last=%d", len(prefix), prefix[len(prefix)-1].Seq)
	}
	if got := prefixEvents(events, 0); len(got) != len(events) {
		t.Fatalf("0 prefix가 전체를 보존하지 않음: %d/%d", len(got), len(events))
	}
}

func TestAuditSessionRejectsIncompleteWithoutStdout(t *testing.T) {
	db := filepath.Join(t.TempDir(), "incomplete.db")
	seedAuditSessionWithoutCollector(t, db)
	var out bytes.Buffer
	err := auditSession(context.Background(), auditQuery{Session: db}, &out)
	if !errors.Is(err, audit.ErrIncompleteObservation) {
		t.Fatalf("불완전 관측 err=%v, want ErrIncompleteObservation", err)
	}
	if out.Len() != 0 {
		t.Fatalf("불완전 관측에서 stdout이 생성됨: %q", out.String())
	}
}

func TestAuditSessionFiltersRequireExistingSpanAndActor(t *testing.T) {
	db := filepath.Join(t.TempDir(), "filters.db")
	seedAuditSession(t, db)
	for _, query := range []auditQuery{{Session: db, Span: "missing"}, {Session: db, Actor: "missing"}} {
		var out bytes.Buffer
		if err := auditSession(context.Background(), query, &out); err == nil {
			t.Fatalf("없는 필터가 성공함: %+v", query)
		} else if out.Len() != 0 {
			t.Fatalf("없는 필터가 stdout을 생성함: %+v output=%q", query, out.String())
		}
	}
}

func TestAuditAtSeqUsesReplayPrefixBoundary(t *testing.T) {
	db := filepath.Join(t.TempDir(), "prefix.db")
	seedAuditSession(t, db)
	log, err := sqlite.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	events, err := log.Reader.ReadFrom(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	auditPrefix := prefixEvents(events, 5)
	replayPrefix, err := readTo(context.Background(), log.Reader, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auditPrefix, replayPrefix) {
		t.Fatalf("audit/replay prefix 경계 불일치:\naudit=%+v\nreplay=%+v", auditPrefix, replayPrefix)
	}
}

func seedAuditSession(t *testing.T, db string) {
	t.Helper()
	ctx := context.Background()
	log, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	trace := strings.Repeat("a", 32)
	root, child := strings.Repeat("b", 16), strings.Repeat("c", 16)
	args, _ := json.Marshal(map[string]string{"path": "/workspace/approved.txt"})
	fs, _ := json.Marshal(gen.FsChangedPayload{Changes: []gen.FsChangedPayloadChangesItem{{Path: "approved.txt", Hash: "sha256:" + strings.Repeat("a", 64), ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded}}})
	parent := root
	events := []gen.EventRecord{
		{TraceID: trace, SpanID: root, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`)},
		{TraceID: trace, SpanID: root, Kind: gen.KindUserMessage, Actor: "parent", Payload: json.RawMessage(`{"text":"user context"}`)},
		{TraceID: trace, SpanID: child, ParentSpanID: &parent, Kind: gen.KindSubagentToolCall, Actor: "subagent:null:1", Payload: mustJSON(t, gen.SubagentToolCallPayload{CallID: "call-1", Name: "Write", Args: args}), UsageIn: ptrInt64(7), UsageOut: ptrInt64(3)},
		{TraceID: trace, SpanID: child, ParentSpanID: &parent, Kind: gen.KindSubagentMessage, Actor: "subagent:null:1", Payload: json.RawMessage(`{"text":"child-only secret"}`)},
		{TraceID: trace, SpanID: child, ParentSpanID: &parent, Kind: gen.KindCollectorFsChanged, Actor: "collector", Payload: fs},
		{TraceID: trace, SpanID: root, Kind: gen.KindSessionEnd, Actor: "parent", Payload: json.RawMessage(`{}`)},
	}
	if err := log.Writer.InitBatch(ctx, events); err != nil {
		log.Close()
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedAuditSessionWithoutCollector(t *testing.T, db string) {
	t.Helper()
	ctx := context.Background()
	log, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	trace := strings.Repeat("d", 32)
	root := strings.Repeat("e", 16)
	args, _ := json.Marshal(map[string]string{"path": "/workspace/missing.txt"})
	if err := log.Writer.InitBatch(ctx, []gen.EventRecord{
		{TraceID: trace, SpanID: root, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`)},
		{TraceID: trace, SpanID: root, Kind: gen.KindSubagentToolCall, Actor: "subagent:null:1", Payload: mustJSON(t, gen.SubagentToolCallPayload{CallID: "call-1", Name: "Write", Args: args})},
	}); err != nil {
		log.Close()
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func ptrInt64(v int64) *int64 { return &v }

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
