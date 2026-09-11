package main

// T17 완료 기준 테스트 (macOS `make ci` 대상):
//
//	(a) 같은 key+fingerprint 재요청 = 무spawn 동일 세션, 다른 fingerprint = KEY_CONFLICT
//	(b) 동시 두 요청에서 launch는 정확히 하나
//	(c) 삭제된 세션 key 재사용이 tombstone으로 거부
//	(d) 정책 파일 변조 시 POLICY_CHANGED 거부
//
// world 조립 자체는 Linux 게이트(worldintegration/t15integration)와 T17
// 생산 launcher 경로가 담당한다 — 여기서는 접수 파이프라인이 launcher
// 인터페이스 앞에서 멈추는 것을 fake로 검증한다.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/seams/accept"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

const testProfileYAML = `id: prod-base
fs_scope:
  - /hxws
egress:
  - example.com
budget:
  tokens: 1000
  time_ms: 60000
  max_depth: 2
approval: manual
`

type fakeLauncher struct {
	calls  atomic.Int64
	status gen.DonePayloadStatus

	mu   sync.Mutex
	last sessionLaunch
}

func (f *fakeLauncher) Launch(_ context.Context, in sessionLaunch) (gen.DonePayload, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.last = in
	f.mu.Unlock()
	status := f.status
	if status == "" {
		status = gen.DonePayloadStatusOk
	}
	return gen.DonePayload{Status: status, Result: "fake done"}, nil
}

type productionFixture struct {
	profilePath string
	acceptRoot  string
	launcher    *fakeLauncher
	request     runRequest
}

func newProductionFixture(t *testing.T) *productionFixture {
	t.Helper()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profile.yaml")
	if err := os.WriteFile(profilePath, []byte(testProfileYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	profileHash, err := pinnedProfileHash(profilePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &productionFixture{
		profilePath: profilePath,
		acceptRoot:  filepath.Join(dir, "acceptance"),
		launcher:    &fakeLauncher{},
		request: runRequest{
			Version: 1, OperationID: "op-1", Scope: "rhizome-test/executor-a",
			IdempotencyKey: "key-1", RequestFingerprint: strings.Repeat("ab", 32),
			RhizomeExecutionID: "exec-1", AdapterID: "claudecode",
			WorkspaceRef: "/hxws/job", ProfileID: "prod-base", ProfileHash: profileHash,
			TaskRef: runTaskRef{Instruction: "respond OK"},
			Budget:  requestBudget{Tokens: 500, TimeMs: 30000, MaxDepth: 1},
		},
	}
}

func (f *productionFixture) run(t *testing.T, req runRequest) (string, error) {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runProduction(context.Background(), productionRun{
		RequestBytes: data, ProfilePath: f.profilePath, AcceptRoot: f.acceptRoot,
		Launcher: f.launcher, Stdout: &out,
	})
	return out.String(), err
}

func decodeControls(t *testing.T, out string) []controlMessage {
	t.Helper()
	var msgs []controlMessage
	for i, line := range nonEmptyLines(out) {
		var msg controlMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("stdout %d행이 NDJSON 제어 메시지가 아님: %v (%q)", i, err, line)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func TestProductionAcceptThenLaunch(t *testing.T) {
	f := newProductionFixture(t)
	out, err := f.run(t, f.request)
	if err != nil {
		t.Fatalf("생산 실행 실패: %v\n%s", err, out)
	}
	msgs := decodeControls(t, out)
	if len(msgs) != 2 {
		t.Fatalf("접수+terminal 두 메시지가 필요: %d\n%s", len(msgs), out)
	}
	acc := msgs[0]
	if acc.Status != "accepted" || acc.SessionRef == nil || acc.AcceptanceSeq != 1 {
		t.Fatalf("접수 메시지 위반: %+v", acc)
	}
	if len(acc.SessionRef.TraceID) != 32 {
		t.Fatalf("trace_id는 32자리 hex여야 함: %q", acc.SessionRef.TraceID)
	}
	if acc.PolicyHash == "" || acc.RequestFingerprint != f.request.RequestFingerprint {
		t.Fatalf("접수 메시지에 policy_hash/fingerprint 필수: %+v", acc)
	}
	if msgs[1].Status != "terminal" || msgs[1].Done == nil || msgs[1].Done.Status != "ok" {
		t.Fatalf("terminal 메시지 위반: %+v", msgs[1])
	}
	if got := f.launcher.calls.Load(); got != 1 {
		t.Fatalf("launch 횟수 = %d, want 1", got)
	}
	// 좁힘 불변식: 실효 budget은 병합 정책(1000/60000/2)이 아니라 그보다
	// 좁은 요청 값이어야 하고, 승인 모드는 manual 그대로다.
	f.launcher.mu.Lock()
	launched := f.launcher.last
	f.launcher.mu.Unlock()
	if launched.Sandbox.Budget != (gen.Budget{Tokens: 500, TimeMs: 30000, MaxDepth: 1}) {
		t.Fatalf("실효 budget이 요청 값으로 좁혀져야 함: %+v", launched.Sandbox.Budget)
	}
	if launched.Sandbox.Workspace != f.request.WorkspaceRef {
		t.Fatalf("정책 sandbox workspace가 요청 workspace와 달라짐: %q != %q", launched.Sandbox.Workspace, f.request.WorkspaceRef)
	}
	if string(launched.Sandbox.Approval) != "manual" {
		t.Fatalf("승인 모드가 manual이어야 함: %q", launched.Sandbox.Approval)
	}

	// 접수 binding이 세션 로그에서 공개 관측 가능해야 한다(hx replay 표면).
	log, err := sqlite.Open(context.Background(), acc.SessionRef.SessionDB)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	events, err := log.Reader.ReadFrom(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Kind != gen.KindSessionStart {
		t.Fatalf("첫 이벤트가 session/start여야 함: %+v", events)
	}
	var payload struct {
		Binding executionBinding `json:"execution_binding"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Binding.PolicyHash != acc.PolicyHash ||
		payload.Binding.RequestFingerprint != f.request.RequestFingerprint ||
		payload.Binding.IdempotencyKey != f.request.IdempotencyKey {
		t.Fatalf("binding payload가 접수 응답과 일치해야 함: %+v", payload.Binding)
	}
	if events[len(events)-1].Kind != gen.KindSessionEnd {
		t.Fatalf("마지막 이벤트가 session/end여야 함: %+v", events[len(events)-1])
	}
}

// (a) 같은 key+fingerprint = 무spawn 동일 세션 / 다른 fingerprint = KEY_CONFLICT.
func TestProductionIdempotentReplayAndConflict(t *testing.T) {
	f := newProductionFixture(t)
	first, err := f.run(t, f.request)
	if err != nil {
		t.Fatal(err)
	}
	firstTrace := decodeControls(t, first)[0].SessionRef.TraceID

	out, err := f.run(t, f.request)
	if err != nil {
		t.Fatalf("동일 요청 재실행은 성공(무spawn)이어야 함: %v", err)
	}
	msgs := decodeControls(t, out)
	if len(msgs) != 1 || msgs[0].Status != "accepted" || !msgs[0].Duplicate {
		t.Fatalf("중복 접수 응답 위반: %+v", msgs)
	}
	if msgs[0].SessionRef.TraceID != firstTrace {
		t.Fatalf("동일 세션 반환이어야 함: %s != %s", msgs[0].SessionRef.TraceID, firstTrace)
	}
	if msgs[0].LaunchClaimed == nil || !*msgs[0].LaunchClaimed {
		t.Fatalf("launch claim 상태가 관측돼야 함: %+v", msgs[0])
	}
	if got := f.launcher.calls.Load(); got != 1 {
		t.Fatalf("재요청이 spawn을 만들면 안 됨: launch %d회", got)
	}

	conflicting := f.request
	conflicting.RequestFingerprint = strings.Repeat("cd", 32)
	out, err = f.run(t, conflicting)
	var rerr *runError
	if !errors.As(err, &rerr) || rerr.Code != codeKeyConflict {
		t.Fatalf("다른 fingerprint는 KEY_CONFLICT여야 함: %v\n%s", err, out)
	}
	if got := f.launcher.calls.Load(); got != 1 {
		t.Fatalf("conflict가 spawn을 만들면 안 됨: launch %d회", got)
	}
}

// (b) 동시 요청에서 launch는 정확히 하나.
func TestProductionConcurrentSingleLaunch(t *testing.T) {
	f := newProductionFixture(t)
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 오류 무시: 지는 쪽은 initializing(무오류) 또는 진행 중 상태를
			// 본다. 단정은 launch 총횟수 하나로 충분하다.
			_, _ = f.run(t, f.request)
		}()
	}
	wg.Wait()
	if got := f.launcher.calls.Load(); got != 1 {
		t.Fatalf("동시 %d요청의 launch 총횟수 = %d, want 1", n, got)
	}
}

// (c) 삭제된 세션의 key 재사용은 tombstone으로 거부, 재초기화 없음.
func TestProductionTombstoneAfterSessionDeletion(t *testing.T) {
	f := newProductionFixture(t)
	out, err := f.run(t, f.request)
	if err != nil {
		t.Fatal(err)
	}
	sessionPath := decodeControls(t, out)[0].SessionRef.SessionDB
	// SQLite 부속 파일까지 제거해 "DB 삭제"를 재현한다.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(sessionPath + suffix)
	}
	out, err = f.run(t, f.request)
	var rerr *runError
	if !errors.As(err, &rerr) || rerr.Code != codeAcceptanceUnknown {
		t.Fatalf("삭제된 세션 재요청은 ACCEPTANCE_UNKNOWN이어야 함: %v\n%s", err, out)
	}
	msgs := decodeControls(t, out)
	if len(msgs) != 1 || msgs[0].Status != "unknown" {
		t.Fatalf("unknown 상태 응답이어야 함: %+v", msgs)
	}
	if _, statErr := os.Stat(sessionPath); !os.IsNotExist(statErr) {
		t.Fatalf("재요청이 세션 파일을 재생성하면 안 됨: %v", statErr)
	}
	if got := f.launcher.calls.Load(); got != 1 {
		t.Fatalf("tombstone 이후 spawn 금지: launch %d회", got)
	}
}

// (d) 사전 pin 이후 정책 파일이 바뀌면 POLICY_CHANGED, claim 없음.
func TestProductionPolicyChangedRejected(t *testing.T) {
	f := newProductionFixture(t)
	changed := strings.Replace(testProfileYAML, "tokens: 1000", "tokens: 999999", 1)
	if err := os.WriteFile(f.profilePath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(t, f.request)
	var rerr *runError
	if !errors.As(err, &rerr) || rerr.Code != codePolicyChanged {
		t.Fatalf("정책 파일 변조는 POLICY_CHANGED여야 함: %v\n%s", err, out)
	}
	registry, err := accept.Open(f.acceptRoot)
	if err != nil {
		t.Fatal(err)
	}
	status, err := registry.Lookup(f.request.Scope, f.request.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "not_submitted" {
		t.Fatalf("거부된 요청은 key를 점유하면 안 됨: %+v", status)
	}
	if got := f.launcher.calls.Load(); got != 0 {
		t.Fatalf("거부된 요청의 launch = %d, want 0", got)
	}
}

func TestProductionRequestValidation(t *testing.T) {
	f := newProductionFixture(t)
	cases := []struct {
		name   string
		mutate func(*runRequest)
		code   string
	}{
		{"미지원 버전", func(r *runRequest) { r.Version = 2 }, codeUnsupportedContract},
		{"미승인 어댑터", func(r *runRequest) { r.AdapterID = "hifipi" }, codeUnsupportedAdapter},
		{"budget 0", func(r *runRequest) { r.Budget.Tokens = 0 }, codeBudgetInvalid},
		{"budget 초과", func(r *runRequest) { r.Budget.Tokens = 5000 }, codeBudgetInvalid},
		{"profile_id 불일치", func(r *runRequest) { r.ProfileID = "other" }, codePolicyChanged},
		{"워크스페이스 스코프 밖", func(r *runRequest) { r.WorkspaceRef = "/etc" }, codePolicyDenied},
		{"fingerprint 형식", func(r *runRequest) { r.RequestFingerprint = "XYZ" }, codeUnsupportedContract},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := f.request
			tc.mutate(&req)
			out, err := f.run(t, req)
			var rerr *runError
			if !errors.As(err, &rerr) || rerr.Code != tc.code {
				t.Fatalf("code=%v want %s\n%s", err, tc.code, out)
			}
			msgs := decodeControls(t, out)
			if len(msgs) != 1 || msgs[0].Status != "rejected" || msgs[0].Error == nil || msgs[0].Error.Code != tc.code {
				t.Fatalf("rejected 제어 메시지 위반: %+v", msgs)
			}
		})
	}
	if got := f.launcher.calls.Load(); got != 0 {
		t.Fatalf("거부 요청의 launch = %d, want 0", got)
	}
}

func TestProductionSessionArgMustMatchDerivedPath(t *testing.T) {
	f := newProductionFixture(t)
	data, err := json.Marshal(f.request)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runProduction(context.Background(), productionRun{
		RequestBytes: data, ProfilePath: f.profilePath, AcceptRoot: f.acceptRoot,
		SessionArg: "/tmp/other.sqlite", Launcher: f.launcher, Stdout: &out,
	})
	var rerr *runError
	if !errors.As(err, &rerr) || rerr.Code != codeKeyConflict {
		t.Fatalf("정본 경로 불일치는 KEY_CONFLICT여야 함: %v", err)
	}
}

// 실패한 launch도 접수·세션 종료·terminal 오류 메시지를 남긴다.
func TestProductionLaunchFailureLeavesTerminalError(t *testing.T) {
	f := newProductionFixture(t)
	failing := &failingLauncher{}
	data, err := json.Marshal(f.request)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runProduction(context.Background(), productionRun{
		RequestBytes: data, ProfilePath: f.profilePath, AcceptRoot: f.acceptRoot,
		Launcher: failing, Stdout: &out,
	})
	if err == nil {
		t.Fatal("launch 실패는 오류로 끝나야 함")
	}
	msgs := decodeControls(t, out.String())
	if len(msgs) != 2 || msgs[0].Status != "accepted" ||
		msgs[1].Status != "terminal" || msgs[1].Error == nil || msgs[1].Error.Code != codeLaunchFailed {
		t.Fatalf("접수+terminal(LAUNCH_FAILED) 순서 위반: %+v", msgs)
	}
}

type failingLauncher struct{}

func (failingLauncher) Launch(context.Context, sessionLaunch) (gen.DonePayload, error) {
	return gen.DonePayload{}, errors.New("world 조립 실패 (모의)")
}
