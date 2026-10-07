package logd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

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
	// Stream writers are line based; multi-line PEM is therefore not promised
	// on stdout/stderr (event payloads still use the full pattern).
	`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`, // PEM 개인키
	`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`,           // JWT
}

// Redactor는 로그 기록 전 redaction 패스다 (FR-LOG-08).
type Redactor struct {
	patterns      []*regexp.Regexp
	literalValues [][]byte
	maxLiteralLen int
}

// NewRedactor는 기본 패턴에 extra 정규식을 더해 컴파일한다.
func NewRedactor(extra ...string) (*Redactor, error) {
	return newRedactor(extra, nil)
}

// NewRedactorWithLiterals는 기본 패턴보다 먼저 적용되는 리터럴 묶음을
// 컴파일한다. 어댑터 secret처럼 기본 정규식이 값의 일부만 먹을 수 있는
// 값은 전체 리터럴을 먼저 치환해야 한다.
func NewRedactorWithLiterals(extra []string, literals []string) (*Redactor, error) {
	return newRedactor(extra, literals)
}

func newRedactor(extra, literals []string) (*Redactor, error) {
	all := make([]string, 0, len(defaultRedactionPatterns)+len(extra)+1)
	literalValues := literalVariants(literals)
	if pattern := literalPattern(literals); pattern != "" {
		all = append(all, pattern)
	}
	all = append(all, defaultRedactionPatterns...)
	all = append(all, extra...)
	r := &Redactor{literalValues: make([][]byte, 0, len(literalValues))}
	for _, value := range literalValues {
		r.literalValues = append(r.literalValues, []byte(value))
		if len(value) > r.maxLiteralLen {
			r.maxLiteralLen = len(value)
		}
	}
	for _, p := range all {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redaction 패턴 %q: %w", p, err)
		}
		r.patterns = append(r.patterns, re)
	}
	return r, nil
}

// literalPattern은 원문과 JSON 문자열(HTML escape on/off)에서 관측될 수
// 있는 값들을 하나의 longest-first alternation으로 만든다. 따옴표 포함
// 리터럴도 JSON 구조 안에서 값으로만 매칭되도록 escaped form을 함께 둔다.
func literalPattern(values []string) string {
	values = literalVariants(values)
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, regexp.QuoteMeta(value))
	}
	if len(parts) == 0 {
		return ""
	}
	return `(?:` + strings.Join(parts, `|`) + `)`
}

func literalVariants(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		add := func(v string) {
			if v != "" {
				seen[v] = struct{}{}
			}
		}
		add(value)
		var html bytes.Buffer
		enc := json.NewEncoder(&html)
		_ = enc.Encode(value)
		if encoded := strings.TrimSuffix(strings.TrimPrefix(html.String(), `"`), "\n"); strings.HasSuffix(encoded, `"`) {
			add(strings.TrimSuffix(encoded, `"`))
		}
		var plain bytes.Buffer
		enc = json.NewEncoder(&plain)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(value)
		if encoded := strings.TrimSuffix(strings.TrimPrefix(plain.String(), `"`), "\n"); strings.HasSuffix(encoded, `"`) {
			add(strings.TrimSuffix(encoded, `"`))
		}
	}
	parts := make([]string, 0, len(seen))
	for value := range seen {
		parts = append(parts, value)
	}
	sort.SliceStable(parts, func(i, j int) bool { return len(parts[i]) > len(parts[j]) })
	return parts
}

func (r *Redactor) literalOverlap() int {
	if r == nil || r.maxLiteralLen < 2 {
		return 0
	}
	return r.maxLiteralLen - 1
}

func (r *Redactor) redactBytes(b []byte) []byte {
	for _, re := range r.patterns {
		b = re.ReplaceAll(b, []byte(Redacted))
	}
	return b
}

// RedactString applies the same redaction pass used by the event writer to a
// control or diagnostic string.
func (r *Redactor) RedactString(value string) string {
	if r == nil {
		return value
	}
	return string(r.redactBytes([]byte(value)))
}

// RedactBytes applies the same redaction pass to arbitrary output bytes.
func (r *Redactor) RedactBytes(value []byte) []byte {
	if r == nil {
		return value
	}
	return r.redactBytes(value)
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
