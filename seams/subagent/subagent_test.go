package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

// FakeStore는 테스트 전용 인메모리 store다.
type FakeStore struct {
	mu       sync.Mutex
	events   []gen.EventRecord
	lastSeq  int64
	failKind gen.Kind
}

func (s *FakeStore) LastSeq(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq, nil
}

func (s *FakeStore) Append(ctx context.Context, rec gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Kind == s.failKind {
		return errors.New("injected store failure")
	}
	s.events = append(s.events, rec)
	s.lastSeq = rec.Seq
	return nil
}

type fakeDecider struct {
	decision policy.ApprovalDecision
	err      error
	entered  chan policy.ApprovalRequest
	release  <-chan struct{}
}

func (d *fakeDecider) Decide(ctx context.Context, req policy.ApprovalRequest) (policy.ApprovalDecision, error) {
	if d.entered != nil {
		d.entered <- req
	}
	if d.release != nil {
		select {
		case <-d.release:
		case <-ctx.Done():
			return policy.ApprovalDecision{}, ctx.Err()
		}
	}
	return d.decision, d.err
}

const approvalRequestID = "11111111-1111-4111-8111-111111111111"

func approvalAdapterScript(emitMessage bool) string {
	message := ""
	if emitMessage {
		message = `printf '%s\n' '{"v":1,"kind":"subagent/message","payload":{"text":"pump-progress"},"raw":""}'` + "\n"
	}
	return `read task
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/approval_request","payload":{"request_id":"` + approvalRequestID + `","call_id":"call-1","name":"Bash","args":{"command":"true"}},"raw":"eyJ0b29sX25hbWUiOiJCYXNoIn0="}'
` + message + `read response
printf '%s' "$response" > "$1"
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"approved"},"raw":""}'`
}

func decodeApprovalResponse(t *testing.T, path string) gen.ApprovalResponsePayload {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var b []byte
	var err error
	for time.Now().Before(deadline) {
		b, err = os.ReadFile(path)
		if err == nil && len(b) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || len(b) == 0 {
		t.Fatalf("approval response not captured: %v", err)
	}
	var cmd gen.Command
	if err := json.Unmarshal(b, &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Cmd != gen.CommandCmdApprovalResponse {
		t.Fatalf("cmd=%s", cmd.Cmd)
	}
	var response gen.ApprovalResponsePayload
	if err := json.Unmarshal(cmd.Payload, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func spawnApprovalAdapter(t *testing.T, store *FakeStore, mode policy.ApprovalMode, decider policy.ApprovalDecider, emitMessage bool) (*Subagent, *logd.Writer, string) {
	t.Helper()
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	responsePath := filepath.Join(t.TempDir(), "response.json")
	spec := spawnSpec([]string{"/bin/sh", "-c", approvalAdapterScript(emitMessage), "sh", responsePath}, "지시")
	spec.ProfileID = "profile-1"
	spec.Approval = mode
	spec.Decider = decider
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1, spec)
	if err != nil {
		w.Close()
		t.Fatal(err)
	}
	return sub, w, responsePath
}

func (s *FakeStore) AppendBatch(ctx context.Context, recs []gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range recs {
		s.events = append(s.events, rec)
		s.lastSeq = rec.Seq
	}
	return nil
}

func (s *FakeStore) ReadFrom(ctx context.Context, fromSeq int64) ([]gen.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]gen.EventRecord{}, s.events...)
	return out, nil
}

func (s *FakeStore) Close() error { return nil }

func buildNullAdapter(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "nulladapter")
	if out, err := exec.Command("go", "build", "-o", bin, "./nulladapter").CombinedOutput(); err != nil {
		t.Fatalf("빌드 실패: %v\n%s", err, out)
	}
	return bin
}

func spawnSpec(cmd []string, instruction string) Spec {
	return Spec{
		Adapter: "null", Command: cmd, Instruction: instruction,
		Workspace: "/workspace",
		Budget:    gen.Budget{Tokens: 1000, TimeMs: 1000, MaxDepth: 2},
		Depth:     0,
	}
}

// FR-ADP-02의 stop: 어댑터는 done(stopped)으로 응답하고 결과가 기록된다.
func TestStopPath(t *testing.T) {
	// stop만 보내는 시나리오: 어댑터가 task 없이 stop을 받도록 별도 spawn
	// 흐름이 필요하므로, task 후 즉시 stop이 아닌 stop-우선 시나리오는
	// null 어댑터의 결정적 각본상 불가 — 대신 sh 기반 즉답 어댑터로 검증.
	script := `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
read line
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"stopped","result":"중단됨"},"raw":""}'`
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Stop(gen.StopPayloadReasonUser); err != nil {
		t.Fatal(err)
	}
	done, err := sub.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != gen.DonePayloadStatusStopped {
		t.Fatalf("status = %s (stopped 기대)", done.Status)
	}
}

func TestApprovalPolicyModesAndDurableAttribution(t *testing.T) {
	cases := []struct {
		name       string
		mode       policy.ApprovalMode
		decider    policy.ApprovalDecider
		decision   gen.ApprovalResponsePayloadDecision
		reasonPart string
	}{
		{"explicit auto", policy.ApprovalAuto, nil, gen.ApprovalResponsePayloadDecisionAllow, ""},
		{"manual nil defaults deny", policy.ApprovalManual, nil, gen.ApprovalResponsePayloadDecisionDeny, "승인 결정자 미배선"},
		{"manual decider allow", policy.ApprovalManual, &fakeDecider{decision: policy.ApprovalDecision{Allow: true}}, gen.ApprovalResponsePayloadDecisionAllow, ""},
		{"decider error denies", policy.ApprovalManual, &fakeDecider{err: errors.New("ui unavailable")}, gen.ApprovalResponsePayloadDecisionDeny, "ui unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &FakeStore{}
			sub, w, responsePath := spawnApprovalAdapter(t, store, tc.mode, tc.decider, false)
			defer w.Close()
			if _, err := sub.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			response := decodeApprovalResponse(t, responsePath)
			if response.RequestID != approvalRequestID || response.Decision != tc.decision {
				t.Fatalf("response=%+v", response)
			}
			if tc.reasonPart != "" && (response.Reason == nil || !strings.Contains(*response.Reason, tc.reasonPart)) {
				t.Fatalf("reason=%v want %q", response.Reason, tc.reasonPart)
			}

			events, _ := store.ReadFrom(context.Background(), 1)
			var request, decision *gen.EventRecord
			for i := range events {
				switch events[i].Kind {
				case gen.KindSubagentApprovalRequest:
					request = &events[i]
				case gen.KindPolicyDecision:
					decision = &events[i]
				}
			}
			if request == nil || decision == nil || decision.Seq <= request.Seq {
				t.Fatalf("approval request/decision durable order missing: %+v", events)
			}
			if decision.Actor != "parent" || decision.SpanID != request.SpanID || decision.ParentSpanID == nil {
				t.Fatalf("policy attribution request=%+v decision=%+v", request, decision)
			}
			var payload gen.PolicyDecisionPayload
			if err := json.Unmarshal(decision.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.ProfileID != "profile-1" || string(payload.Decision) != string(tc.decision) {
				t.Fatalf("policy payload=%+v", payload)
			}
		})
	}
}

func TestApprovalDecisionDoesNotBlockPump(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan policy.ApprovalRequest, 1)
	decider := &fakeDecider{
		decision: policy.ApprovalDecision{Allow: true}, entered: entered, release: release,
	}
	store := &FakeStore{}
	sub, w, _ := spawnApprovalAdapter(t, store, policy.ApprovalManual, decider, true)
	defer w.Close()
	select {
	case req := <-entered:
		if req.RequestID != approvalRequestID || req.SpanID == "" {
			t.Fatalf("request=%+v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decider not entered")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, _ := store.ReadFrom(context.Background(), 1)
		found := false
		for _, event := range events {
			if event.Kind == gen.KindSubagentMessage {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("approval decision blocked stdout pump")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if _, err := sub.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalEmptyDenyReasonIsFatalAfterForcedDeny(t *testing.T) {
	store := &FakeStore{}
	decider := &fakeDecider{decision: policy.ApprovalDecision{Allow: false}}
	sub, w, responsePath := spawnApprovalAdapter(t, store, policy.ApprovalManual, decider, false)
	defer w.Close()
	if _, err := sub.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "deny reason이 비어 있음") {
		t.Fatalf("empty deny reason not fatal: %v", err)
	}
	response := decodeApprovalResponse(t, responsePath)
	if response.Decision != gen.ApprovalResponsePayloadDecisionDeny || response.Reason == nil || *response.Reason == "" {
		t.Fatalf("forced response=%+v", response)
	}
}

func TestApprovalRecordFailureSendsDenyBeforeTermination(t *testing.T) {
	store := &FakeStore{failKind: gen.KindPolicyDecision}
	sub, w, responsePath := spawnApprovalAdapter(t, store, policy.ApprovalAuto, nil, false)
	defer w.Close()
	if _, err := sub.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "policy/decision 기록") {
		t.Fatalf("record failure not surfaced: %v", err)
	}
	response := decodeApprovalResponse(t, responsePath)
	if response.Decision != gen.ApprovalResponsePayloadDecisionDeny || response.Reason == nil || *response.Reason != "정책 판정 기록 실패" {
		t.Fatalf("record failure response=%+v", response)
	}
}

// §5.2 위반 어댑터(비 NDJSON, raw 누락 등)는 거부된다 — 계약을 어기는
// 어댑터는 등록될 수 없다의 런타임 판.
func TestContractViolatingAdapterRejected(t *testing.T) {
	cases := map[string]string{
		"비 JSON 출력": `read line
printf 'not json at all\n'`,
		"raw 누락": `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"}}'`,
		"미지 kind": `read line
printf '%s\n' '{"v":1,"kind":"subagent/spawned","payload":{},"raw":""}'`,
		"done 없이 종료": `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			store := &FakeStore{}
			w, err := logd.NewWriter(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
				spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sub.Wait(context.Background()); err == nil {
				t.Fatal("계약 위반 어댑터가 정상 완료로 처리됨")
			}
		})
	}
}

// T7 재리뷰 차단 1의 회귀: §5.2 시퀀스 위반은 전부 거부된다 (FR-ADP-03).
func TestSequenceViolationsRejected(t *testing.T) {
	cases := map[string]string{
		"ready 없는 done": `read line
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'`,
		"ready 중복": `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'`,
		"done 이후 출력": `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/message","payload":{"text":"유령"},"raw":""}'`,
		"ready 전 중간 이벤트": `read line
printf '%s\n' '{"v":1,"kind":"subagent/message","payload":{"text":"x"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			store := &FakeStore{}
			w, err := logd.NewWriter(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
				spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sub.Wait(context.Background()); err == nil {
				t.Fatal("시퀀스 위반 어댑터가 정상 완료로 처리됨")
			}
		})
	}
}

// T7 재리뷰 차단 2의 회귀 (1): done 이후 프로세스가 늘어져도 Wait(ctx)는
// deadline을 지킨다 — kill 후 즉시 반환, reap은 펌프가 수행.
func TestWaitHonorsContextAfterDone(t *testing.T) {
	script := `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'
sleep 5`
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, waitErr := sub.Wait(ctx)
	elapsed := time.Since(start)
	if waitErr == nil {
		t.Fatal("deadline이 무시되고 성공 반환됨")
	}
	if elapsed > time.Second {
		t.Fatalf("Wait가 %v 소요 — deadline(100ms) 무시", elapsed)
	}
}

// T7 재리뷰 차단 2의 회귀 (2): 검증 실패 후 프로세스가 늘어져도 kill되어
// 빠르게 오류가 반환된다 (fail 후 hang 없음).
func TestInvalidEventKillsLingeringProcess(t *testing.T) {
	script := `read line
printf 'not json\n'
sleep 5`
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, waitErr := sub.Wait(context.Background())
	if waitErr == nil {
		t.Fatal("검증 실패가 성공으로 처리됨")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("kill이 동작하지 않아 %v 소요", time.Since(start))
	}
}

// T7 재리뷰 차단 2의 회귀 (3): done 이후 비정상 exit 코드는 보존된다.
func TestAbnormalExitAfterDonePreserved(t *testing.T) {
	script := `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'
exit 3`
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "비정상 종료") {
		t.Fatalf("exit 오류가 보존되지 않음: %v", err)
	}
}

// T7 재재리뷰 차단 1의 회귀 (1): 어댑터 본체가 정상 종료했는데 자손이
// stdout을 쥐고 있어도 종료 관측은 EOF에 인질 잡히지 않는다 —
// Wait는 deadline 안에 정상 결과를 반환한다.
func TestExitObservationIndependentOfStdoutEOF(t *testing.T) {
	script := `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'
sleep 2 &
exit 0`
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	done, waitErr := sub.Wait(ctx)
	if waitErr != nil {
		t.Fatalf("자손의 파이프 점유로 정상 결과가 유실됨: %v (elapsed=%v)", waitErr, time.Since(start))
	}
	if done.Status != gen.DonePayloadStatusOk {
		t.Fatalf("status = %s", done.Status)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("종료 관측이 EOF에 종속됨: %v 소요", time.Since(start))
	}
}

// T7 재재리뷰 차단 1의 회귀 (2): Spawn context 취소는 리더만이 아니라
// 프로세스 그룹 전체를 죽인다 — 자손이 남아 Wait를 지연시키지 않는다.
func TestSpawnContextCancelKillsGroup(t *testing.T) {
	script := `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
sleep 2 &
wait`
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	spawnCtx, cancel := context.WithCancel(context.Background())
	sub, err := Spawn(spawnCtx, w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
	if err != nil {
		t.Fatal(err)
	}
	// ready가 기록될 때까지 대기 후 취소
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, _ := store.ReadFrom(context.Background(), 1)
		found := false
		for _, e := range events {
			if e.Kind == gen.KindSubagentReady {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ready가 기록되지 않음")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	start := time.Now()
	if _, waitErr := sub.Wait(context.Background()); waitErr == nil {
		t.Fatal("취소됐는데 정상 완료로 처리됨")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("취소가 그룹을 죽이지 못해 %v 대기 — 자손 잔존", time.Since(start))
	}
}

// T7 재재리뷰 차단 2의 회귀: 공백 줄은 상태(ready 전·중간·done 후)와
// 무관하게 §5.2 위반이다 — post-done 공백이 시퀀스 검사를 우회하지 않는다.
func TestBlankLinesAreViolations(t *testing.T) {
	cases := map[string]string{
		"ready 전 공백": `read line
printf '\n'
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'`,
		"중간 공백": `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '\n'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'`,
		"done 후 공백": `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'
printf '\n'`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			store := &FakeStore{}
			w, err := logd.NewWriter(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
				spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sub.Wait(context.Background()); err == nil {
				t.Fatal("공백 줄이 §5.2 검사를 우회함")
			}
		})
	}
}

// T7 재재재리뷰 차단 2의 회귀: 리더 종료 시 잔여 그룹이 즉시 종료된다 —
// done 이후 0.4초 뒤에 출력하려던 자손은 실행 자체가 남지 않는다
// (유예 기반 마감의 fail-open 소멸). 재현 스크립트 그대로.
func TestLeaderExitTerminatesRemainingGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "grandchild-ran")
	script := `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{"grade":"observable"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'
( sleep 0.4; printf '%s\n' '{"v":1,"kind":"subagent/message","payload":{"text":"유령"},"raw":""}'; touch "` + marker + `" ) &
exit 0`
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	done, waitErr := sub.Wait(context.Background())
	if waitErr != nil || done.Status != gen.DonePayloadStatusOk {
		t.Fatalf("정상 결과 아님: %+v %v", done, waitErr)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("EOF 종속 잔존: %v 소요", time.Since(start))
	}
	// 자손이 살아남았다면 0.4초 뒤 마커를 만든다 — 만들어지면 안 된다
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("리더 종료 후에도 자손이 실행됨 — 그룹 종료 실패 (fail-open)")
	}
}

// T7 재재재리뷰 차단 1의 회귀 (1): 정상 완료 후 부모 측 파이프가 닫힌다.
func TestPipesClosedAfterCompletion(t *testing.T) {
	bin := buildNullAdapter(t)
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{bin}, "요청"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	// EPIPE/EOF가 아니라 실제 Close 상태(os.ErrClosed)임을 단정 —
	// Close 제거 회귀를 정확히 잡는다.
	if _, err := sub.proc.StatStdin(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("완료 후 stdin이 닫힌 상태가 아님: %v", err)
	}
	if _, err := sub.proc.StatStdout(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("완료 후 stdout이 닫힌 상태가 아님: %v", err)
	}
}

// T7 재재재리뷰 차단 1의 회귀 (2): 취소 가능한 Spawn을 반복해도
// goroutine이 누적되지 않는다 (watchCtx 누수의 재발 방지).
func TestNoGoroutineAccumulationAcrossSpawns(t *testing.T) {
	bin := buildNullAdapter(t)
	settle := func() int {
		best := 1 << 30
		for i := 0; i < 20; i++ {
			runtime.GC()
			n := runtime.NumGoroutine()
			if n < best {
				best = n
			}
			time.Sleep(20 * time.Millisecond)
		}
		return best
	}
	baseline := settle()
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		store := &FakeStore{}
		w, err := logd.NewWriter(context.Background(), store)
		if err != nil {
			t.Fatal(err)
		}
		sub, err := Spawn(ctx, w, logd.NewTraceID(), logd.NewSpanID(), 1, spawnSpec([]string{bin}, "요청"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sub.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		cancel()
		w.Close()
	}
	after := settle()
	if after > baseline+3 {
		t.Fatalf("goroutine 누적: %d → %d (허용 +3)", baseline, after)
	}
}

// T7 재재재리뷰 차단 1의 회귀 (3): task 전송 실패 경로도 프로세스·파이프·
// goroutine이 정리된다.
func TestSpawnSendFailureCleansUp(t *testing.T) {
	bin := buildNullAdapter(t)
	settle := func() int {
		best := 1 << 30
		for i := 0; i < 20; i++ {
			runtime.GC()
			n := runtime.NumGoroutine()
			if n < best {
				best = n
			}
			time.Sleep(20 * time.Millisecond)
		}
		return best
	}
	baseline := settle()
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// 빈 instruction → 발신 task가 계약 위반(minLength 1) → 전송 실패 경로
	if _, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{bin}, "")); err == nil {
		t.Fatal("계약 위반 task가 전송됨")
	}
	after := settle()
	if after > baseline+3 {
		t.Fatalf("실패 경로 goroutine 누적: %d → %d", baseline, after)
	}
}

// null 어댑터 전 이벤트가 child span + subagent actor로 기록되고
// usage가 envelope에 집계된다 (FR-ADP-03/04/07, FR-LOG-10 전제).
func TestNullAdapterNormalization(t *testing.T) {
	bin := buildNullAdapter(t)
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	trace, root := logd.NewTraceID(), logd.NewSpanID()
	sub, err := Spawn(context.Background(), w, trace, root, 1, spawnSpec([]string{bin}, "요청"))
	if err != nil {
		t.Fatal(err)
	}
	done, err := sub.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != gen.DonePayloadStatusOk || !strings.Contains(done.Result, "요청") {
		t.Fatalf("done 이상: %+v", done)
	}
	events, _ := store.ReadFrom(context.Background(), 1)
	if events[0].Kind != gen.KindSubagentSpawn {
		t.Fatalf("첫 이벤트 %s (spawn 기대)", events[0].Kind)
	}
	child := events[0].SpanID
	if child == root {
		t.Fatal("child span이 발급되지 않음")
	}
	var sawUsage bool
	for _, e := range events[1:] {
		if e.SpanID != child || e.ParentSpanID == nil || *e.ParentSpanID != root {
			t.Fatalf("%s: span 귀속 이상", e.Kind)
		}
		if !strings.HasPrefix(e.Actor, "subagent:null:1") {
			t.Fatalf("%s: actor = %s", e.Kind, e.Actor)
		}
		if e.Raw == nil {
			t.Fatalf("%s: raw 누락 (FR-ADP-04)", e.Kind)
		}
		if e.Kind == gen.KindSubagentUsage {
			if e.UsageIn == nil || *e.UsageIn != 12 || e.UsageOut == nil || *e.UsageOut != 34 {
				t.Fatalf("usage envelope 집계 안 됨: %+v", e)
			}
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Fatal("usage 이벤트 없음")
	}
}
