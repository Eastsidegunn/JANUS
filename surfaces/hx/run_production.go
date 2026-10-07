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
	"net/netip"
	"os"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/runtimedir"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/approvaltiming"
	"github.com/Eastsidegunn/JANUS/seams/accept"
	"github.com/Eastsidegunn/JANUS/seams/approvalrelay"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
	"github.com/Eastsidegunn/JANUS/seams/subagent/claudecode"
	local "github.com/Eastsidegunn/JANUS/seams/world/local"
)

// runProductionCmd는 생산 경로의 CLI 진입점이다. world backend 구성은
// claim 이전에 끝난다 — world를 조립할 수 없는 호스트는 key를 소모하지
// 않고 실패한다.
func runProductionCmd(requestPath, profilePath string, overlayPaths []string, acceptRoot, worldConfigPath, sessionArg, approvalEndpoint string) error {
	if _, err := runtimedir.Dir(); err != nil {
		return fmt.Errorf("hx: runtime directory: %w", err)
	}
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
	launcher.approvalEndpoint = approvalEndpoint
	return runProduction(context.Background(), productionRun{
		RequestBytes: requestBytes, ProfilePath: profilePath, OverlayPaths: overlayPaths,
		AcceptRoot: acceptRoot, SessionArg: sessionArg, Launcher: launcher, Stdout: os.Stdout, Stderr: os.Stderr,
		ApprovalEndpoint: approvalEndpoint, RedactionValues: cfg.redactionValues(),
		RedactionPatterns: append([]string(nil), cfg.RedactionPatterns...),
	})
}

// sessionLauncher는 접수가 durable해진 뒤의 실행 단계다. 인터페이스로
// 분리한 이유는 검증 현실이다: 접수·claim·정책 파이프라인은 macOS
// `make ci`에서 단위 검증하고, 실제 world 조립은 Linux 게이트가 검증한다.
// 생산 구현은 worldLauncher 하나뿐이며 startProductionWorld만 경유한다.
type sessionLauncher interface {
	Launch(ctx context.Context, in sessionLaunch) (gen.DonePayload, error)
}

// preClaimChecker는 launcher가 실효 sandbox와 자기 운영자 설정의 정합을
// key claim 이전에 검사하는 선택적 단계다(SCP-T26-001 §3.3).
type preClaimChecker interface {
	CheckBeforeClaim(sandbox policy.SandboxConfig) error
}

type sessionLaunch struct {
	Log        *sqlite.Log
	TraceID    string
	RootSpan   string
	Sandbox    policy.SandboxConfig
	Request    runRequest
	PolicyHash string
	Redactor   *logd.Redactor
	Stderr     io.Writer
}

type productionRun struct {
	RequestBytes      []byte
	ProfilePath       string
	OverlayPaths      []string
	AcceptRoot        string
	SessionArg        string // --session 제공 시 정본 경로와 일치 검증
	Launcher          sessionLauncher
	Stdout            io.Writer
	Stderr            io.Writer
	Now               func() int64
	ApprovalEndpoint  string
	RedactionValues   map[string][]string
	RedactionPatterns []string
}

type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

// redactErr keeps sentinel identity available to errors.Is/errors.As while
// ensuring the string rendered by main and other control-plane callers has
// passed through the same redactor as stdout, stderr, and the event writer.
// Callers must not print the Unwrap() cause, which is the unredacted original.
func redactErr(redactor *logd.Redactor, err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{message: redactor.RedactString(err.Error()), cause: err}
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
	values := append([]string(nil), run.RedactionValues[req.AdapterID]...)
	redactor, err := logd.NewRedactorWithLiterals(run.RedactionPatterns, values)
	if err != nil {
		return rejectControl(run.Stdout, req.OperationID, newRunError(codePolicyDenied, "redaction_patterns 검증 실패: %v", err))
	}
	if run.Stdout == nil {
		run.Stdout = io.Discard
	}
	stdout := logd.NewRedactingWriter(run.Stdout, redactor)
	defer stdout.Flush()
	run.Stdout = stdout
	stderrTarget := run.Stderr
	if stderrTarget == nil {
		stderrTarget = io.Discard
	}
	stderr := logd.NewRedactingWriter(stderrTarget, redactor)
	defer stderr.Flush()
	reject := func(rerr *runError) error {
		return redactErr(redactor, rejectControl(run.Stdout, req.OperationID, rerr))
	}
	if gen.SubagentSpawnPayloadSessionMode(req.SessionMode) == gen.SubagentSpawnPayloadSessionModeMultiturn && run.ApprovalEndpoint == "" {
		// send_message·stop은 제어 socket으로만 도달한다. socket 없는
		// multiturn은 후속 턴도 종료 명령도 받을 수 없으므로 claim 전에 거부.
		return reject(newRunError(codeUnsupportedContract, "session_mode multiturn에는 --approval-endpoint 제어 socket이 필요"))
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
	// SCP-T26-001 §3.3: 운영자 설정(egress_pins)과 병합 정책의 정합은 key
	// claim 이전에 검사한다 — 어긋난 설정이 key를 소모하지 않는다(T17 P1).
	// Backend.Prepare가 같은 검사를 다시 수행한다(fail-closed 이중화).
	if checker, ok := run.Launcher.(preClaimChecker); ok {
		if err := checker.CheckBeforeClaim(sandbox); err != nil {
			return reject(newRunError(codePolicyDenied, "%v", err))
		}
	}

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
		return redactErr(redactor, replayAcceptance(run.Stdout, req, registry, acc, policyHash, sessionPath))
	}

	// 접수 binding durable → session DB 초기화. 조회가 아니라 최초 접수
	// 경로에서만 DB 파일이 생긴다.
	var logOpts []logd.Option
	logOpts = append(logOpts, logd.WithRedactor(redactor))
	log, err := sqlite.Open(ctx, sessionPath, logOpts...)
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
		return redactErr(redactor, err)
	}

	// 외부 효과 직전의 durable launch claim — 이 지점 이후 어떤 재시도도
	// 같은 key로 두 번째 spawn을 만들 수 없다.
	if err := registry.MarkLaunched(req.Scope, req.IdempotencyKey); err != nil {
		terr := newRunError(codeIOError, "launch claim: %v", err)
		_ = emitControl(run.Stdout, controlMessage{OperationID: req.OperationID, Status: "terminal", Error: terr})
		return redactErr(redactor, terr)
	}
	done, launchErr := run.Launcher.Launch(ctx, sessionLaunch{
		Log: log, TraceID: traceID, RootSpan: rootSpan, Sandbox: sandbox, Request: req, PolicyHash: policyHash,
		Redactor: redactor, Stderr: stderr,
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
		return redactErr(redactor, errors.Join(launchErr, endErr, err))
	}
	if launchErr != nil || endErr != nil {
		return redactErr(redactor, errors.Join(launchErr, endErr))
	}
	if done.Status != gen.DonePayloadStatusOk {
		return redactErr(redactor, fmt.Errorf("서브에이전트 status=%s", done.Status))
	}
	return nil
}

func selectApprovalDecider(endpoint, traceID, policyHash string, timeoutMs int64, session approvalrelay.SessionControl) (policy.ApprovalDecider, io.Closer, *approvalrelay.Server, error) {
	if endpoint == "" {
		return policy.DenyAll{}, nil, nil, nil
	}
	t := time.Duration(timeoutMs) * time.Millisecond
	srv, err := approvalrelay.NewServer(endpoint, t)
	if err != nil {
		return nil, nil, nil, err
	}
	// 세션 바인딩은 listen 전에 둔다 — 첫 연결부터 send_message·events_tail이
	// 결정적으로 판정된다(T25).
	srv.SetSession(session)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Listen() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(endpoint); err == nil {
			break
		}
		select {
		case err := <-errCh:
			_ = srv.Close()
			return nil, nil, nil, err
		default:
		}
		if time.Now().After(deadline) {
			_ = srv.Close()
			return nil, nil, nil, fmt.Errorf("approval endpoint did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
	relay, err := approvalrelay.NewServerApprovalRelay(srv, approvalrelay.RelayConfig{Endpoint: endpoint, TraceID: traceID, PolicyHash: policyHash, Timeout: t})
	if err != nil {
		_ = srv.Close()
		return nil, nil, nil, err
	}
	return relay, srv, srv, nil
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
	// EgressPins는 선언 게이트웨이 핀이다(SCP-T26-001, FR-SBX-03). 부재 =
	// 빈 목록. 핀은 정책 allowlist를 넓히지 못하며 주소 해석 방식만 바꾼다.
	EgressPins        []worldEgressPin `json:"egress_pins,omitempty"`
	RedactionPatterns []string         `json:"redaction_patterns,omitempty"`
}

type worldEgressPin struct {
	Domain  string `json:"domain"`
	Address string `json:"address"`
}

func (cfg worldConfig) egressPins() (map[string]netip.AddrPort, error) {
	entries := make([]local.EgressPinConfig, 0, len(cfg.EgressPins))
	for _, pin := range cfg.EgressPins {
		entries = append(entries, local.EgressPinConfig{Domain: pin.Domain, Address: pin.Address})
	}
	return local.NormalizeEgressPins(entries)
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
	// Env is plaintext NAME=VALUE, including the CLIProxyAPI access key (T23).
	// ANTHROPIC_AUTH_TOKEN supplies Bearer auth; ANTHROPIC_BASE_URL selects
	// the gateway: https://code.claude.com/docs/en/llm-gateway-connect
	// Subscription OAuth tokens belong exclusively to the external gateway.
	Env       []string `json:"env,omitempty"`
	SecretEnv []string `json:"secret_env,omitempty"`
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
	if _, err := logd.NewRedactor(cfg.RedactionPatterns...); err != nil {
		return worldConfig{}, fmt.Errorf("world config: redaction_patterns: %w", err)
	}
	if err := local.ValidateImageReferenceConfig(cfg.ProxyImage.Repository, cfg.ProxyImage.Digest, cfg.ProxyImage.UID, cfg.ProxyImage.GID); err != nil {
		return worldConfig{}, fmt.Errorf("world config: proxy image: %w", err)
	}
	for name, adapter := range cfg.Adapters {
		if !approvedAdapters[name] {
			return worldConfig{}, fmt.Errorf("world config: 승인 목록 밖 어댑터 %q", name)
		}
		if adapter.Bin == "" || len(adapter.AgentArgv) == 0 {
			return worldConfig{}, fmt.Errorf("world config: 어댑터 %q에 bin/agent_argv 필수", name)
		}
		if err := local.ValidateImageReferenceConfig(adapter.Image.Repository, adapter.Image.Digest, adapter.Image.UID, adapter.Image.GID); err != nil {
			return worldConfig{}, fmt.Errorf("world config: 어댑터 %q image: %w", name, err)
		}
		if adapter.ControlMode != "tool_approval" && adapter.ControlMode != "container_only" {
			return worldConfig{}, fmt.Errorf("world config: 어댑터 %q control_mode %q — tool_approval|container_only만 허용", name, adapter.ControlMode)
		}
		if err := local.ValidateAgentEnvironment(adapter.Env); err != nil {
			return worldConfig{}, fmt.Errorf("world config: 어댑터 %q: %w", name, err)
		}
		if err := local.ValidateSecretEnvNames(adapter.SecretEnv); err != nil {
			return worldConfig{}, fmt.Errorf("world config: 어댑터 %q: %w", name, err)
		}
		if err := local.ValidateSecretEnvironmentValues(adapter.Env, adapter.SecretEnv); err != nil {
			return worldConfig{}, fmt.Errorf("world config: 어댑터 %q: %w", name, err)
		}
	}
	if _, err := cfg.egressPins(); err != nil {
		return worldConfig{}, fmt.Errorf("world config: egress_pins: %w", err)
	}
	return cfg, nil
}

func (cfg worldConfig) redactionValues() map[string][]string {
	values := make(map[string][]string, len(cfg.Adapters))
	for name, adapter := range cfg.Adapters {
		values[name] = local.SecretEnvironmentValues(adapter.Env, adapter.SecretEnv)
	}
	return values
}

// worldLauncher는 유일한 생산 sessionLauncher다. backend 구성이 CLI 단계
// (claim 이전)에서 끝나므로, world를 조립할 수 없는 호스트(macOS 등)는
// key를 소모하기 전에 실패한다.
type worldLauncher struct {
	backend          world.Backend
	config           worldConfig
	approvalEndpoint string
}

func newWorldLauncher(cfg worldConfig) (*worldLauncher, error) {
	pins, err := cfg.egressPins()
	if err != nil {
		return nil, fmt.Errorf("world config: egress_pins: %w", err)
	}
	backend, err := local.NewBackend(local.Config{
		StateRoot:            cfg.StateRoot,
		ProxyImageRepository: cfg.ProxyImage.Repository,
		ProxyImageDigest:     cfg.ProxyImage.Digest,
		ProxyIdentity:        world.AgentIdentity{UID: cfg.ProxyImage.UID, GID: cfg.ProxyImage.GID},
		EgressPins:           pins,
	})
	if err != nil {
		return nil, err
	}
	return &worldLauncher{backend: backend, config: cfg}, nil
}

// CheckBeforeClaim는 핀 domain이 병합 정책 egress allowlist 안에 있는지
// key claim 이전에 확인한다(SCP-T26-001 §3.3). Prepare가 같은 규칙을 재검사한다.
func (l *worldLauncher) CheckBeforeClaim(sandbox policy.SandboxConfig) error {
	pins, err := l.config.egressPins()
	if err != nil {
		return fmt.Errorf("world config: egress_pins: %w", err)
	}
	return local.ValidateEgressPinsWithinPolicy(pins, sandbox.Egress)
}

func (l *worldLauncher) Launch(ctx context.Context, in sessionLaunch) (gen.DonePayload, error) {
	timeoutMs := in.Sandbox.Budget.TimeMs
	timeoutMs = boundedApprovalWaitMs(timeoutMs)
	sessionMode := gen.SubagentSpawnPayloadSessionMode(in.Request.SessionMode)
	multiturn := sessionMode == gen.SubagentSpawnPayloadSessionModeMultiturn
	var events approvalrelay.EventSource
	if in.Log != nil {
		// events_tail은 세션 로그 Reader의 읽기 전용 사영이다 — writer 없음.
		events = in.Log.Reader.ReadFrom
	}
	decider, closer, srv, err := selectApprovalDecider(l.approvalEndpoint, in.TraceID, in.PolicyHash, timeoutMs,
		approvalrelay.SessionControl{SessionID: in.TraceID, Multiturn: multiturn, Events: events})
	if err != nil {
		return gen.DonePayload{}, err
	}
	if closer != nil {
		defer closer.Close()
	}
	adapter, ok := l.config.Adapters[in.Request.AdapterID]
	if !ok {
		return gen.DonePayload{}, fmt.Errorf("world config에 어댑터 %q 정의가 없음", in.Request.AdapterID)
	}
	controlMode := gen.SubagentSpawnPayloadControlModeToolApproval
	if adapter.ControlMode == "container_only" {
		controlMode = gen.SubagentSpawnPayloadControlModeContainerOnly
	}
	agentArgv := adapter.AgentArgv
	if in.Request.AdapterID == "claudecode" {
		agentArgv = claudecode.ContainerArgvFor(adapter.AgentArgv[0], in.Request.TaskRef.Instruction, sessionMode)
	}
	childSpan := logd.NewSpanID()
	spawnSpec := world.NewSpawnSpec(
		world.NewEffectivePolicy(in.Sandbox),
		world.NewImageReference(adapter.Image.Repository, adapter.Image.Digest),
		agentArgv, 0, in.TraceID, childSpan,
		world.AgentIdentity{UID: adapter.Image.UID, GID: adapter.Image.GID}, nil,
	)
	// Plaintext gateway URL and access key from operator-owned config (T23).
	if len(adapter.Env) > 0 {
		spawnSpec = spawnSpec.WithAgentEnv(adapter.Env)
	}
	if len(adapter.SecretEnv) > 0 {
		spawnSpec = spawnSpec.WithSecretEnvNames(adapter.SecretEnv)
	}
	active, err := startProductionWorld(ctx, worldLaunch{
		Backend: l.backend, SpawnSpec: spawnSpec, Writer: in.Log.Writer,
		TraceID: in.TraceID, ParentSpan: in.RootSpan,
		AdapterCommand: []string{adapter.Bin}, AdapterName: in.Request.AdapterID,
		ControlMode: controlMode, AdapterStderr: in.Stderr, SessionMode: sessionMode,
		// FR-SBX-02: in.Sandbox.Workspace는 host mount 원본이며, 어댑터는
		// local backend가 그 overlay를 노출하는 container 내부 경로를 받는다.
		Instruction: in.Request.TaskRef.Instruction, Workspace: local.ContainerWorkspacePath,
		Budget: in.Sandbox.Budget, Depth: 0, ProfileID: in.Sandbox.ProfileID,
		// T18 전까지 승인 decider는 DenyAll 고정 — 자동 allow 경로 없음.
		Approval: subagent.Spec{Approval: in.Sandbox.Approval, Decider: decider},
		// 호스트 어댑터 환경은 최소로 유지 — 러너 자격증명이 컨테이너
		// 자격증명이 되는 경로를 차단한다(T15와 동일).
		AdapterBaseEnv: adapterBaseEnv(),
	})
	if err != nil {
		return gen.DonePayload{}, err
	}
	if relay, ok := decider.(*approvalrelay.UnixApprovalRelay); ok {
		relay.SetStopHandler(func(m approvalrelay.Message) {
			var reason gen.StopPayloadReason
			switch m.Reason {
			case "user":
				reason = gen.StopPayloadReasonUser
			case "budget_exceeded":
				reason = gen.StopPayloadReasonBudgetExceeded
			case "policy":
				reason = gen.StopPayloadReasonPolicy
			}
			_ = active.Subagent.Stop(reason)
		}, nil, nil)
		// T25 send_message: 세션 소유 subagent seam만 경유한다 — user/message
		// durable 기록(단일 writer) → 어댑터 message 명령. 컨테이너 직접 접근 없음.
		relay.SetMessageHandler(sessionMessageHandler(active.Subagent))
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
	if srv != nil {
		if events, e := in.Log.Reader.ReadFrom(ctx, 1); e == nil {
			for _, ev := range events {
				if ev.Kind == gen.KindSubagentDone {
					srv.MarkTerminal(ev.Seq)
				}
			}
		}
	}
	return done, nil
}

// boundedApprovalWaitMs fixes the approval window once per session. A larger
// session budget must not extend the host relay past ApprovalWaitMax.
func boundedApprovalWaitMs(requested int64) int64 {
	max := approvaltiming.ApprovalWaitMax.Milliseconds()
	if requested <= 0 || requested > max {
		return max
	}
	return requested
}

// adapterBaseEnv keeps the host adapter environment minimal while carrying the
// runtime root needed by adapters that create their own Unix sockets.
func adapterBaseEnv() []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if runtimeDir := os.Getenv("HX_RUNTIME_DIR"); runtimeDir != "" {
		env = append(env, "HX_RUNTIME_DIR="+runtimeDir)
	}
	return env
}

// sessionMessageHandler routes a relay send_message to the session-owned
// subagent seam (T25): Send records user/message through the single writer and
// only then delivers the adapter message command. The seam's sentinels map to
// the relay's structured rejections here, keeping the seams import-free of
// each other.
func sessionMessageHandler(sub *subagent.Subagent) approvalrelay.MessageHandler {
	return func(ctx context.Context, text string) (int64, error) {
		seq, err := sub.Send(ctx, text)
		switch {
		case errors.Is(err, subagent.ErrNotMultiturn):
			return seq, approvalrelay.ErrNotMultiturn
		case errors.Is(err, subagent.ErrSessionTerminal):
			return seq, approvalrelay.ErrSessionTerminal
		}
		return seq, err
	}
}
