package main

// T17 실행 요청(request.json) 파싱·검증과 접수 NDJSON 제어 메시지.
//
// 계약 근거: ../Rhizome/docs/janus-execution-contract.md §2·§3. 공개 프로토콜
// 스키마는 Rhizome이 임시 소유한다(결정 시트 1번) — 여기서는 계약 초안
// 필드를 구현하고, 어긋남·추가 필요 사항은 docs/rhizome-contract-reply-t17.md
// 로 회신한다. contracts/ 밑에 새 스키마 파일을 만들지 않는다.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/Eastsidegunn/JANUS/core/policy"
)

// 계약 §3.1 오류 코드. POLICY_DENIED와 LAUNCH_FAILED는 초안에 없는 추가
// 제안이다(회신 문서 참조).
const (
	codeUnsupportedContract = "UNSUPPORTED_CONTRACT"
	codeUnsupportedAdapter  = "UNSUPPORTED_ADAPTER"
	codePolicyChanged       = "POLICY_CHANGED"
	codePolicyDenied        = "POLICY_DENIED"
	codeBudgetInvalid       = "BUDGET_INVALID"
	codeKeyConflict         = "KEY_CONFLICT"
	codeSessionCorrupt      = "SESSION_CORRUPT"
	codeIOError             = "IO_ERROR"
	codeAcceptanceUnknown   = "ACCEPTANCE_UNKNOWN"
	codeLaunchFailed        = "LAUNCH_FAILED"
)

// runRequestVersion은 지원하는 요청 계약 버전이다.
const runRequestVersion = 1

// approvedAdapters는 코드에 고정된 어댑터 허용 목록이다(결정 시트 2번
// "adapter 허용 목록"). world config에 정의가 있어도 이 목록 밖이면 거부.
var approvedAdapters = map[string]bool{"claudecode": true, "codex": true}

var hex64Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type runRequest struct {
	Version              int64         `json:"version"`
	OperationID          string        `json:"operation_id"`
	Scope                string        `json:"scope"`
	IdempotencyKey       string        `json:"idempotency_key"`
	RequestFingerprint   string        `json:"request_fingerprint"`
	RhizomeExecutionID   string        `json:"rhizome_execution_id"`
	MissionID            string        `json:"mission_id"`
	CorrelationID        string        `json:"correlation_id"`
	AdapterID            string        `json:"adapter_id"`
	AdapterVersion       string        `json:"adapter_version"`
	WorkspaceRef         string        `json:"workspace_ref"`
	TaskRef              runTaskRef    `json:"task_ref"`
	ProfileID            string        `json:"profile_id"`
	ProfileHash          string        `json:"profile_hash"`
	PolicyMappingVersion string        `json:"policy_mapping_version"`
	Budget               requestBudget `json:"budget"`
}

// runTaskRef는 실행 의도와 승인된 입력 참조다. v1에서는 어댑터 instruction
// 하나로 한정한다 — Hi-Fi Pi 명령 스키마를 새로 정의하지 않는다(계약 §3.1).
type runTaskRef struct {
	Instruction string `json:"instruction"`
}

type requestBudget struct {
	Tokens   int64 `json:"tokens"`
	TimeMs   int64 `json:"time_ms"`
	MaxDepth int64 `json:"max_depth"`
}

// runError는 접수 오류다. Retryable은 자동 재실행 허가가 아니다(계약 §2).
type runError struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

func (e *runError) Error() string { return e.Code + ": " + e.Message }

func newRunError(code, format string, args ...any) *runError {
	return &runError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// controlMessage는 stdout NDJSON 제어 메시지다(계약 §2 공통 응답 형태).
type controlMessage struct {
	Version            int64       `json:"version"`
	OperationID        string      `json:"operation_id,omitempty"`
	Status             string      `json:"status"` // accepted|initializing|unknown|rejected|terminal
	SessionRef         *sessionRef `json:"session_ref,omitempty"`
	IdempotencyKey     string      `json:"idempotency_key,omitempty"`
	RequestFingerprint string      `json:"request_fingerprint,omitempty"`
	PolicyHash         string      `json:"policy_hash,omitempty"`
	AcceptanceSeq      int64       `json:"acceptance_seq,omitempty"`
	LaunchClaimed      *bool       `json:"launch_claimed,omitempty"`
	Duplicate          bool        `json:"duplicate,omitempty"`
	Done               *doneRef    `json:"done,omitempty"`
	LastSeq            int64       `json:"last_seq,omitempty"`
	Error              *runError   `json:"error,omitempty"`
}

type sessionRef struct {
	SessionDB string `json:"session_db"`
	TraceID   string `json:"trace_id"`
}

type doneRef struct {
	Status string `json:"status"`
	Result string `json:"result"`
}

func emitControl(w io.Writer, msg controlMessage) error {
	msg.Version = runRequestVersion
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(msg)
}

// parseRunRequest는 request.json을 엄격 모드로 해석한다. v1 계약 밖의
// 필드는 추정 실행하지 않고 UNSUPPORTED_CONTRACT로 거부한다.
func parseRunRequest(data []byte) (runRequest, *runError) {
	var req runRequest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return runRequest{}, newRunError(codeUnsupportedContract, "request 해석 불가: %v", err)
	}
	if req.Version != runRequestVersion {
		return runRequest{}, newRunError(codeUnsupportedContract, "지원하지 않는 요청 버전 %d (지원: %d)", req.Version, runRequestVersion)
	}
	required := map[string]string{
		"operation_id":         req.OperationID,
		"scope":                req.Scope,
		"idempotency_key":      req.IdempotencyKey,
		"rhizome_execution_id": req.RhizomeExecutionID,
		"adapter_id":           req.AdapterID,
		"workspace_ref":        req.WorkspaceRef,
		"profile_id":           req.ProfileID,
		"task_ref.instruction": req.TaskRef.Instruction,
	}
	for field, value := range required {
		if value == "" {
			return runRequest{}, newRunError(codeUnsupportedContract, "필수 필드 %s 부재", field)
		}
	}
	if !hex64Pattern.MatchString(req.RequestFingerprint) {
		return runRequest{}, newRunError(codeUnsupportedContract, "request_fingerprint는 64자리 소문자 hex여야 함")
	}
	if !hex64Pattern.MatchString(req.ProfileHash) {
		return runRequest{}, newRunError(codeUnsupportedContract, "profile_hash는 64자리 소문자 hex여야 함")
	}
	if !approvedAdapters[req.AdapterID] {
		return runRequest{}, newRunError(codeUnsupportedAdapter, "승인되지 않은 어댑터 %q", req.AdapterID)
	}
	if req.Budget.Tokens <= 0 || req.Budget.TimeMs <= 0 || req.Budget.MaxDepth <= 0 {
		// 무제한·미지정은 Rhizome 쪽에서 이미 거부됨 — 도달 정책은 항상
		// 유한·완전해야 한다(브리핑 §4). 0/음수는 추정하지 않고 거부.
		return runRequest{}, newRunError(codeBudgetInvalid, "budget 세 축은 모두 양수여야 함: %+v", req.Budget)
	}
	return req, nil
}

// pinnedProfileHash는 요청 전에 고정된 정책 파일 내용의 hash다:
// sha256(uvarint(len(b0))‖b0‖uvarint(len(b1))‖b1‖…), b_i는 profile,
// overlay(순서대로) 파일의 원문 바이트. 실행 시 재검증해 사전 검사 이후
// 파일이 바뀌었으면 POLICY_CHANGED로 거부한다(계약 §3.1).
func pinnedProfileHash(profilePath string, overlayPaths []string) (string, error) {
	h := sha256.New()
	var buf [binary.MaxVarintLen64]byte
	for _, path := range append([]string{profilePath}, overlayPaths...) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("정책 파일 %s 읽기: %w", path, err)
		}
		n := binary.PutUvarint(buf[:], uint64(len(data)))
		h.Write(buf[:n])
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// effectivePolicyHash는 실행 시 재병합 결과의 정책 fingerprint다:
// `hx dump-config --profile … --overlay … --workspace <workspace_ref>`
// stdout의 sha256과 정의상 동일하다 — Rhizome은 사전 dump-config 출력을
// hash해 같은 값을 예측·대조할 수 있다.
func effectivePolicyHash(base policy.Profile, overlays []policy.Profile, workspace string) (string, error) {
	rendered, err := renderDumpConfig(base, overlays, workspace)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(rendered)
	return hex.EncodeToString(sum[:]), nil
}
