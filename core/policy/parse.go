package policy

import (
	"bytes"
	"fmt"
	"io"
	"path"

	yaml "github.com/goccy/go-yaml"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// profileYAML은 파싱 중간 형태다. 포인터로 "명시 여부"를 구분한다 —
// strict 모드는 미지 필드·중복 키만 거부할 뿐 required·enum·비음수를
// 강제하지 않으므로([H] 리뷰 확인) 검증은 여기서 별도로 한다.
type profileYAML struct {
	ID       *string     `yaml:"id"`
	FSScope  []string    `yaml:"fs_scope"`
	Egress   []string    `yaml:"egress"`
	Budget   *budgetYAML `yaml:"budget"`
	Approval *string     `yaml:"approval"`
}

type budgetYAML struct {
	Tokens   *int64 `yaml:"tokens"`
	TimeMs   *int64 `yaml:"time_ms"`
	MaxDepth *int64 `yaml:"max_depth"`
}

// ParseProfile은 선언적 YAML 프로파일을 파싱·검증한다 (FR-POL-01).
//
// fail-closed 규칙:
//   - 미지 필드·중복 키 거부 (yaml.Strict + goccy 기본 동작)
//   - 다중 문서 거부 — UnmarshalWithOptions는 첫 문서만 읽고 성공하므로
//     ([H] 리뷰 실증) Decoder로 읽은 뒤 두 번째 decode가 io.EOF임을 확인
//   - id 필수(비어 있지 않음)
//   - approval 명시 필수, manual|auto만 — 자동 승인은 명시적으로만
//     켜진다(FR-POL-05). zero value로 auto가 되는 경로 없음
//   - budget 필수, 세 축(tokens/time_ms/max_depth) 전부 명시, 비음수
//   - fs_scope 엔트리: 비어 있지 않은 절대 경로, POSIX 정규화해 저장
//   - egress 엔트리: 비어 있지 않음
func ParseProfile(data []byte) (Profile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data), yaml.Strict())
	var raw profileYAML
	if err := dec.Decode(&raw); err != nil {
		return Profile{}, fmt.Errorf("policy: 프로파일 파싱: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Profile{}, fmt.Errorf("policy: 프로파일은 단일 YAML 문서여야 함 (두 번째 문서 감지)")
	}

	if raw.ID == nil || *raw.ID == "" {
		return Profile{}, fmt.Errorf("policy: id는 비어 있지 않게 명시해야 함")
	}
	if raw.Approval == nil {
		return Profile{}, fmt.Errorf("policy: approval은 명시해야 함 (manual|auto) — 기본값 없음 (FR-POL-05)")
	}
	approval := ApprovalMode(*raw.Approval)
	if approval != ApprovalManual && approval != ApprovalAuto {
		return Profile{}, fmt.Errorf("policy: approval %q — manual|auto만 허용", *raw.Approval)
	}
	if raw.Budget == nil {
		return Profile{}, fmt.Errorf("policy: budget은 명시해야 함 (tokens/time_ms/max_depth)")
	}
	budget, err := raw.Budget.validate()
	if err != nil {
		return Profile{}, err
	}
	scope := make([]string, 0, len(raw.FSScope))
	for _, s := range raw.FSScope {
		if s == "" || !path.IsAbs(s) {
			return Profile{}, fmt.Errorf("policy: fs_scope 엔트리 %q — 비어 있지 않은 절대 경로여야 함", s)
		}
		scope = append(scope, path.Clean(s))
	}
	for _, d := range raw.Egress {
		if d == "" {
			return Profile{}, fmt.Errorf("policy: egress 엔트리는 비어 있으면 안 됨")
		}
	}
	return Profile{
		ID:       *raw.ID,
		FSScope:  scope,
		Egress:   raw.Egress,
		Budget:   budget,
		Approval: approval,
	}, nil
}

func (b *budgetYAML) validate() (gen.Budget, error) {
	axes := []struct {
		name string
		v    *int64
	}{
		{"tokens", b.Tokens},
		{"time_ms", b.TimeMs},
		{"max_depth", b.MaxDepth},
	}
	for _, a := range axes {
		if a.v == nil {
			return gen.Budget{}, fmt.Errorf("policy: budget.%s는 명시해야 함", a.name)
		}
		if *a.v < 0 {
			return gen.Budget{}, fmt.Errorf("policy: budget.%s %d — 비음수여야 함", a.name, *a.v)
		}
	}
	return gen.Budget{Tokens: *b.Tokens, TimeMs: *b.TimeMs, MaxDepth: *b.MaxDepth}, nil
}
