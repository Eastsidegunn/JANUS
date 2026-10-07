package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/audit"
	"github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

const runBWarningLine = "경고: 모델 재시도 루프 의심 — api.anthropic.com CONNECT deny 8건(간격 0.5s→24.5s), 모델 가시 이벤트 0"

// seedRetryLoopSession writes a Run B-shaped session (T27(c)): ready, then
// only same-domain egress denies with backoff gaps and no model-visible event.
// modelVisible=true gives the Run A3 shape (message after ready).
func seedRetryLoopSession(t *testing.T, db string, modelVisible, withFsChanged bool) {
	t.Helper()
	ctx := context.Background()
	log, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	trace := strings.Repeat("a", 32)
	root, child := strings.Repeat("b", 16), strings.Repeat("c", 16)
	parent := root
	actor := "subagent:claude-code:1"
	events := []gen.EventRecord{
		{TraceID: trace, SpanID: root, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`)},
		{TraceID: trace, SpanID: child, ParentSpanID: &parent, Kind: gen.KindSubagentReady, Actor: actor, Payload: mustJSON(t, gen.ReadyPayload{Grade: gen.ReadyPayloadGradeObservable})},
	}
	if modelVisible {
		events = append(events, gen.EventRecord{TraceID: trace, SpanID: child, ParentSpanID: &parent, Kind: gen.KindSubagentMessage, Actor: actor, Payload: json.RawMessage(`{"text":"작업 중"}`)})
	}
	for _, at := range []int64{0, 500, 1500, 3500, 8000, 16000, 28500, 53000} {
		reason := "domain not allowed"
		events = append(events, gen.EventRecord{TraceID: trace, SpanID: child, ParentSpanID: &parent, Kind: gen.KindCollectorEgress, Actor: "collector",
			Payload: mustJSON(t, gen.EgressPayload{AtMs: 1_790_000_000_000 + at, Decision: gen.EgressPayloadDecisionDeny, Domain: "api.anthropic.com", Method: "CONNECT", Reason: &reason})})
	}
	if withFsChanged {
		fs := gen.FsChangedPayload{Changes: []gen.FsChangedPayloadChangesItem{{Path: "marker.txt", Hash: "sha256:" + strings.Repeat("a", 64), ChangeType: gen.FsChangedPayloadChangesItemChangeTypeAdded}}}
		events = append(events, gen.EventRecord{TraceID: trace, SpanID: child, ParentSpanID: &parent, Kind: gen.KindCollectorFsChanged, Actor: "collector", Payload: mustJSON(t, fs)})
	}
	events = append(events,
		gen.EventRecord{TraceID: trace, SpanID: child, ParentSpanID: &parent, Kind: gen.KindSubagentDone, Actor: actor, Payload: mustJSON(t, gen.DonePayload{Status: gen.DonePayloadStatusStopped, Result: "stopped"})},
		gen.EventRecord{TraceID: trace, SpanID: root, Kind: gen.KindSessionEnd, Actor: "parent", Payload: json.RawMessage(`{}`)},
	)
	if err := log.Writer.InitBatch(ctx, events); err != nil {
		log.Close()
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func readSessionEvents(t *testing.T, db string) []gen.EventRecord {
	t.Helper()
	log, err := sqlite.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	events, err := log.Reader.ReadFrom(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func countLine(text, line string) int {
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			n++
		}
	}
	return n
}

// T27(c): hx audit 리포트와 hx replay 요약이 Run B 패턴에서 경고 1줄을 표면화
// 하고, 그 줄은 같은 로그를 다시 읽어 core 판정 함수로 재계산한 값과 같다.
func TestAuditAndReplaySurfaceRetryLoopWarning(t *testing.T) {
	db := filepath.Join(t.TempDir(), "runb.db")
	seedRetryLoopSession(t, db, false, true)

	var auditOut bytes.Buffer
	if err := auditSession(context.Background(), auditQuery{Session: db}, &auditOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(auditOut.String(), "effect_observation: complete\n") || countLine(auditOut.String(), runBWarningLine) != 1 {
		t.Fatalf("audit 경고 1줄 누락:\n%s", auditOut.String())
	}

	var replayOut, replayErr bytes.Buffer
	if err := replaySession(context.Background(), db, 0, &replayOut, &replayErr); err != nil {
		t.Fatal(err)
	}
	if countLine(replayErr.String(), runBWarningLine) != 1 || !strings.HasPrefix(replayErr.String(), "hx replay: trace=") {
		t.Fatalf("replay 요약 경고 1줄 누락:\n%s", replayErr.String())
	}
	if strings.Contains(replayOut.String(), "경고:") {
		t.Fatalf("경고가 이벤트 NDJSON(stdout)에 섞임: %s", replayOut.String())
	}

	// 재계산 일치: 같은 로그를 다시 읽어 순수 함수로 판정한 결과와 같다.
	recomputed, err := audit.RetryLoopWarnings(readSessionEvents(t, db), audit.DefaultRetryLoopDenyThreshold)
	if err != nil || len(recomputed) != 1 || recomputed[0] != runBWarningLine {
		t.Fatalf("로그 재계산 판정 불일치: %q err=%v", recomputed, err)
	}
	// 반복 재생도 바이트 동일하다 (FR-LOG-06).
	var again, againErr bytes.Buffer
	if err := replaySession(context.Background(), db, 0, &again, &againErr); err != nil {
		t.Fatal(err)
	}
	if again.String() != replayOut.String() || againErr.String() != replayErr.String() {
		t.Fatal("동일 세션 재생 출력이 비결정적")
	}
}

// 정상 세션(Run A3 형태)은 audit·replay 모두 무경고다.
func TestAuditAndReplayNormalSessionNoWarning(t *testing.T) {
	db := filepath.Join(t.TempDir(), "runa3.db")
	seedRetryLoopSession(t, db, true, true)
	var auditOut bytes.Buffer
	if err := auditSession(context.Background(), auditQuery{Session: db}, &auditOut); err != nil {
		t.Fatal(err)
	}
	var replayOut, replayErr bytes.Buffer
	if err := replaySession(context.Background(), db, 0, &replayOut, &replayErr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(auditOut.String(), "경고:") || strings.Contains(replayErr.String(), "경고:") {
		t.Fatalf("정상 세션에 경고:\naudit=%s\nreplay=%s", auditOut.String(), replayErr.String())
	}
}

// 불완전 관측(fs_changed 부재)에서도 stdout 무출력 규칙은 유지되고, 경고는
// 오류 텍스트로 표면화된다.
func TestAuditIncompleteObservationCarriesRetryLoopWarningInError(t *testing.T) {
	db := filepath.Join(t.TempDir(), "runb-incomplete.db")
	seedRetryLoopSession(t, db, false, false)
	var out bytes.Buffer
	err := auditSession(context.Background(), auditQuery{Session: db}, &out)
	if !errors.Is(err, audit.ErrIncompleteObservation) {
		t.Fatalf("err=%v, want ErrIncompleteObservation", err)
	}
	if out.Len() != 0 {
		t.Fatalf("불완전 관측에서 stdout 생성: %q", out.String())
	}
	if countLine(err.Error(), runBWarningLine) != 1 {
		t.Fatalf("오류에 경고 1줄 누락: %v", err)
	}
}
