package policy

import (
	"strings"
	"testing"
)

const validProfileYAML = `
id: opaque-default
fs_scope:
  - /workspace
  - /tmp/scratch/../scratch
egress:
  - registry.npmjs.org
  - github.com
budget:
  tokens: 100000
  time_ms: 600000
  max_depth: 2
approval: manual
allowed_extensions:
  - mcp-fs
  - lint@registry.example
allowed_registries:
  - Registry.Example.
`

func TestParseProfileValid(t *testing.T) {
	p, err := ParseProfile([]byte(validProfileYAML))
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "opaque-default" || p.Approval != ApprovalManual {
		t.Fatalf("파싱 결과 이상: %+v", p)
	}
	if p.Budget.Tokens != 100000 || p.Budget.TimeMs != 600000 || p.Budget.MaxDepth != 2 {
		t.Fatalf("budget 이상: %+v", p.Budget)
	}
	// fs_scope는 정규화되어 저장된다
	if len(p.FSScope) != 2 || p.FSScope[1] != "/tmp/scratch" {
		t.Fatalf("fs_scope 정규화 안 됨: %v", p.FSScope)
	}
	if len(p.AllowedExtensions) != 2 || len(p.AllowedRegistries) != 1 || p.AllowedRegistries[0] != "registry.example" {
		t.Fatalf("확장 정책 정규화 이상: extensions=%v registries=%v", p.AllowedExtensions, p.AllowedRegistries)
	}
}

// fail-closed: 위반 프로파일은 전부 거부된다 ([H] 승인 조건의 검증 목록).
func TestParseProfileRejections(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{"미지 필드", `
id: p
unknown_field: 1
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "unknown"},
		{"중복 키", `
id: p
id: q
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "already defined"},
		{"다중 문서", validProfileYAML + "\n---\nid: second\nbudget: {tokens: 1, time_ms: 1, max_depth: 1}\napproval: manual\n", "단일 YAML 문서"},
		{"id 누락", `
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "id"},
		{"approval 누락 — 기본값 없음", `
id: p
budget: {tokens: 1, time_ms: 1, max_depth: 1}
`, "approval"},
		{"approval 미지값", `
id: p
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: always
`, "manual|auto"},
		{"budget 누락", `
id: p
approval: manual
`, "budget"},
		{"budget 축 누락", `
id: p
budget: {tokens: 1, time_ms: 1}
approval: manual
`, "max_depth"},
		{"음수 예산", `
id: p
budget: {tokens: -1, time_ms: 1, max_depth: 1}
approval: manual
`, "비음수"},
		{"fs_scope 상대 경로", `
id: p
fs_scope: [workspace]
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "절대 경로"},
		{"fs_scope 빈 엔트리", `
id: p
fs_scope: [""]
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "절대 경로"},
		{"egress 빈 엔트리", `
id: p
egress: [""]
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "egress"},
		{"allowed_extensions 빈 엔트리", `
id: p
allowed_extensions: [""]
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "allowed_extensions"},
		{"allowed_registries 경로", `
id: p
allowed_registries: ["https://registry.example"]
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "allowed_registries"},
		{"암묵적 루트 스코프 — /workspace/..", `
id: p
fs_scope: ["/workspace/.."]
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "루트"},
		{"암묵적 루트 스코프 — //", `
id: p
fs_scope: ["//"]
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "루트"},
		{"암묵적 루트 스코프 — /./", `
id: p
fs_scope: ["/./"]
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`, "루트"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseProfile([]byte(c.yaml))
			if err == nil {
				t.Fatal("위반 프로파일이 파싱됨")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.wantErr)) {
				t.Fatalf("오류 %q에 %q 없음", err, c.wantErr)
			}
		})
	}
}

// 명시적 루트 스코프("/")는 파싱된다 — 암묵과 명시의 경계 회귀.
func TestParseProfileExplicitRootScope(t *testing.T) {
	p, err := ParseProfile([]byte(`
id: root-profile
fs_scope: ["/"]
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: manual
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.FSScope) != 1 || p.FSScope[0] != "/" {
		t.Fatalf("fs_scope = %v", p.FSScope)
	}
}

// 자동 승인은 명시적으로만 켜진다 (FR-POL-05).
func TestParseProfileExplicitAuto(t *testing.T) {
	p, err := ParseProfile([]byte(`
id: trusted
budget: {tokens: 1, time_ms: 1, max_depth: 1}
approval: auto
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Approval != ApprovalAuto {
		t.Fatalf("approval = %s", p.Approval)
	}
}
