package main

// T17 생산 실행 경로: request.json 검증 → 정책 pin 재검증 → scoped key
// claim → durable 접수 binding → launch claim → production world 조립.
//
// 순서 불변식(계약 §3.3): key 배타 점유 → 접수 binding durable → session DB
// 초기화 → 접수 응답 → launch claim durable → 외부 효과(컨테이너/어댑터).
// 샌드박스 없는 시작 경로는 만들지 않는다 — launcher는 startProductionWorld
// 만 경유한다(결정 시트 2번).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/accept"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
	local "github.com/Eastsidegunn/JANUS/seams/world/local"
)

// runProductionCmd는 생산 경로의 CLI 진입점이다. world backend 구성은
// claim 이전에 끝난다 — world를 조립할 수 없는 호스트는 key를 소모하지
// 않고 실패한다.
func runProductionCmd(requestPath, profilePath string, overlayPaths []string, acceptRoot, worldConfigPath, sessionArg string) error {
	requestBytes, err := os.ReadFile(requestPath)
	if err != nil {
		return fmt.Errorf("request 읽기: %w", err)
	}
	worldBytes, err := os.ReadFile(worldConfigPath)
	if err != nil {
		return fmt.Errorf("world config 읽기: %w", err)
	}
	cfg, err := parseWorldConfig(worldBytes)
	if err != nil {
		return err
	}
	launcher, err := newWorldLauncher(cfg)
	if err != nil {
		return err
	}
	return runProduction(context.Background(), productionRun{
		RequestBytes: requestBytes, ProfilePath: profilePath, OverlayPaths: overlayPaths,
		AcceptRoot: acceptRoot, SessionArg: sessionArg, Launcher: launcher, Stdout: os.Stdout,
	})
}

// sessionLauncher는 접수가 durable해진 뒤의 실행 단계다. 인터페이스로
// 분리한 이유는 검증 현실이다: 접수·claim·정책 파이프라인은 macOS
// `make ci`에서 단위 검증하고, 실제 world 조립은 Linux 게이트가 검증한다.
// 생산 구현은 worldLauncher 하나뿐이며 startProductionWorld만 경유한다.
type sessionLauncher interface {
	Launch(ctx context.Context, in sessionLaunch) (gen.DonePayload, error)
}

type sessionLaunch struct {
	Log      *sqlite.Log
	TraceID  string
	RootSpan string
	Sandbox  policy.SandboxConfig
	Request  runRequest
}

type productionRun struct {
	RequestBytes []byte
	ProfilePath  string
	OverlayPaths []string
	AcceptRoot   string
	SessionArg   string // --session 제공 시 정본 경로와 일치 검증
	Launcher     sessionLauncher
	Stdout       io.Writer
	Now          func() int64
}

// executionBinding은 session/start payload에 기록되는 접수 binding이다.
// Rhizome은 `hx replay`로 이 값을 관측해 세션 튜플·policy hash·요청
// fingerprint를 자기 binding 기록과 대조한다(계약 §2).
type executionBinding struct {
	OperationID        string `json:"operation_id"`
	Scope              string `json:"scope"`
	IdempotencyKey     string `json:"idempotency_key"`
	RequestFingerprint string `json:"request_fingerprint"`
	PolicyHash         string `json:"policy_hash"`
	ProfileID          string `json:"profile_id"`
	AdapterID          string `json:"adapter_id"`
	WorkspaceRef       string `json:"workspace_ref"`
	RhizomeExecutionID string `json:"rhizome_execution_id"`
	MissionID          string `json:"mission_id,omitempty"`
	CorrelationID      string `json:"correlation_id,omitempty"`
}

// acceptanceSeq: 접수 binding은 배타적 초기화 배치의 첫 이벤트(session/start)
// 에 실리므로 seq는 항상 1이다.
const acceptanceSeq = 1

// runProduction은 T17 접수 파이프라인의 전체다. 반환 오류는 종료 코드
// 1을 뜻하고, 모든 제어 응답은 Stdout NDJSON으로 이미 방출된 상태다.
func runProduction(ctx context.Context, run productionRun) error {
	if run.Now == nil {
		run.Now = nowMs
	}
	req, rerr := parseRunRequest(run.RequestBytes)
	if rerr != nil {
		return rejectControl(run.Stdout, req.OperationID, rerr)
	}
	reject := func(rerr *runError) error {
		return rejectControl(run.Stdout, req.OperationID, rerr)
	}

	// 정책 pin 재검증: 사전 dump-config 시점에 고정한 파일 내용 hash와
	// 실행 시점의 파일이 일치해야 한다. pin 없는 사전 출력만으로 실행을
	// 허가하지 않는다(계약 §3.1).
	pinned, err := pinnedProfileHash(run.ProfilePath, run.OverlayPaths)
	if err != nil {
		return reject(newRunError(codeIOError, "%v", err))
	}
	if pinned != req.ProfileHash {
		return reject(newRunError(codePolicyChanged, "정책 파일 hash 불일치: 요청 %s, 현재 %s", req.ProfileHash, pinned))
	}
	base, err := readProfile(run.ProfilePath)
	if err != nil {
		return reject(newRunError(codeIOError, "%v", err))
	}
	overlays := make([]policy.Profile, 0, len(run.OverlayPaths))
	effective := base
	for _, path := range run.OverlayPaths {
		overlay, err := readProfile(path)
		if err != nil {
			return reject(newRunError(codeIOError, "%v", err))
		}
		overlays = append(overlays, overlay)
		effective = policy.Merge(effective, overlay)
	}
	if effective.ID != req.ProfileID {
		return reject(newRunError(codePolicyChanged, "profile_id 불일치: 요청 %q, 병합 결과 %q", req.ProfileID, effective.ID))
	}
	policyHash, err := effectivePolicyHash(base, overlays, req.WorkspaceRef)
	if err != nil {
		return reject(newRunError(codePolicyDenied, "정책 평가 거부: %v", err))
	}
	sandbox, denial := policy.Evaluate(effective, policy.SpawnRequest{
		Adapter: req.AdapterID, Workspace: req.WorkspaceRef,
		Egress: append([]string(nil), effective.Egress...), Depth: 0,
	})
	if denial != nil {
		return reject(newRunError(codePolicyDenied, "%v", denial))
	}
	// 실행 요청 budget은 병합 정책을 넘지 않아야 하며(계약 §3.2), 실효
	// budget은 요청 값이다 — 좁히기만 가능하다.
	if req.Budget.Tokens > sandbox.Budget.Tokens || req.Budget.TimeMs > sandbox.Budget.TimeMs ||
		req.Budget.MaxDepth > sandbox.Budget.MaxDepth {
		return reject(newRunError(codeBudgetInvalid,
			"요청 budget %+v가 병합 정책 budget %+v를 초과", req.Budget, sandbox.Budget))
	}
	sandbox.Budget = gen.Budget{Tokens: req.Budget.Tokens, TimeMs: req.Budget.TimeMs, MaxDepth: req.Budget.MaxDepth}

	registry, err := accept.Open(run.AcceptRoot)
	if err != nil {
		return reject(newRunError(codeIOError, "%v", err))
	}
	sessionPath := registry.SessionPath(req.Scope, req.IdempotencyKey)
	if run.SessionArg != "" && run.SessionArg != sessionPath {
		return reject(newRunError(codeKeyConflict,
			"--session %q이 scoped key의 정본 경로 %q와 다름", run.SessionArg, sessionPath))
	}

	traceID := logd.NewTraceID()
	acc, created, err := registry.Claim(accept.Acceptance{
		Scope: req.Scope, Key: req.IdempotencyKey, Fingerprint: req.RequestFingerprint,
		TraceID: traceID, PolicyHash: policyHash, OperationID: req.OperationID,
		CreatedAtMs: run.Now(),
	})
	switch {
	case errors.Is(err, accept.ErrKeyConflict):
		return reject(newRunError(codeKeyConflict, "%v", err))
	case errors.Is(err, accept.ErrClaimCorrupt):
		return reject(newRunError(codeSessionCorrupt, "%v", err))
	case err != nil:
		return reject(newRunError(codeIOError, "%v", err))
	}
	if !created {
		return replayAcceptance(run.Stdout, req, registry, acc, policyHash, sessionPath)
	}

	// 접수 binding durable → session DB 초기화. 조회가 아니라 최초 접수
	// 경로에서만 DB 파일이 생긴다.
	log, err := sqlite.Open(ctx, sessionPath)
	if err != nil {
		return reject(newRunError(codeIOError, "세션 로그 열기: %v", err))
	}
	defer log.Close()
	rootSpan := logd.NewSpanID()
	bindingPayload, err := json.Marshal(struct {
		ExecutionBinding executionBinding `json:"execution_binding"`
	}{executionBinding{
		OperationID: req.OperationID, Scope: req.Scope, IdempotencyKey: req.IdempotencyKey,
		RequestFingerprint: req.RequestFingerprint, PolicyHash: policyHash,
		ProfileID: effective.ID, AdapterID: req.AdapterID, WorkspaceRef: req.WorkspaceRef,
		RhizomeExecutionID: req.RhizomeExecutionID, MissionID: req.MissionID,
		CorrelationID: req.CorrelationID,
	}})
	if err != nil {
		return reject(newRunError(codeIOError, "%v", err))
	}
	if err := log.Writer.InitBatch(ctx, []gen.EventRecord{{
		Ts: run.Now(), TraceID: traceID, SpanID: rootSpan,
		Kind: gen.KindSessionStart, Actor: "parent", Payload: bindingPayload,
	}}); err != nil {
		if errors.Is(err, logd.ErrDestinationNotEmpty) {
			// claim은 이 프로세스가 방금 배타 점유했는데 DB에 이미 로그가
			// 있다 — claim 없는 쓰기 주체가 있었다는 뜻이므로 corrupt.
			return reject(newRunError(codeSessionCorrupt,
				"claim 없는 기존 로그가 세션 파일 %s에 존재", sessionPath))
		}
		return reject(newRunError(codeIOError, "접수 binding 기록: %v", err))
	}
	if err := registry.MarkDBInitialized(req.Scope, req.IdempotencyKey); err != nil {
		return reject(newRunError(codeIOError, "%v", err))
	}
	launchClaimed := false
	if err := emitControl(run.Stdout, controlMessage{
		OperationID: req.OperationID, Status: "accepted",
		SessionRef:     &sessionRef{SessionDB: sessionPath, TraceID: traceID},
		IdempotencyKey: req.IdempotencyKey, RequestFingerprint: req.RequestFingerprint,
		PolicyHash: policyHash, AcceptanceSeq: acceptanceSeq, LaunchClaimed: &launchClaimed,
	}); err != nil {
		return err
	}

	// 외부 효과 직전의 durable launch claim — 이 지점 이후 어떤 재시도도
	// 같은 key로 두 번째 spawn을 만들 수 없다.
	if err := registry.MarkLaunched(req.Scope, req.IdempotencyKey); err != nil {
		terr := newRunError(codeIOError, "launch claim: %v", err)
		_ = emitControl(run.Stdout, controlMessage{OperationID: req.OperationID, Status: "terminal", Error: terr})
		return terr
	}
	done, launchErr := run.Launcher.Launch(ctx, sessionLaunch{
		Log: log, TraceID: traceID, RootSpan: rootSpan, Sandbox: sandbox, Request: req,
	})
	// launch 성패와 무관하게 세션 종료를 durable하게 남긴다.
	_, endErr := log.Writer.Submit(ctx, gen.EventRecord{
		Ts: run.Now(), TraceID: traceID, SpanID: rootSpan,
		Kind: gen.KindSessionEnd, Actor: "parent", Payload: json.RawMessage(`{}`),
	})
	lastSeq, _ := log.Reader.LastSeq(ctx)
	terminal := controlMessage{OperationID: req.OperationID, Status: "terminal", LastSeq: lastSeq}
	if launchErr != nil {
		terminal.Error = newRunError(codeLaunchFailed, "%v", launchErr)
	} else {
		terminal.Done = &doneRef{Status: string(done.Status), Result: done.Result}
	}
	if err := emitControl(run.Stdout, terminal); err != nil {
		return errors.Join(launchErr, endErr, err)
	}
	if launchErr != nil || endErr != nil {
		return errors.Join(launchErr, endErr)
	}
	if done.Status != gen.DonePayloadStatusOk {
		return fmt.Errorf("서브에이전트 status=%s", done.Status)
	}
	return nil
}

// replayAcceptance는 동일 key 재요청의 무spawn 경로다: 응답 유실 재조회는
// 허용하되 두 번째 spawn은 금지한다(계약 §3.3).
func replayAcceptance(out io.Writer, req runRequest, registry *accept.Registry, acc accept.Acceptance, policyHash, sessionPath string) error {
	if acc.PolicyHash != policyHash {
		return rejectControl(out, req.OperationID, newRunError(codePolicyChanged,
			"기존 접수의 policy hash %s와 현재 병합 결과 %s가 다름", acc.PolicyHash, policyHash))
	}
	status, err := registry.Lookup(req.Scope, req.IdempotencyKey)
	if err != nil {
		return rejectControl(out, req.OperationID, newRunError(codeSessionCorrupt, "%v", err))
	}
	switch {
	case !status.DBInitialized:
		// 접수 binding은 durable하나 세션 초기화 완료 기록이 없다 — 읽기
		// 전용 재조회 대상이며 실행 재요청은 금지(계약 §4 표).
		return emitControl(out, controlMessage{
			OperationID: req.OperationID, Status: "initializing",
			IdempotencyKey: req.IdempotencyKey, RequestFingerprint: acc.Fingerprint,
			PolicyHash: acc.PolicyHash, Duplicate: true,
		})
	case !status.SessionExists:
		// tombstone: DB가 삭제됐다. 같은 key로 재초기화·재spawn하지 않는다.
		rerr := newRunError(codeAcceptanceUnknown,
			"접수된 세션 파일 %s가 존재하지 않음 — 같은 key 재사용 불가", sessionPath)
		_ = emitControl(out, controlMessage{
			OperationID: req.OperationID, Status: "unknown",
			IdempotencyKey: req.IdempotencyKey, RequestFingerprint: acc.Fingerprint,
			PolicyHash: acc.PolicyHash, Duplicate: true, Error: rerr,
		})
		return rerr
	default:
		launched := status.Launched
		return emitControl(out, controlMessage{
			OperationID: req.OperationID, Status: "accepted",
			SessionRef:     &sessionRef{SessionDB: sessionPath, TraceID: acc.TraceID},
			IdempotencyKey: req.IdempotencyKey, RequestFingerprint: acc.Fingerprint,
			PolicyHash: acc.PolicyHash, AcceptanceSeq: acceptanceSeq,
			LaunchClaimed: &launched, Duplicate: true,
		})
	}
}

func rejectControl(out io.Writer, operationID string, rerr *runError) error {
	if err := emitControl(out, controlMessage{OperationID: operationID, Status: "rejected", Error: rerr}); err != nil {
		return errors.Join(rerr, err)
	}
	return rerr
}

// --- 생산 world launcher ---

// worldConfig는 호스트 운영자 소유 설정이다(Rhizome 계약 밖): podman state
// root, 신뢰 프록시 이미지, 어댑터별 실행 파일·에이전트 이미지. 실행
// 이미지는 불변 digest만 허용한다.
type worldConfig struct {
	StateRoot  string                        `json:"state_root"`
	ProxyImage worldImageConfig              `json:"proxy_image"`
	Adapters   map[string]worldAdapterConfig `json:"adapters"`
}

type worldImageConfig struct {
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
	UID        uint32 `json:"uid"`
	GID        uint32 `json:"gid"`
}

type worldAdapterConfig struct {
	Bin         string           `json:"bin"`
	Image       worldImageConfig `json:"image"`
	AgentArgv   []string         `json:"agent_argv"`
	ControlMode string           `json:"control_mode"` // tool_approval | container_only
}

func parseWorldConfig(data []byte) (worldConfig, error) {
	var cfg worldConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return worldConfig{}, fmt.Errorf("world config 해석: %w", err)
	}
	if cfg.StateRoot == "" {
		return worldConfig{}, fmt.Errorf("world config: state_root 필수")
	}
	for name, adapter := range cfg.Adapters {
		if !approvedAdapters[name] {
			return worldConfig{}, fmt.Errorf("world config: 승인 목록 밖 어댑터 %q", name)
		}
		if adapter.Bin == "" || len(adapter.AgentArgv) == 0 {
			return worldConfig{}, fmt.Errorf("world config: 어댑터 %q에 bin/agent_argv 필수", name)
		}
		if adapter.ControlMode != "tool_approval" && adapter.ControlMode != "container_only" {
			return worldConfig{}, fmt.Errorf("world config: 어댑터 %q control_mode %q — tool_approval|container_only만 허용", name, adapter.ControlMode)
		}
	}
	return cfg, nil
}

// worldLauncher는 유일한 생산 sessionLauncher다. backend 구성이 CLI 단계
// (claim 이전)에서 끝나므로, world를 조립할 수 없는 호스트(macOS 등)는
// key를 소모하기 전에 실패한다.
type worldLauncher struct {
	backend world.Backend
	config  worldConfig
}

func newWorldLauncher(cfg worldConfig) (*worldLauncher, error) {
	backend, err := local.NewBackend(local.Config{
		StateRoot:            cfg.StateRoot,
		ProxyImageRepository: cfg.ProxyImage.Repository,
		ProxyImageDigest:     cfg.ProxyImage.Digest,
		ProxyIdentity:        world.AgentIdentity{UID: cfg.ProxyImage.UID, GID: cfg.ProxyImage.GID},
	})
	if err != nil {
		return nil, err
	}
	return &worldLauncher{backend: backend, config: cfg}, nil
}

func (l *worldLauncher) Launch(ctx context.Context, in sessionLaunch) (gen.DonePayload, error) {
	adapter, ok := l.config.Adapters[in.Request.AdapterID]
	if !ok {
		return gen.DonePayload{}, fmt.Errorf("world config에 어댑터 %q 정의가 없음", in.Request.AdapterID)
	}
	controlMode := gen.SubagentSpawnPayloadControlModeToolApproval
	if adapter.ControlMode == "container_only" {
		controlMode = gen.SubagentSpawnPayloadControlModeContainerOnly
	}
	childSpan := logd.NewSpanID()
	spawnSpec := world.NewSpawnSpec(
		world.NewEffectivePolicy(in.Sandbox),
		world.NewImageReference(adapter.Image.Repository, adapter.Image.Digest),
		adapter.AgentArgv, 0, in.TraceID, childSpan,
		world.AgentIdentity{UID: adapter.Image.UID, GID: adapter.Image.GID}, nil,
	)
	active, err := startProductionWorld(ctx, worldLaunch{
		Backend: l.backend, SpawnSpec: spawnSpec, Writer: in.Log.Writer,
		TraceID: in.TraceID, ParentSpan: in.RootSpan,
		AdapterCommand: []string{adapter.Bin}, AdapterName: in.Request.AdapterID,
		ControlMode: controlMode, AdapterStderr: os.Stderr,
		Instruction: in.Request.TaskRef.Instruction, Workspace: "/workspace",
		Budget: in.Sandbox.Budget, Depth: 0, ProfileID: in.Sandbox.ProfileID,
		// T18 전까지 승인 decider는 DenyAll 고정 — 자동 allow 경로 없음.
		Approval: subagent.Spec{Approval: in.Sandbox.Approval, Decider: policy.DenyAll{}},
		// 호스트 어댑터 환경은 최소로 유지 — 러너 자격증명이 컨테이너
		// 자격증명이 되는 경로를 차단한다(T15와 동일).
		AdapterBaseEnv: []string{"PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		return gen.DonePayload{}, err
	}
	finalized := false
	defer func() {
		if !finalized {
			_ = active.Lease.Close(context.Background())
		}
	}()
	done, waitErr := active.Subagent.Wait(ctx)
	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	closeErr := active.FinalizeCollection(closeCtx)
	cancel()
	finalized = true
	if waitErr != nil {
		return gen.DonePayload{}, errors.Join(waitErr, closeErr)
	}
	if closeErr != nil {
		return gen.DonePayload{}, closeErr
	}
	return done, nil
}
