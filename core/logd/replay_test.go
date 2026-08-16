package logd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

func ptr[T any](v T) *T { return &v }

func mkEvent(seq int64, kind gen.Kind, actor, span, payload string) gen.EventRecord {
	return gen.EventRecord{
		Seq: seq, Ts: seq * 10, TraceID: strings.Repeat("a", 32),
		SpanID: span, Kind: kind, Actor: actor, Payload: []byte(payload),
	}
}

// FR-LOG-03/04/10: 부모 모델 가시 히스토리 프로젝션 —
// 루트 span의 대화·툴 이벤트와 자식의 최종 결과만 진입하고,
// 자식 중간 이벤트·chunk·훅·수집기 이벤트는 제외된다.
func TestReplayMessagesProjection(t *testing.T) {
	root := strings.Repeat("b", 16)
	child := strings.Repeat("c", 16)
	events := []gen.EventRecord{
		mkEvent(1, gen.KindSessionStart, "parent", root, `{}`),
		mkEvent(2, gen.KindUserMessage, "parent", root, `{"text":"질문"}`),
		mkEvent(3, gen.KindAssistantChunk, "parent", root, `{"delta":"…"}`), // 제외
		mkEvent(4, gen.KindAssistantMessage, "parent", root, `{"text":"응답"}`),
		mkEvent(5, gen.KindToolCall, "parent", root, `{"name":"bash"}`),
		mkEvent(6, gen.KindToolResult, "parent", root, `{"output":"ok"}`),
		mkEvent(7, gen.KindSubagentSpawn, "parent", child, `{"adapter":"null"}`),
		mkEvent(8, gen.KindSubagentMessage, "subagent:null:1", child, `{"text":"중간"}`),                                    // 제외 (자식 중간)
		mkEvent(9, gen.KindSubagentToolCall, "subagent:null:1", child, `{"name":"edit"}`),                                 // 제외 (자식 중간)
		mkEvent(10, gen.KindSubagentDone, "subagent:null:1", child, `{"status":"ok","result":"요약"}`),                      // 포함
		mkEvent(11, gen.KindHookVerdict, "parent", root, `{"point":"pre_step","verdict":"continue"}`),                     // 제외
		mkEvent(12, gen.KindCollectorEgress, "collector", root, `{"domain":"x","method":"GET","size_bytes":1,"at_ms":1}`), // 제외
		mkEvent(13, gen.KindSessionEnd, "parent", root, `{}`),
	}
	s, err := Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	wantRoles := []Role{RoleUser, RoleAssistant, RoleToolCall, RoleToolResult, RoleSubagentResult}
	if len(s.Messages) != len(wantRoles) {
		t.Fatalf("히스토리 %d항목 (%d 기대): %+v", len(s.Messages), len(wantRoles), s.Messages)
	}
	for i, r := range wantRoles {
		if s.Messages[i].Role != r {
			t.Errorf("항목 %d role=%s (%s 기대)", i, s.Messages[i].Role, r)
		}
	}
	if s.Messages[4].SpanID != child || string(s.Messages[4].Content) != `{"status":"ok","result":"요약"}` {
		t.Errorf("subagent 결과 항목 이상: %+v", s.Messages[4])
	}
	if !s.Ended || s.Spawns != 1 {
		t.Errorf("상태 이상: ended=%v spawns=%d", s.Ended, s.Spawns)
	}
}

// 자식 span의 대화·툴 kind는 (subagent/done 제외) 부모 히스토리에 안 들어간다.
func TestReplayChildSpanConversationExcluded(t *testing.T) {
	root := strings.Repeat("b", 16)
	child := strings.Repeat("c", 16)
	events := []gen.EventRecord{
		mkEvent(1, gen.KindSessionStart, "parent", root, `{}`),
		mkEvent(2, gen.KindToolCall, "subagent:null:1", child, `{"name":"x"}`), // 자식 span의 tool/call
	}
	s, err := Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 0 {
		t.Fatalf("자식 span 이벤트가 부모 히스토리에 진입: %+v", s.Messages)
	}
}

func TestReplayUsageAggregation(t *testing.T) {
	root := strings.Repeat("b", 16)
	e1 := mkEvent(1, gen.KindSessionStart, "parent", root, `{}`)
	e2 := mkEvent(2, gen.KindAssistantMessage, "parent", root, `{"text":"x"}`)
	e2.UsageIn, e2.UsageOut = ptr(int64(100)), ptr(int64(50))
	e3 := mkEvent(3, gen.KindSubagentUsage, "subagent:null:1", root, `{}`)
	e3.UsageIn, e3.UsageOut = ptr(int64(7)), ptr(int64(3))
	s, err := Replay([]gen.EventRecord{e1, e2, e3})
	if err != nil {
		t.Fatal(err)
	}
	if s.UsageIn != 107 || s.UsageOut != 53 {
		t.Fatalf("전체 usage %d/%d (107/53 기대)", s.UsageIn, s.UsageOut)
	}
	if u := s.UsageByActor["subagent:null:1"]; u.In != 7 || u.Out != 3 {
		t.Fatalf("actor별 usage 이상: %+v", s.UsageByActor)
	}
}

func TestReplayRejectsCorruptSequences(t *testing.T) {
	root := strings.Repeat("b", 16)
	base := mkEvent(1, gen.KindSessionStart, "parent", root, `{}`)
	cases := map[string][]gen.EventRecord{
		"빈 시퀀스":  {},
		"seq 역전": {base, mkEvent(1, gen.KindSessionEnd, "parent", root, `{}`)},
		"복수 trace": {base, func() gen.EventRecord {
			e := mkEvent(2, gen.KindSessionEnd, "parent", root, `{}`)
			e.TraceID = strings.Repeat("d", 32)
			return e
		}()},
	}
	for name, evs := range cases {
		if _, err := Replay(evs); err == nil {
			t.Errorf("%s: 손상 시퀀스가 재생됨", name)
		}
	}
}

// Replay는 입력을 변형하지 않는다 (순수 함수).
func TestReplayDoesNotMutateInput(t *testing.T) {
	root := strings.Repeat("b", 16)
	events := []gen.EventRecord{
		mkEvent(1, gen.KindSessionStart, "parent", root, `{}`),
		mkEvent(2, gen.KindUserMessage, "parent", root, `{"text":"원본"}`),
	}
	before, _ := json.Marshal(events)
	s, err := Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	// 프로젝션 내용을 변조해도 원본 이벤트는 불변이어야 한다(방어적 복사)
	s.Messages[0].Content[2] = 'X'
	after, _ := json.Marshal(events)
	if string(before) != string(after) {
		t.Fatal("Replay 산출물 변조가 입력 이벤트에 전파됨 — 복사 누락")
	}
}

// FR-LOG-05: 포크 — 새 trace_id, 원본 참조 보존, 포크 지점까지의
// 파생 상태 동일, 원본 불변.
func TestForkPreservesStateAndOrigin(t *testing.T) {
	ctx := context.Background()
	src := &FakeStore{}
	sw, err := NewWriter(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	root := strings.Repeat("b", 16)
	kinds := []struct {
		kind    gen.Kind
		payload string
	}{
		{gen.KindSessionStart, `{}`},
		{gen.KindUserMessage, `{"text":"하나"}`},
		{gen.KindAssistantMessage, `{"text":"둘"}`},
		{gen.KindUserMessage, `{"text":"셋"}`},
	}
	for _, k := range kinds {
		ev := mkEvent(0, k.kind, "parent", root, k.payload)
		if _, err := sw.Submit(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	sw.Close()

	dst := &FakeStore{}
	dw, err := NewWriter(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	newTrace := strings.Repeat("f", 32)
	const at = 3
	if err := Fork(ctx, src, at, newTrace, dw); err != nil {
		t.Fatal(err)
	}
	dw.Close()

	// 원본 불변
	origEvents, _ := src.ReadFrom(ctx, 1)
	if len(origEvents) != 4 {
		t.Fatalf("원본이 변형됨: %d건", len(origEvents))
	}
	for _, e := range origEvents {
		if e.TraceID != strings.Repeat("a", 32) {
			t.Fatal("원본 trace_id가 변형됨")
		}
	}

	forked, _ := dst.ReadFrom(ctx, 1)
	if len(forked) != at+1 { // session/fork + 복사 3건
		t.Fatalf("포크 로그 %d건 (%d건 기대)", len(forked), at+1)
	}
	fs, err := Replay(forked)
	if err != nil {
		t.Fatal(err)
	}
	if fs.TraceID != newTrace {
		t.Fatalf("포크 trace_id = %s", fs.TraceID)
	}
	if fs.Origin == nil || fs.Origin.OriginTraceID != strings.Repeat("a", 32) || fs.Origin.OriginSeq != at {
		t.Fatalf("원본 참조 훼손: %+v", fs.Origin)
	}
	for _, e := range forked {
		if e.TraceID != newTrace {
			t.Fatalf("포크 이벤트에 원본 trace_id 잔존: seq %d", e.Seq)
		}
	}
	// 포크 지점까지의 모델 가시 히스토리는 원본과 동일하다
	os, err := Replay(origEvents[:at])
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.Messages) != len(os.Messages) {
		t.Fatalf("히스토리 길이 %d vs %d", len(fs.Messages), len(os.Messages))
	}
	for i := range os.Messages {
		if fs.Messages[i].Role != os.Messages[i].Role ||
			string(fs.Messages[i].Content) != string(os.Messages[i].Content) {
			t.Fatalf("히스토리 %d 불일치", i)
		}
	}
}

func TestForkRejectsBadPoints(t *testing.T) {
	ctx := context.Background()
	src := &FakeStore{}
	sw, _ := NewWriter(ctx, src)
	root := strings.Repeat("b", 16)
	sw.Submit(ctx, mkEvent(0, gen.KindSessionStart, "parent", root, `{}`))
	sw.Close()
	dst := &FakeStore{}
	dw, _ := NewWriter(ctx, dst)
	defer dw.Close()

	if err := Fork(ctx, src, 0, strings.Repeat("f", 32), dw); err == nil {
		t.Error("seq 0 포크가 성공함")
	}
	if err := Fork(ctx, src, 99, strings.Repeat("f", 32), dw); err == nil {
		t.Error("범위 밖 seq 포크가 성공함")
	}
	if err := Fork(ctx, src, 1, strings.Repeat("a", 32), dw); err == nil {
		t.Error("원본과 동일한 trace_id 포크가 성공함")
	}
	if got, _ := dst.ReadFrom(ctx, 1); len(got) != 0 {
		t.Errorf("거부된 포크가 이벤트를 남김: %d건", len(got))
	}
}
