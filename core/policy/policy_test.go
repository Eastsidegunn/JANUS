package policy

import (
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

const extensionHash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func testExtension(name, source string) gen.Extension {
	return gen.Extension{Name: name, Version: "1.2.3", Integrity: extensionHash, Source: source, Egress: []string{"API.Example.com.", "api.example.com"}}
}

func b(tokens, timeMs, depth int64) gen.Budget {
	return gen.Budget{Tokens: tokens, TimeMs: timeMs, MaxDepth: depth}
}

// FR-POL-03: 교집합·최솟값·승인 강화 단위 테스트 (속성 테스트는 core 패키지).
func TestMerge(t *testing.T) {
	base := Profile{
		ID:                "org",
		FSScope:           []string{"/workspace", "/data"},
		Egress:            []string{"github.com", "pypi.org", "registry.npmjs.org"},
		AllowedExtensions: []string{"mcp-fs", "lint@registry.example"},
		AllowedRegistries: []string{"Registry.Example."},
		Budget:            b(100_000, 600_000, 3),
		Approval:          ApprovalAuto,
	}
	overlay := Profile{
		ID:                "task",
		FSScope:           []string{"/workspace"},
		Egress:            []string{"registry.npmjs.org", "github.com", "evil.example"},
		AllowedExtensions: []string{"mcp-fs", "lint@registry.example"},
		AllowedRegistries: []string{"registry.example"},
		Budget:            b(50_000, 900_000, 2),
		Approval:          ApprovalAuto,
	}
	m := Merge(base, overlay)
	if m.ID != "org+task" {
		t.Errorf("ID = %s", m.ID)
	}
	if len(m.FSScope) != 1 || m.FSScope[0] != "/workspace" {
		t.Errorf("FSScope = %v", m.FSScope)
	}
	if len(m.Egress) != 2 || m.Egress[0] != "github.com" || m.Egress[1] != "registry.npmjs.org" {
		t.Errorf("Egress = %v (교집합·정렬 기대)", m.Egress)
	}
	if len(m.AllowedExtensions) != 2 || m.AllowedExtensions[0] != "lint@registry.example" || m.AllowedExtensions[1] != "mcp-fs" {
		t.Errorf("AllowedExtensions = %v (교집합·정렬 기대)", m.AllowedExtensions)
	}
	if len(m.AllowedRegistries) != 1 || m.AllowedRegistries[0] != "registry.example" {
		t.Errorf("AllowedRegistries = %v (canonical 교집합 기대)", m.AllowedRegistries)
	}
	if m.Budget != b(50_000, 600_000, 2) {
		t.Errorf("Budget = %+v (축별 최솟값 기대)", m.Budget)
	}
	if m.Approval != ApprovalAuto {
		t.Errorf("양쪽 auto인데 %s", m.Approval)
	}

	// 한쪽이라도 manual이면 manual — 자동 승인은 완화될 수 없다 (FR-POL-05)
	overlay.Approval = ApprovalManual
	if Merge(base, overlay).Approval != ApprovalManual {
		t.Error("manual 오버레이가 auto로 완화됨")
	}
	base2 := base
	base2.Approval = ApprovalManual
	overlay.Approval = ApprovalAuto
	if Merge(base2, overlay).Approval != ApprovalManual {
		t.Error("auto 오버레이가 manual 상위를 완화함")
	}
}

func TestMergeExtensionSelectorSpecificity(t *testing.T) {
	got := Merge(
		Profile{AllowedExtensions: []string{"mcp-fs"}},
		Profile{AllowedExtensions: []string{"mcp-fs@Registry.Example."}},
	).AllowedExtensions
	if len(got) != 1 || got[0] != "mcp-fs@registry.example" {
		t.Fatalf("name selector와 name@source 교집합이 source-specific으로 좁혀지지 않음: %v", got)
	}
}

// FR-POL-02: 평가는 순수 함수 — 같은 입력은 같은 판정, 입력 불변.
func TestEvaluatePure(t *testing.T) {
	p := Profile{
		ID:      "p",
		FSScope: []string{"/workspace"},
		Egress:  []string{"github.com"},
		Budget:  b(1000, 1000, 2),
	}
	req := SpawnRequest{Adapter: "null", Workspace: "/workspace/sub", Egress: []string{"github.com", "evil.example"}, Depth: 0}
	c1, d1 := Evaluate(p, req)
	c2, d2 := Evaluate(p, req)
	if d1 != nil || d2 != nil {
		t.Fatalf("거부됨: %v %v", d1, d2)
	}
	if len(c1.Egress) != len(c2.Egress) || c1.ProfileID != c2.ProfileID {
		t.Fatal("평가가 비결정적")
	}
	if len(c1.Egress) != 1 || c1.Egress[0] != "github.com" {
		t.Errorf("통과 도메인 = %v", c1.Egress)
	}
	if len(c1.DeniedEgress) != 1 || c1.DeniedEgress[0] != "evil.example" {
		t.Errorf("거부 도메인 = %v", c1.DeniedEgress)
	}
}

func TestEvaluateWorkspaceScope(t *testing.T) {
	p := Profile{FSScope: []string{"/workspace"}, Budget: b(1, 1, 5)}
	cases := []struct {
		ws    string
		allow bool
	}{
		{"/workspace", true},
		{"/workspace/proj", true},
		{"/workspace-evil", false}, // 경로 경계 — 접두사 문자열 매칭 금지
		{"/etc", false},
		{"/", false},
		// [H] 재리뷰 재현 3종의 영구 회귀 — 경로 탈출은 정규화 후 판정
		{"/workspace/../etc", false},
		{"/workspace/sub/../../etc", false},
		{"", false},                // 빈 경로 거부
		{"workspace/x", false},     // 상대 경로 거부
		{"/workspace/./sub", true}, // 정규화 후 스코프 안
	}
	for _, c := range cases {
		_, denial := Evaluate(p, SpawnRequest{Workspace: c.ws, Depth: 0})
		if (denial == nil) != c.allow {
			t.Errorf("workspace %q: denial=%v (allow=%v 기대)", c.ws, denial, c.allow)
		}
	}
}

// [H] 재리뷰 재현: 빈 문자열 스코프 엔트리는 아무 권한도 부여하지 않는다.
func TestEvaluateEmptyScopeEntryGrantsNothing(t *testing.T) {
	p := Profile{FSScope: []string{""}, Budget: b(1, 1, 5)}
	if _, denial := Evaluate(p, SpawnRequest{Workspace: "/etc", Depth: 0}); denial == nil {
		t.Fatal(`FSScope [""]가 /etc를 허용함 — fail-open`)
	}
	rel := Profile{FSScope: []string{"workspace"}, Budget: b(1, 1, 5)}
	if _, denial := Evaluate(rel, SpawnRequest{Workspace: "/workspace", Depth: 0}); denial == nil {
		t.Fatal("상대 경로 스코프 엔트리가 권한을 부여함")
	}
}

// T6 재재리뷰 차단의 영구 회귀: 정규화하면 루트가 되는 스코프 엔트리는
// 암묵적 루트다 — 무권한. 루트는 원문이 정확히 "/"일 때만 허용된다.
func TestEvaluateImplicitRootScopeGrantsNothing(t *testing.T) {
	implicit := []string{"/workspace/..", "//", "/./"}
	for _, entry := range implicit {
		p := Profile{FSScope: []string{entry}, Budget: b(1, 1, 5)}
		if _, denial := Evaluate(p, SpawnRequest{Workspace: "/etc", Depth: 0}); denial == nil {
			t.Errorf("암묵적 루트 스코프 %q가 /etc를 허용함", entry)
		}
	}
	// 명시적 루트는 허용된다
	root := Profile{FSScope: []string{"/"}, Budget: b(1, 1, 5)}
	if _, denial := Evaluate(root, SpawnRequest{Workspace: "/etc", Depth: 0}); denial != nil {
		t.Errorf("명시적 루트 스코프가 거부됨: %v", denial)
	}
}

// SandboxConfig.Workspace는 정규화된 경로다 — 하류(T10)에 비정규 경로 금지.
func TestEvaluateReturnsNormalizedWorkspace(t *testing.T) {
	p := Profile{FSScope: []string{"/workspace"}, Budget: b(1, 1, 5)}
	cfg, denial := Evaluate(p, SpawnRequest{Workspace: "/workspace/./a/b/../sub", Depth: 0})
	if denial != nil {
		t.Fatal(denial)
	}
	if cfg.Workspace != "/workspace/a/sub" {
		t.Fatalf("Workspace = %q (정규화 기대)", cfg.Workspace)
	}
}

func TestEvaluateDepthDenial(t *testing.T) {
	p := Profile{FSScope: []string{"/w"}, Budget: b(1, 1, 2)}
	if _, denial := Evaluate(p, SpawnRequest{Workspace: "/w", Depth: 1}); denial != nil {
		t.Errorf("한도 안 깊이가 거부됨: %v", denial)
	}
	if _, denial := Evaluate(p, SpawnRequest{Workspace: "/w", Depth: 2}); denial == nil {
		t.Error("깊이 == 한도인데 허용됨 (>= 차단이어야 함)")
	} else if !strings.Contains(denial.Reason, "깊이") {
		t.Errorf("사유 이상: %s", denial.Reason)
	}
}

// FR-POL-06 완료 기준: 예산 초과 판정 단위 테스트.
func TestExceededReason(t *testing.T) {
	budget := b(100, 1000, 2)
	cases := []struct {
		name     string
		usage    Usage
		exceeded bool
		contains string
	}{
		{"전부 한도 안", Usage{Tokens: 50, ElapsedMs: 500, Depth: 1}, false, ""},
		{"토큰 정확히 소진(경계)", Usage{Tokens: 100, ElapsedMs: 0, Depth: 0}, false, ""},
		{"토큰 초과", Usage{Tokens: 101}, true, "토큰"},
		{"시간 정확히 소진(경계)", Usage{ElapsedMs: 1000}, false, ""},
		{"시간 초과", Usage{ElapsedMs: 1001}, true, "시간"},
		{"깊이 한도 도달(>=)", Usage{Depth: 2}, true, "깊이"},
		{"깊이 한도 초과", Usage{Depth: 3}, true, "깊이"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, exceeded := ExceededReason(budget, c.usage)
			if exceeded != c.exceeded {
				t.Fatalf("exceeded=%v (%v 기대), reason=%s", exceeded, c.exceeded, reason)
			}
			if c.exceeded && !strings.Contains(reason, c.contains) {
				t.Fatalf("사유 %q에 %q 없음", reason, c.contains)
			}
			if !c.exceeded && reason != "" {
				t.Fatalf("초과 아닌데 사유 존재: %s", reason)
			}
		})
	}
}

func TestIntersectDeterministic(t *testing.T) {
	a := []string{"c", "a", "b", "a"}
	bb := []string{"b", "a", "c", "z"}
	got := intersect(a, bb)
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("intersect = %v (정렬·중복 제거 기대)", got)
	}
	if len(intersect(a, nil)) != 0 {
		t.Fatal("공집합 교집합이 비어 있지 않음")
	}
}

func TestEvaluateExtensionsAndExecutionEgress(t *testing.T) {
	p := Profile{
		ID:                "ext-policy",
		FSScope:           []string{"/workspace"},
		Egress:            []string{"api.example.com", "registry.example"},
		AllowedExtensions: []string{"mcp-fs"},
		AllowedRegistries: []string{"REGISTRY.EXAMPLE."},
		Budget:            b(10, 10, 2),
	}
	ext := testExtension("mcp-fs", "Registry.Example.")
	ext.Egress = []string{"API.Example.com."}
	cfg, denial := Evaluate(p, SpawnRequest{Workspace: "/workspace", Depth: 0, Extensions: []gen.Extension{ext}})
	if denial != nil {
		t.Fatalf("허용 확장이 거부됨: %v", denial)
	}
	if len(cfg.Extensions) != 1 || cfg.Extensions[0].Source != "registry.example" {
		t.Fatalf("정규화된 확장 이상: %+v", cfg.Extensions)
	}
	if len(cfg.Egress) != 1 || cfg.Egress[0] != "api.example.com" {
		t.Fatalf("실행 egress 편입 이상: %v", cfg.Egress)
	}
	// source registry is not inherited merely because provisioning used it.
	if contains(cfg.Egress, "registry.example") {
		t.Fatal("source registry가 실행 egress에 자동 편입됨")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestEvaluateExtensionDenialsHaveNoSideEffects(t *testing.T) {
	base := Profile{FSScope: []string{"/workspace"}, Budget: b(10, 10, 2)}
	ext := testExtension("mcp-fs", "registry.example")
	cases := []struct {
		name string
		p    Profile
		ext  gen.Extension
	}{
		{"omitted allowlists deny", base, ext},
		{"name outside intersection", Profile{FSScope: []string{"/workspace"}, AllowedExtensions: []string{"other"}, AllowedRegistries: []string{"registry.example"}, Budget: b(10, 10, 2)}, ext},
		{"registry outside intersection", Profile{FSScope: []string{"/workspace"}, AllowedExtensions: []string{"mcp-fs"}, AllowedRegistries: []string{"other.example"}, Budget: b(10, 10, 2)}, ext},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, denial := Evaluate(tc.p, SpawnRequest{Workspace: "/workspace", Depth: 0, Extensions: []gen.Extension{tc.ext}})
			if denial == nil {
				t.Fatal("정책 밖 확장이 허용됨")
			}
			if cfg.Extensions != nil || cfg.Egress != nil {
				t.Fatalf("거부 시 실행 설정이 남음: %+v", cfg)
			}
		})
	}
}

func TestNormalizeExtensionRejectsUnpinnedDeclarations(t *testing.T) {
	valid := testExtension("mcp-fs", "registry.example")
	for _, version := range []string{"latest", "^1.2.3", ">=1.0.0", "v1.2.3", "1.2"} {
		ext := valid
		ext.Version = version
		if _, err := NormalizeExtension(ext); err == nil {
			t.Errorf("version %q가 허용됨", version)
		}
	}
	for _, integrity := range []string{"md5:abc", "sha256:abc", "sha256:" + strings.Repeat("A", 64)} {
		ext := valid
		ext.Integrity = integrity
		if _, err := NormalizeExtension(ext); err == nil {
			t.Errorf("integrity %q가 허용됨", integrity)
		}
	}
}
