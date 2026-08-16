package logd

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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

// T4 재리뷰 차단 2의 회귀: usage 합산은 fail-closed다 —
// int64 overflow(음수 래핑)와 음수 usage를 조용히 통과시키지 않는다.
func TestReplayUsageOverflowRejected(t *testing.T) {
	root := strings.Repeat("b", 16)
	e1 := mkEvent(1, gen.KindAssistantMessage, "parent", root, `{"text":"x"}`)
	e1.UsageIn = ptr(int64(math.MaxInt64))
	e2 := mkEvent(2, gen.KindAssistantMessage, "parent", root, `{"text":"y"}`)
	e2.UsageIn = ptr(int64(1))
	if _, err := Replay([]gen.EventRecord{e1, e2}); err == nil {
		t.Fatal("MaxInt64+1 합산이 통과함 — overflow 미검출")
	}

	// actor별 합산만 overflow하는 경우도 잡힌다 (다른 actor로 분산 시 전체는 안전)
	e3 := mkEvent(1, gen.KindSubagentUsage, "subagent:null:1", root, `{}`)
	e3.UsageOut = ptr(int64(math.MaxInt64))
	e4 := mkEvent(2, gen.KindSubagentUsage, "subagent:null:1", root, `{}`)
	e4.UsageOut = ptr(int64(math.MaxInt64))
	if _, err := Replay([]gen.EventRecord{e3, e4}); err == nil {
		t.Fatal("actor별 overflow가 통과함")
	}

	neg := mkEvent(1, gen.KindAssistantMessage, "parent", root, `{"text":"z"}`)
	neg.UsageIn = ptr(int64(-1))
	if _, err := Replay([]gen.EventRecord{neg}); err == nil {
		t.Fatal("음수 usage가 통과함")
	}
}

// T4 재리뷰 차단 1의 회귀: 포크 목적지는 반드시 비어 있어야 한다.
func TestForkDestinationMustBeEmpty(t *testing.T) {
	ctx := context.Background()
	root := strings.Repeat("b", 16)

	src := &FakeStore{}
	sw, _ := NewWriter(ctx, src)
	sw.Submit(ctx, mkEvent(0, gen.KindSessionStart, "parent", root, `{}`))
	sw.Submit(ctx, mkEvent(0, gen.KindUserMessage, "parent", root, `{"text":"x"}`))
	sw.Close()
	srcBefore, _ := src.ReadFrom(ctx, 1)

	t.Run("자기 자신으로의 포크", func(t *testing.T) {
		// 같은 store의 Reader/Writer — 원본이 비어 있지 않으므로 거부
		selfW, _ := NewWriter(ctx, src)
		defer selfW.Close()
		err := Fork(ctx, src, 1, strings.Repeat("f", 32), selfW)
		if !errors.Is(err, ErrDestinationNotEmpty) {
			t.Fatalf("자기 포크 = %v (ErrDestinationNotEmpty 기대)", err)
		}
		after, _ := src.ReadFrom(ctx, 1)
		if len(after) != len(srcBefore) {
			t.Fatalf("자기 포크 거부 후 원본 %d건 (%d건 기대) — 오염", len(after), len(srcBefore))
		}
	})

	t.Run("비어 있지 않은 별도 목적지", func(t *testing.T) {
		dst := &FakeStore{}
		dw, _ := NewWriter(ctx, dst)
		defer dw.Close()
		if _, err := dw.Submit(ctx, mkEvent(0, gen.KindSessionStart, "parent", root, `{}`)); err != nil {
			t.Fatal(err)
		}
		dstBefore, _ := dst.ReadFrom(ctx, 1)
		err := Fork(ctx, src, 1, strings.Repeat("f", 32), dw)
		if !errors.Is(err, ErrDestinationNotEmpty) {
			t.Fatalf("비공백 목적지 포크 = %v (ErrDestinationNotEmpty 기대)", err)
		}
		dstAfter, _ := dst.ReadFrom(ctx, 1)
		if len(dstAfter) != len(dstBefore) {
			t.Fatal("거부된 포크가 목적지를 변형함")
		}
	})

	t.Run("배치보다 먼저 admission된 제출과의 직렬화", func(t *testing.T) {
		// 빈 목적지라도 배치 처리 전에 다른 제출이 커밋되면 거부된다 —
		// LastSeq 사전 검사 방식의 TOCTOU가 루프 직렬화로 닫혀 있음을 확인.
		dst := &FakeStore{}
		dw, _ := NewWriter(ctx, dst)
		defer dw.Close()
		if _, err := dw.Submit(ctx, mkEvent(0, gen.KindSessionStart, "parent", root, `{}`)); err != nil {
			t.Fatal(err)
		}
		if err := Fork(ctx, src, 1, strings.Repeat("f", 32), dw); !errors.Is(err, ErrDestinationNotEmpty) {
			t.Fatalf("선행 커밋 후 포크 = %v", err)
		}
	})
}

// T4 재재리뷰 차단의 회귀: 포크 배치는 저장소 수준에서 all-or-nothing이다 —
// 배치 중간(2번째) 커밋 실패 시 목적지에 아무것도 남지 않는다. session/fork만
// 내구 저장되면 존재하지 않는 복사 상태를 가리키는 손상 로그가 되기 때문.
func TestForkAtomicOnStoreFailure(t *testing.T) {
	ctx := context.Background()
	root := strings.Repeat("b", 16)
	src := &FakeStore{}
	sw, _ := NewWriter(ctx, src)
	sw.Submit(ctx, mkEvent(0, gen.KindSessionStart, "parent", root, `{}`))
	sw.Submit(ctx, mkEvent(0, gen.KindUserMessage, "parent", root, `{"text":"x"}`))
	sw.Close()

	dst := &FakeStore{failOn: 2, failErr: errors.New("injected append failure")}
	dw, err := NewWriter(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	forkErr := Fork(ctx, src, 2, strings.Repeat("f", 32), dw)
	if forkErr == nil {
		t.Fatal("주입 실패에도 포크가 성공함")
	}
	got, _ := dst.ReadFrom(ctx, 1)
	if len(got) != 0 {
		t.Fatalf("배치 실패 후 목적지에 %d건 잔존 — 부분 커밋 (all-or-nothing 위반)", len(got))
	}
	// 저장소 커밋 실패이므로 writer는 종료 상태다
	if _, err := dw.Submit(ctx, mkEvent(0, gen.KindSessionStart, "parent", root, `{}`)); !errors.Is(err, ErrTerminal) {
		t.Fatalf("배치 실패 후 Submit = %v (ErrTerminal 기대)", err)
	}
	if err := dw.Close(); !errors.Is(err, ErrTerminal) {
		t.Fatalf("Close = %v (ErrTerminal 기대)", err)
	}
}

// 비차단 권고의 회귀: atSeq는 원본에 실제 존재하는 seq여야 한다.
func TestForkRequiresExistingSeq(t *testing.T) {
	ctx := context.Background()
	root := strings.Repeat("b", 16)
	// seq gap이 있는 store (2, 4만 존재)
	gapStore := &FakeStore{
		events: []gen.EventRecord{
			mkEvent(2, gen.KindSessionStart, "parent", root, `{}`),
			mkEvent(4, gen.KindUserMessage, "parent", root, `{"text":"x"}`),
		},
		lastSeq: 4,
	}
	dst := &FakeStore{}
	dw, _ := NewWriter(ctx, dst)
	defer dw.Close()

	// 첫 이벤트 seq(2)보다 앞선 지점 — cut이 비는 경우 (panic 회귀)
	if err := Fork(ctx, gapStore, 1, strings.Repeat("f", 32), dw); err == nil {
		t.Error("첫 seq 이전 지점 포크가 성공함")
	}
	// gap 내부의 존재하지 않는 seq
	if err := Fork(ctx, gapStore, 3, strings.Repeat("f", 32), dw); err == nil {
		t.Error("존재하지 않는 seq로의 포크가 성공함")
	}
	if got, _ := dst.ReadFrom(ctx, 1); len(got) != 0 {
		t.Errorf("거부된 포크가 이벤트를 남김: %d건", len(got))
	}
	// 존재하는 seq(2)로는 성공
	if err := Fork(ctx, gapStore, 2, strings.Repeat("f", 32), dw); err != nil {
		t.Errorf("존재하는 seq 포크 실패: %v", err)
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
