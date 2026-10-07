package claudecode

import (
	"encoding/base64"
	"encoding/json"

	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/seams/subagent/internal/procgroup"
)

// T27(d) root-cause 회귀: native result가 이미 파싱돼 pending done(ok)이 있고
// 프로세스가 정상 종료(ExitErr=nil)했더라도, done 방출 전에 접수된 stop은
// done을 stopped로 만든다. 수리 전 finishNative는 ExitErr가 있을 때만 stop을
// 반영해, "deny → native 정상 종료"가 kill보다 빠른 창에서 ok가 나갔다.
func TestFinishNativeStopBeforeDoneOverridesCleanNativeResult(t *testing.T) {
	for _, status := range []gen.DonePayloadStatus{gen.DonePayloadStatusOk, gen.DonePayloadStatusError} {
		t.Run(string(status), func(t *testing.T) {
			payload, err := json.Marshal(gen.DonePayload{Status: status, Result: "2"})
			if err != nil {
				t.Fatal(err)
			}
			pending := &Event{Kind: gen.EventKindSubagentDone, Payload: payload, Raw: []byte(`{"type":"result"}`)}
			for _, stop := range []bool{false, true} {
				event, err := finishNative(procgroup.DrainResult{}, pending, stop)
				if err != nil {
					t.Fatal(err)
				}
				var got gen.DonePayload
				if err := json.Unmarshal(event.Payload, &got); err != nil {
					t.Fatal(err)
				}
				want := status
				if stop {
					want = gen.DonePayloadStatusStopped
				}
				if got.Status != want || got.Result != "2" || string(event.Raw) != `{"type":"result"}` {
					t.Fatalf("stop=%v done=%+v raw=%q want status=%s", stop, got, event.Raw, want)
				}
			}
		})
	}
}

// T27(d): stop 접수(NoteStop)와 terminal 봉인(SealTerminal)은 전순서다.
// 봉인 전 접수 → 봉인값 true, 봉인 후 stop → 미접수(false)이고 봉인값 불변.
func TestParserStopSealIsTotalOrder(t *testing.T) {
	p := NewParser()
	if !p.NoteStop() {
		t.Fatal("봉인 전 stop이 접수되지 않음")
	}
	if !p.SealTerminal() {
		t.Fatal("봉인 전 접수된 stop이 봉인값에 반영되지 않음")
	}

	p = NewParser()
	if p.SealTerminal() {
		t.Fatal("stop 없는 봉인이 stopped")
	}
	if p.NoteStop() {
		t.Fatal("봉인 후 stop이 접수됨 — 이미 결정된 done을 바꿀 수 있음")
	}
	if p.stopRequested.Load() {
		t.Fatal("미접수 stop이 상태에 남음")
	}
}

// 동시 경합에서도 "NoteStop이 true를 돌려줬다 ⇔ 봉인값 true"가 성립한다.
func TestParserStopSealConcurrentAgreement(t *testing.T) {
	for i := 0; i < 2000; i++ {
		p := NewParser()
		var accepted bool
		var sealed bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); accepted = p.NoteStop() }()
		go func() { defer wg.Done(); sealed = p.SealTerminal() }()
		wg.Wait()
		if accepted != sealed {
			t.Fatalf("iter %d: stop 접수=%v 봉인값=%v 불일치", i, accepted, sealed)
		}
	}
}

// T27(a) 프로세스 관통: 실행 파일 어댑터가 합성 API 오류를 wire 계약 안에서
// ready → usage{0,0}(raw=합성 줄) → done{error, 사유∋텍스트}로 방출한다.
// 입력은 이 테스트의 인라인 NDJSON을 임시 파일로 둔 것이다(fixtures/ 무관).
func TestAdapterExecutableSyntheticAPIErrorNotModelMessage(t *testing.T) {
	bins := buildAdapterBinaries(t)
	input := strings.Join([]string{initLine, syntheticAuthBoth, authErrResultLine}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "synthetic.ndjson")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	run := runFixtureProcess(t, bins, path, nil, nil)
	if run.err != nil {
		t.Fatalf("adapter exit: %v\n%s", run.err, run.stderr)
	}
	var kinds []string
	for _, ev := range run.events {
		kinds = append(kinds, string(ev.Kind))
		if ev.Kind == gen.EventKindSubagentMessage {
			t.Fatalf("합성 API 오류가 subagent/message로 방출됨: %s", ev.Payload)
		}
	}
	if strings.Join(kinds, " ") != "subagent/ready subagent/usage subagent/done" {
		t.Fatalf("kinds=%v", kinds)
	}
	if run.events[1].Raw != base64.StdEncoding.EncodeToString([]byte(syntheticAuthBoth)) {
		t.Fatalf("합성 줄 raw 미보존: %q", run.events[1].Raw)
	}
	assertLastDone(t, run.events, gen.DonePayloadStatusError,
		"Failed to authenticate. API Error: 403 egress denied",
		base64.StdEncoding.EncodeToString([]byte(authErrResultLine)))
}
