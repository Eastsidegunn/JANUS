// Package policy는 정책 프로파일의 병합과 순수 평가를 구현한다 (FR-POL).
// 평가는 순수 함수다: (프로파일, spawn 요청) → 거부 또는 샌드박스 설정
// (FR-POL-02). 병합은 강화만 허용한다: allow 리스트는 교집합, 예산은
// 최솟값 — 어떤 오버레이도 상위 프로파일보다 넓은 권한을 만들 수 없다
// (FR-POL-03, 속성 테스트로 CI 검증).
//
// YAML 프로파일 파싱(FR-POL-01)은 외부 라이브러리 승인 후 이 패키지에
// 추가된다 — docs/t6-yaml-proposal.md 참조.
package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// ApprovalMode는 승인 모드다. 자동 승인은 프로파일에서 명시적으로만
// 켤 수 있다(FR-POL-05) — zero value는 manual이 아니므로 파서가 명시를
// 강제한다.
type ApprovalMode string

const (
	ApprovalManual ApprovalMode = "manual"
	ApprovalAuto   ApprovalMode = "auto"
)

// Profile은 v0.1 정책 프로파일이다 (FR-POL-01 필드).
type Profile struct {
	ID       string
	FSScope  []string     // 마운트 허용 경로 (FR-SBX-02의 입력)
	Egress   []string     // egress allow 도메인 (FR-SBX-03의 입력)
	Budget   gen.Budget   // 토큰/시간/spawn 깊이 — contracts 계약 재사용
	Approval ApprovalMode // manual | auto
}

// Merge는 base 위에 overlay를 얹는다 — 강화만 가능하다 (FR-POL-03):
// allow 리스트는 교집합(정렬), 예산은 축별 최솟값, 승인은 양쪽 모두
// auto일 때만 auto(manual이 이긴다). ID는 계보를 남긴다.
func Merge(base, overlay Profile) Profile {
	approval := ApprovalManual
	if base.Approval == ApprovalAuto && overlay.Approval == ApprovalAuto {
		approval = ApprovalAuto
	}
	return Profile{
		ID:      mergeID(base.ID, overlay.ID),
		FSScope: intersect(base.FSScope, overlay.FSScope),
		Egress:  intersect(base.Egress, overlay.Egress),
		Budget: gen.Budget{
			Tokens:   min64(base.Budget.Tokens, overlay.Budget.Tokens),
			TimeMs:   min64(base.Budget.TimeMs, overlay.Budget.TimeMs),
			MaxDepth: min64(base.Budget.MaxDepth, overlay.Budget.MaxDepth),
		},
		Approval: approval,
	}
}

func mergeID(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "+" + b
	}
}

func intersect(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range a {
		set[s] = true
	}
	out := []string{}
	seen := map[string]bool{}
	for _, s := range b {
		if set[s] && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	sort.Strings(out)
	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// SpawnRequest는 서브에이전트 spawn 요청의 정책 평가 입력이다.
type SpawnRequest struct {
	Adapter   string
	Workspace string   // 요청 워크스페이스 경로
	Egress    []string // 요청 egress 도메인
	Depth     int64    // 이 spawn이 위치할 깊이 (root 직계 자식 = 0)
}

// SandboxConfig는 평가를 통과한 spawn의 샌드박스 설정이다 (FR-POL-02).
// 정책 집행은 이 설정이 샌드박스 환경으로 구워지는 것으로 이루어진다
// (FR-POL-04 — T10).
type SandboxConfig struct {
	ProfileID string
	Workspace string
	// Egress는 요청 도메인 중 정책 평가를 통과한 것만이다 — 통과분만
	// 실행 프로파일에 편입된다(FR-EXT-06과 동일 의미론).
	Egress []string
	// DeniedEgress는 요청됐으나 거부된 도메인이다 — policy/decision
	// 이벤트의 사유 재료.
	DeniedEgress []string
	Budget       gen.Budget
	Approval     ApprovalMode
}

// Denial은 spawn 거부다. 사유는 policy/decision 이벤트로 기록된다.
type Denial struct {
	Reason string
}

func (d *Denial) Error() string { return "policy: spawn 거부: " + d.Reason }

// Evaluate는 순수 함수다: 프로파일과 spawn 요청만으로 판정하며 부수효과가
// 없다 (FR-POL-02). 거부 사유:
//   - 워크스페이스가 fs 스코프 밖 (경로 자체 또는 스코프 경로의 하위만 허용)
//   - spawn 깊이가 예산 한도 이상 (depth >= max_depth)
//
// 요청 egress 중 allowlist 밖 도메인은 spawn 자체를 거부하지 않고
// 편입에서 제외된다(DeniedEgress에 기록) — 차단은 샌드박스가 한다(FR-SBX-03).
func Evaluate(p Profile, req SpawnRequest) (SandboxConfig, *Denial) {
	if req.Depth >= p.Budget.MaxDepth {
		return SandboxConfig{}, &Denial{Reason: fmt.Sprintf(
			"spawn 깊이 %d가 한도 %d 이상 (FR-ADP-08)", req.Depth, p.Budget.MaxDepth)}
	}
	if !workspaceAllowed(p.FSScope, req.Workspace) {
		return SandboxConfig{}, &Denial{Reason: fmt.Sprintf(
			"워크스페이스 %q가 fs 스코프 %v 밖", req.Workspace, p.FSScope)}
	}
	allowed := map[string]bool{}
	for _, d := range p.Egress {
		allowed[d] = true
	}
	granted := []string{}
	denied := []string{}
	for _, d := range req.Egress {
		if allowed[d] {
			granted = append(granted, d)
		} else {
			denied = append(denied, d)
		}
	}
	sort.Strings(granted)
	sort.Strings(denied)
	return SandboxConfig{
		ProfileID:    p.ID,
		Workspace:    req.Workspace,
		Egress:       granted,
		DeniedEgress: denied,
		Budget:       p.Budget,
		Approval:     p.Approval,
	}, nil
}

// workspaceAllowed는 요청 경로가 스코프 경로와 같거나 그 하위인지 본다.
// 접두사 문자열 매칭이 아니라 경로 구분자 경계를 지킨다 —
// "/workspace"가 "/workspace-evil"을 허용하면 안 된다.
func workspaceAllowed(scope []string, workspace string) bool {
	for _, s := range scope {
		if workspace == s || strings.HasPrefix(workspace, strings.TrimSuffix(s, "/")+"/") {
			return true
		}
	}
	return false
}

// Usage는 예산 소비 현황이다 (FR-POL-06 판정 입력).
type Usage struct {
	Tokens    int64 // 소비 토큰
	ElapsedMs int64 // 경과 시간
	Depth     int64 // 시도하려는 spawn의 깊이
}

// ExceededReason은 예산 초과 판정의 순수 함수다 (FR-POL-06). 초과면
// (사유, true) — 호출자는 해당 서브에이전트를 stop하고 사유를 이벤트로
// 기록한다. 토큰·시간은 한도를 넘어서면(>) 초과, spawn 깊이는 한도에
// 도달하면(>=) 새 spawn이 차단된다.
func ExceededReason(b gen.Budget, u Usage) (string, bool) {
	switch {
	case u.Tokens > b.Tokens:
		return fmt.Sprintf("토큰 예산 초과 (%d > %d)", u.Tokens, b.Tokens), true
	case u.ElapsedMs > b.TimeMs:
		return fmt.Sprintf("시간 예산 초과 (%dms > %dms)", u.ElapsedMs, b.TimeMs), true
	case u.Depth >= b.MaxDepth:
		return fmt.Sprintf("spawn 깊이 한도 도달 (%d >= %d)", u.Depth, b.MaxDepth), true
	}
	return "", false
}
