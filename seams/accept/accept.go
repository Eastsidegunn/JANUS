// Package accept는 scoped idempotency key의 durable 접수 레지스트리다 (T17).
//
// 계약 근거: ../Rhizome/docs/janus-execution-contract.md §3.3 — 시작 전 key의
// 배타 점유, 외부 효과 전 durable 접수/launch claim, 삭제된 세션 key의
// 재사용 차단(tombstone). 물리 형식은 JANUS 결정 사항이며 여기 구현이 그
// 결정이다:
//
//	<root>/<keyhash>/claim.json      접수 binding (배타 공개, 불변)
//	<root>/<keyhash>/db-initialized  session DB 초기화 완료 마커
//	<root>/<keyhash>/launch          launch claim 마커 (외부 효과 직전)
//	<root>/<keyhash>/session.sqlite  세션 이벤트 로그 (writer만 접근)
//
// keyhash = hex(sha256(uvarint(len(scope)) ‖ scope ‖ uvarint(len(key)) ‖ key)).
// scope와 key 양쪽을 길이 접두사로 구분해 (scope="ab",key="c")와
// (scope="a",key="bc")의 충돌을 차단한다. key 원문은 경로에 넣지 않는다.
//
// 배타 점유는 os.Link의 원자성으로 얻는다: claim은 임시 파일에 완전히
// 쓰고 fsync한 뒤 link(2)로 최종 이름에 공개한다 — 부분 내용이 최종
// 이름으로 관측되는 경로가 없고, 동시 시도 중 정확히 하나만 성공한다.
//
// claim.json은 삭제되지 않는다. session.sqlite가 사라져도 claim이 남아
// 같은 key의 재사용(새 spawn)이 구조적으로 불가능하다 — 이것이 tombstone
// 규칙의 구현이다. 레지스트리는 세션 이벤트 로그 파일을 열지 않는다
// (단일 writer 불변식): 존재 여부만 stat으로 관측한다.
package accept

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SessionFileName은 keyhash 디렉터리 안의 세션 로그 파일 이름이다.
const SessionFileName = "session.sqlite"

const (
	claimFileName  = "claim.json"
	dbMarkerName   = "db-initialized"
	launchName     = "launch"
	acceptanceVers = 1
)

// ErrKeyConflict: 같은 key에 다른 fingerprint(또는 keyhash 충돌로 다른
// scope/key 원문)의 claim이 이미 있다 — 계약 §3.3 KEY_CONFLICT.
var ErrKeyConflict = errors.New("accept: 같은 key에 다른 요청의 claim이 존재")

// ErrClaimCorrupt: claim.json이 있으나 해석 불가 — 자동 초기화·덮어쓰기
// 없이 사람 조사 대상으로 남긴다.
var ErrClaimCorrupt = errors.New("accept: claim 기록이 손상됨")

// ErrAlreadyLaunched: launch claim이 이미 존재 — scoped key당 launch는
// 전역에서 정확히 하나다.
var ErrAlreadyLaunched = errors.New("accept: launch claim이 이미 존재")

// Acceptance는 접수 binding의 durable 내용이다. 비밀·tool args 원문을
// 담지 않는다.
type Acceptance struct {
	Version     int    `json:"version"`
	Scope       string `json:"scope"`
	Key         string `json:"idempotency_key"`
	Fingerprint string `json:"request_fingerprint"`
	TraceID     string `json:"trace_id"`
	PolicyHash  string `json:"policy_hash"`
	OperationID string `json:"operation_id"`
	CreatedAtMs int64  `json:"created_at_ms"`
}

// Status는 key 하나의 관측 상태다. 계약 §4의 lookup 상태 어휘 중
// 레지스트리가 스스로 증명할 수 있는 부분만 표현한다.
type Status struct {
	// State: "not_submitted" | "initializing" | "accepted".
	// running/terminal 판정은 세션 로그의 몫이다(레지스트리는 로그를 열지 않는다).
	State         string
	Acceptance    Acceptance // State != "not_submitted"일 때만 유효
	DBInitialized bool
	Launched      bool
	SessionExists bool // session.sqlite 파일 존재 여부 (stat만)
}

// Registry는 파일시스템 접수 레지스트리다. root는 절대 경로여야 하며
// 0700으로 생성·유지된다.
type Registry struct {
	root string
}

// Open은 레지스트리를 연다. root가 없으면 0700으로 만든다.
func Open(root string) (*Registry, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("accept: root는 절대 경로여야 함: %q", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("accept: root 생성: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("accept: root가 디렉터리가 아님: %q", root)
	}
	return &Registry{root: root}, nil
}

// KeyHash는 scope와 key의 결정론적 경로 식별자다.
func KeyHash(scope, key string) string {
	h := sha256.New()
	var buf [binary.MaxVarintLen64]byte
	for _, s := range []string{scope, key} {
		n := binary.PutUvarint(buf[:], uint64(len(s)))
		h.Write(buf[:n])
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Registry) keyDir(scope, key string) string {
	return filepath.Join(r.root, KeyHash(scope, key))
}

// SessionPath는 이 key의 정본 세션 로그 경로다. Rhizome이 --session으로
// 전달한 경로는 이 값과 일치해야 한다(계약 §3.3의 결정론적 경로).
func (r *Registry) SessionPath(scope, key string) string {
	return filepath.Join(r.keyDir(scope, key), SessionFileName)
}

// Claim은 key의 배타 점유를 시도한다.
//
//   - 점유 성공: (acc, true, nil) — 접수 binding이 durable하다.
//   - 같은 scope/key/fingerprint의 claim이 이미 존재: (기존 claim, false, nil)
//     — 호출자는 절대 새 spawn을 시작하지 않는다.
//   - 다른 fingerprint 또는 scope/key 원문 불일치: ErrKeyConflict.
//
// acc.Version은 무시되고 레지스트리 버전으로 강제된다.
func (r *Registry) Claim(acc Acceptance) (Acceptance, bool, error) {
	if acc.Scope == "" || acc.Key == "" || acc.Fingerprint == "" {
		return Acceptance{}, false, fmt.Errorf("accept: scope/key/fingerprint는 비어 있을 수 없음")
	}
	acc.Version = acceptanceVers
	dir := r.keyDir(acc.Scope, acc.Key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Acceptance{}, false, fmt.Errorf("accept: key 디렉터리 생성: %w", err)
	}
	content, err := json.Marshal(acc)
	if err != nil {
		return Acceptance{}, false, err
	}
	claimPath := filepath.Join(dir, claimFileName)
	tmp, err := os.CreateTemp(dir, claimFileName+".tmp.*")
	if err != nil {
		return Acceptance{}, false, fmt.Errorf("accept: claim 임시 파일: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return Acceptance{}, false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Acceptance{}, false, err
	}
	if err := tmp.Close(); err != nil {
		return Acceptance{}, false, err
	}
	if err := os.Link(tmpName, claimPath); err != nil {
		if !os.IsExist(err) {
			return Acceptance{}, false, fmt.Errorf("accept: claim 공개: %w", err)
		}
		existing, readErr := r.readClaim(dir)
		if readErr != nil {
			return Acceptance{}, false, readErr
		}
		if existing.Scope != acc.Scope || existing.Key != acc.Key {
			return Acceptance{}, false, fmt.Errorf("%w (keyhash 충돌: 저장된 scope/key 원문 불일치)", ErrKeyConflict)
		}
		if existing.Fingerprint != acc.Fingerprint {
			return Acceptance{}, false, fmt.Errorf("%w (fingerprint 불일치)", ErrKeyConflict)
		}
		return existing, false, nil
	}
	if err := syncDir(dir); err != nil {
		return Acceptance{}, false, err
	}
	return acc, true, nil
}

func (r *Registry) readClaim(dir string) (Acceptance, error) {
	data, err := os.ReadFile(filepath.Join(dir, claimFileName))
	if err != nil {
		return Acceptance{}, fmt.Errorf("accept: claim 읽기: %w", err)
	}
	var acc Acceptance
	if err := json.Unmarshal(data, &acc); err != nil {
		return Acceptance{}, fmt.Errorf("%w: %v", ErrClaimCorrupt, err)
	}
	if acc.Scope == "" || acc.Key == "" || acc.Fingerprint == "" {
		return Acceptance{}, fmt.Errorf("%w: 필수 필드 부재", ErrClaimCorrupt)
	}
	return acc, nil
}

// MarkDBInitialized는 session DB 초기화 완료를 durable하게 기록한다.
// 멱등이다(이미 있으면 성공).
func (r *Registry) MarkDBInitialized(scope, key string) error {
	return r.mark(scope, key, dbMarkerName, true)
}

// MarkLaunched는 외부 효과 직전의 launch claim이다. 이미 존재하면
// ErrAlreadyLaunched — scoped key당 launch는 하나다.
func (r *Registry) MarkLaunched(scope, key string) error {
	return r.mark(scope, key, launchName, false)
}

func (r *Registry) mark(scope, key, name string, idempotent bool) error {
	dir := r.keyDir(scope, key)
	if _, err := os.Stat(filepath.Join(dir, claimFileName)); err != nil {
		return fmt.Errorf("accept: claim 없는 key에 %s 마커 불가: %w", name, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			if idempotent {
				return nil
			}
			return ErrAlreadyLaunched
		}
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(dir)
}

// Lookup은 key의 접수 상태를 읽기 전용으로 관측한다. 세션 로그 파일은
// 열지 않는다(존재만 stat).
func (r *Registry) Lookup(scope, key string) (Status, error) {
	dir := r.keyDir(scope, key)
	if _, err := os.Stat(filepath.Join(dir, claimFileName)); err != nil {
		if os.IsNotExist(err) {
			return Status{State: "not_submitted"}, nil
		}
		return Status{}, err
	}
	acc, err := r.readClaim(dir)
	if err != nil {
		return Status{}, err
	}
	if acc.Scope != scope || acc.Key != key {
		return Status{}, fmt.Errorf("%w (keyhash 충돌: 저장된 scope/key 원문 불일치)", ErrKeyConflict)
	}
	status := Status{State: "initializing", Acceptance: acc}
	if _, err := os.Stat(filepath.Join(dir, dbMarkerName)); err == nil {
		status.DBInitialized = true
		status.State = "accepted"
	}
	if _, err := os.Stat(filepath.Join(dir, launchName)); err == nil {
		status.Launched = true
	}
	if _, err := os.Stat(filepath.Join(dir, SessionFileName)); err == nil {
		status.SessionExists = true
	}
	return status, nil
}

// syncDir은 디렉터리 엔트리 변경(link/create)을 fsync한다.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
