package main

// T7 완료 기준 E2E: hx run → null 어댑터 spawn → NDJSON 정규화 → writer →
// child span → hx replay 관통. 실제 빌드된 바이너리 두 개(hx, nulladapter)를
// 프로세스로 구동한다 (FR-ADP-01의 독립 실행 파일 성질까지 검증).

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

func buildBinaries(t *testing.T) (hx, adapter string) {
	t.Helper()
	dir := t.TempDir()
	hx = filepath.Join(dir, "hx")
	adapter = filepath.Join(dir, "nulladapter")
	for bin, pkg := range map[string]string{
		hx:      ".",
		adapter: "../../seams/subagent/nulladapter",
	} {
		cmd := exec.Command("go", "build", "-o", bin, pkg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("빌드 실패 %s: %v\n%s", pkg, err, out)
		}
	}
	return hx, adapter
}

func TestRunReplayEndToEnd(t *testing.T) {
	hx, adapter := buildBinaries(t)
	dir := t.TempDir()
	session := filepath.Join(dir, "session.db")

	// hx run
	run := exec.Command(hx, "run", "--session", session, "--adapter", adapter, "테스트 지시")
	var runOut, runErr bytes.Buffer
	run.Stdout, run.Stderr = &runOut, &runErr
	if err := run.Run(); err != nil {
		t.Fatalf("hx run 실패: %v\nstderr:\n%s", err, runErr.String())
	}
	// FR-CLI-06: stdout은 NDJSON — 각 줄이 유효 JSON 이벤트
	runLines := nonEmptyLines(runOut.String())
	if len(runLines) == 0 {
		t.Fatal("run stdout에 이벤트 없음")
	}
	for i, line := range runLines {
		var rec gen.EventRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("run stdout %d행이 NDJSON 이벤트가 아님: %v", i, err)
		}
	}

	// hx replay 2회 — 동일 세션의 재생은 결정론적 (FR-LOG-06)
	replay1out, replay1err := runReplay(t, hx, session, nil)
	replay2out, _ := runReplay(t, hx, session, nil)
	if replay1out != replay2out {
		t.Fatal("두 replay의 stdout이 다름 — 재생 비결정")
	}
	if replay1out != runOut.String() {
		t.Fatal("replay 이벤트가 run 출력과 다름")
	}
	if !strings.Contains(replay1err, "spawns=1") || !strings.Contains(replay1err, "ended=true") {
		t.Fatalf("replay 요약 이상: %s", replay1err)
	}

	// 세션 파일 직접 검증: 자식 이벤트는 child span, 부모 히스토리엔 done만
	ctx := context.Background()
	log, err := sqlite.Open(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	events, err := log.Reader.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	rootSpan := events[0].SpanID
	var childKinds []gen.Kind
	for _, e := range events {
		if strings.HasPrefix(e.Actor, "subagent:null:") {
			if e.SpanID == rootSpan {
				t.Fatalf("자식 이벤트 %s가 루트 span에 기록됨", e.Kind)
			}
			if e.ParentSpanID == nil || *e.ParentSpanID != rootSpan {
				t.Fatalf("자식 이벤트 %s의 parent_span_id가 루트가 아님", e.Kind)
			}
			childKinds = append(childKinds, e.Kind)
		}
	}
	// 어댑터 MUST 이벤트 (FR-ADP-03) + 중간 이벤트가 child span에 있다
	for _, want := range []gen.Kind{gen.KindSubagentReady, gen.KindSubagentMessage,
		gen.KindSubagentToolCall, gen.KindSubagentToolResult, gen.KindSubagentUsage, gen.KindSubagentDone} {
		found := false
		for _, k := range childKinds {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("child span에 %s 없음 (전체: %v)", want, childKinds)
		}
	}

	// FR-LOG-10: 부모 모델 히스토리에는 자식의 최종 결과만 진입한다
	state, err := logd.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) != 1 || state.Messages[0].Role != logd.RoleSubagentResult {
		t.Fatalf("부모 히스토리 = %+v (subagent_result 1건 기대 — 자식 중간 이벤트 미포함)", state.Messages)
	}
	if !strings.Contains(string(state.Messages[0].Content), "null 어댑터 완료: 테스트 지시") {
		t.Fatalf("최종 결과 내용 이상: %s", state.Messages[0].Content)
	}
	// usage는 어댑터 보고가 envelope로 집계된다 (FR-ADP-07)
	if state.UsageIn != 12 || state.UsageOut != 34 {
		t.Fatalf("usage 집계 %d/%d (12/34 기대)", state.UsageIn, state.UsageOut)
	}

	// --to prefix 재생 (FR-CLI-02)
	toOut, _ := runReplay(t, hx, session, []string{"--to", "3"})
	toLines := nonEmptyLines(toOut)
	if len(toLines) != 3 {
		t.Fatalf("--to 3 재생이 %d행 (3행 기대)", len(toLines))
	}
}

func runReplay(t *testing.T, hx, session string, extra []string) (stdout, stderr string) {
	t.Helper()
	args := append([]string{"replay", "--session", session}, extra...)
	cmd := exec.Command(hx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("hx replay 실패: %v\nstderr:\n%s", err, errb.String())
	}
	return out.String(), errb.String()
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
