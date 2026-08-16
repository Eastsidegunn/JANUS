package logd

import (
	"encoding/base64"
	"fmt"
	"regexp"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// Redacted는 마스킹된 자격증명을 대체하는 문자열이다.
// JSON 문자열 내부에 안전하게 삽입 가능해야 한다(따옴표·역슬래시 금지).
const Redacted = "[REDACTED]"

// defaultRedactionPatterns는 FR-LOG-08의 기본 자격증명 패턴이다.
// 규칙은 NewRedactor의 extra 인자로 확장 가능하다.
var defaultRedactionPatterns = []string{
	`AKIA[0-9A-Z]{16}`,                       // AWS access key ID
	`sk-ant-[A-Za-z0-9_-]{10,}`,              // Anthropic API key
	`sk-[A-Za-z0-9_-]{20,}`,                  // OpenAI 류 secret key
	`(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`, // GitHub 토큰
	`github_pat_[A-Za-z0-9_]{20,}`,           // GitHub fine-grained PAT
	`xox[baprs]-[A-Za-z0-9-]{10,}`,           // Slack 토큰
	`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`, // PEM 개인키
	`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`,           // JWT
}

// Redactor는 로그 기록 전 redaction 패스다 (FR-LOG-08).
type Redactor struct {
	patterns []*regexp.Regexp
}

// NewRedactor는 기본 패턴에 extra 정규식을 더해 컴파일한다.
func NewRedactor(extra ...string) (*Redactor, error) {
	all := append(append([]string{}, defaultRedactionPatterns...), extra...)
	r := &Redactor{}
	for _, p := range all {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redaction 패턴 %q: %w", p, err)
		}
		r.patterns = append(r.patterns, re)
	}
	return r, nil
}

func (r *Redactor) redactBytes(b []byte) []byte {
	for _, re := range r.patterns {
		b = re.ReplaceAll(b, []byte(Redacted))
	}
	return b
}

// RedactEvent는 payload와 raw(base64 디코드 후)를 마스킹한다.
// raw는 원본 보존(FR-LOG-07) 대상이지만 자격증명은 원본에서도 마스킹된 채
// 보존된다 — redaction은 기록 전 패스이므로(FR-LOG-08) 마스킹 전 값은
// 어디에도 남지 않는다.
func (r *Redactor) RedactEvent(rec *gen.EventRecord) error {
	rec.Payload = r.redactBytes(rec.Payload)
	if rec.Raw != nil {
		decoded, err := base64.StdEncoding.DecodeString(*rec.Raw)
		if err != nil {
			return fmt.Errorf("raw base64 디코드: %w", err)
		}
		masked := base64.StdEncoding.EncodeToString(r.redactBytes(decoded))
		rec.Raw = &masked
	}
	return nil
}
