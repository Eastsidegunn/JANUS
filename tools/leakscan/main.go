// Command leakscan rejects tracked files that contain credentials or other
// repository-private values. It intentionally imports only the standard
// library and is independent from the product packages.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const maxFileSize = 2 * 1024 * 1024

type category string

const (
	secrets      category = "secrets"
	privateIP    category = "private_ip"
	personalPath category = "personal_path"
	internalID   category = "internal_id"
	hosts        category = "hosts"
	private      category = "private"
	skip         category = "skip"
)

var categories = []category{secrets, privateIP, personalPath, internalID, hosts, private, skip}

type detector struct {
	category     category
	pattern      *regexp.Regexp
	privateIndex int
}

type allowRule struct {
	category category
	pathRE   *regexp.Regexp
	matcher  *regexp.Regexp
	exact    string
	skipOnly bool
	useCount int
	line     int
}

type finding struct {
	path         string
	line         int
	start        int
	match        string
	category     category
	privateIndex int
	reason       string
	allowed      bool
}

type scanResult struct {
	findings      []finding
	violations    map[category]int
	allowed       map[category]int
	skippedBinary int
	skippedLarge  int
	skippedOther  int
	tracked       int
}

func defaultDetectors() []detector {
	octet := `(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])`
	ip := `\b(?:10\.` + octet + `\.` + octet + `\.` + octet + `|172\.(?:1[6-9]|2[0-9]|3[01])\.` + octet + `\.` + octet + `|192\.168\.` + octet + `\.` + octet + `|100\.(?:6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.` + octet + `\.` + octet + `)\b`
	raw := []struct {
		c category
		r string
	}{
		{secrets, `AKIA[0-9A-Z]{16}`},
		{secrets, `sk-ant-[A-Za-z0-9_-]{10,}`},
		{secrets, `sk-[A-Za-z0-9_-]{20,}`},
		{secrets, `github_pat_[A-Za-z0-9_]{20,}`},
		{secrets, `gh[pousr]_[A-Za-z0-9]{20,}`},
		{secrets, `xox[abprs]-[A-Za-z0-9-]{10,}`},
		{secrets, `-----BEGIN [A-Z ]*PRIVATE KEY( BLOCK)?-----`},
		{secrets, `eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.`},
		{privateIP, ip},
		{personalPath, `/Users/[A-Za-z0-9][A-Za-z0-9._-]*`},
		{personalPath, `/home/[A-Za-z0-9._-]+`},
		{personalPath, `(?i)\b[A-Z]:[\\/]Users[\\/][A-Za-z0-9._-]+`},
		{internalID, `\bq-[0-9a-f]{6,32}\b`},
		{internalID, `(?i)\b(run|job)(?:_id)?[ #:=_-]*[0-9]{9,}\b`},
		{internalID, `actions/runs/[0-9]+`},
		{hosts, `(?i)[a-z0-9-]+\.ts\.net\b`},
	}
	out := make([]detector, 0, len(raw))
	for _, item := range raw {
		out = append(out, detector{category: item.c, pattern: regexp.MustCompile(item.r)})
	}
	return out
}

func loadAllow(path string) ([]allowRule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read allow list: %w", err)
	}
	var rules []allowRule
	for lineNo, line := range strings.Split(string(b), "\n") {
		lineNo++
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		before, reason, ok := strings.Cut(line, " # ")
		if !ok || strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("allow list line %d: reason is required", lineNo)
		}
		fields := strings.Fields(before)
		if len(fields) < 2 || (fields[0] != string(skip) && len(fields) < 3) {
			return nil, fmt.Errorf("allow list line %d: expected category path-glob matcher # reason", lineNo)
		}
		c := category(fields[0])
		if !isCategory(c) {
			return nil, fmt.Errorf("allow list line %d: unknown category %q", lineNo, fields[0])
		}
		pathGlob := fields[1]
		pathRE, err := compileGlob(pathGlob)
		if err != nil {
			return nil, fmt.Errorf("allow list line %d: bad path glob: %w", lineNo, err)
		}
		rule := allowRule{category: c, pathRE: pathRE, line: lineNo, skipOnly: c == skip}
		matcherText := ""
		if !rule.skipOnly {
			matcherText = strings.TrimSpace(strings.TrimPrefix(before, fields[0]))
			matcherText = strings.TrimSpace(strings.TrimPrefix(matcherText, pathGlob))
			if matcherText == "" {
				return nil, fmt.Errorf("allow list line %d: empty matcher", lineNo)
			}
		}
		if rule.skipOnly {
			if len(fields) != 2 {
				return nil, fmt.Errorf("allow list line %d: skip rules are path-only", lineNo)
			}
			rules = append(rules, rule)
			continue
		}
		if strings.HasPrefix(matcherText, "re:") {
			rule.matcher, err = regexp.Compile("^(?:" + strings.TrimPrefix(matcherText, "re:") + ")$")
			if err != nil {
				return nil, fmt.Errorf("allow list line %d: bad regex: %w", lineNo, err)
			}
		} else {
			rule.exact = matcherText
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func loadPrivate(path string) ([]detector, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private patterns: %s", privateReadError(err))
	}
	var out []detector
	patternNo := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		patternNo++
		r, err := regexp.Compile(line)
		if err != nil {
			return nil, fmt.Errorf("private pattern %d is invalid", patternNo)
		}
		if r.MatchString("") {
			return nil, fmt.Errorf("private pattern %d matches the empty string", patternNo)
		}
		out = append(out, detector{category: private, pattern: r, privateIndex: patternNo})
	}
	return out, nil
}

func privateReadError(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "not found"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	default:
		return "read error"
	}
}

func isCategory(c category) bool {
	for _, known := range categories {
		if c == known {
			return true
		}
	}
	return false
}

func compileGlob(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := strings.IndexByte(glob[i+1:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			j += i + 1
			class := glob[i+1 : j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteByte('[')
			b.WriteString(class)
			b.WriteByte(']')
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func gitFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	parts := bytes.Split(out, []byte{0})
	files := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			files = append(files, string(part))
		}
	}
	return files, nil
}

func scan(root string, files []string, detectors []detector, rules []allowRule) scanResult {
	result := scanResult{violations: make(map[category]int), allowed: make(map[category]int), tracked: len(files)}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		rootAbs = filepath.Clean(root)
	}
	if resolvedRoot, resolveErr := filepath.EvalSymlinks(rootAbs); resolveErr == nil {
		rootAbs = resolvedRoot
	}
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		skipReason := ""
		if err != nil {
			skipReason = "other"
		}
		var data []byte
		var targetData []byte
		if skipReason == "" && info.Mode()&os.ModeSymlink != 0 {
			linkText, readlinkErr := os.Readlink(path)
			if readlinkErr != nil {
				skipReason = "other"
			} else {
				data = []byte(linkText)
				if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
					resolvedAbs, absErr := filepath.Abs(resolved)
					if absErr == nil {
						relTarget, relErr := filepath.Rel(rootAbs, resolvedAbs)
						insideRoot := relErr == nil && relTarget != ".." && !strings.HasPrefix(relTarget, ".."+string(filepath.Separator))
						if insideRoot {
							targetInfo, targetErr := os.Lstat(resolvedAbs)
							if targetErr == nil && targetInfo.Mode().IsRegular() && targetInfo.Size() <= maxFileSize {
								if targetBytes, readErr := os.ReadFile(resolvedAbs); readErr == nil && len(targetBytes) <= maxFileSize {
									targetData = targetBytes
								}
							}
						}
					}
				}
			}
		} else if skipReason == "" {
			if !info.Mode().IsRegular() {
				skipReason = "other"
			}
			if skipReason == "" && info.Size() > maxFileSize {
				skipReason = "large"
			}
			if skipReason == "" {
				data, err = os.ReadFile(path)
				if err != nil {
					skipReason = "other"
				}
			}
			// The size check above closes the common case without reading a
			// large file; recheck after reading so a file that grows during the
			// scan is still treated according to the same limit.
			if skipReason == "" && len(data) > maxFileSize {
				skipReason = "large"
			}
		}
		if skipReason == "" && bytes.IndexByte(data, 0) >= 0 {
			skipReason = "binary"
		}
		skipAllowed := false
		if skipReason != "" {
			switch skipReason {
			case "binary":
				result.skippedBinary++
			case "large":
				result.skippedLarge++
			default:
				result.skippedOther++
			}
			for i := range rules {
				rule := &rules[i]
				if rule.skipOnly && rule.pathRE.MatchString(rel) {
					skipAllowed = true
					rule.useCount++
					break
				}
			}
			f := finding{path: rel, line: 1, match: skipReason, category: skip, reason: skipReason, allowed: skipAllowed}
			if f.allowed {
				result.allowed[skip]++
			} else {
				result.violations[skip]++
			}
			result.findings = append(result.findings, f)
		}
		scanData := func(scanBytes []byte) {
			for _, d := range detectors {
				if len(scanBytes) == 0 && skipReason != "binary" {
					continue
				}
				for _, loc := range d.pattern.FindAllIndex(scanBytes, -1) {
					if len(loc) != 2 {
						continue
					}
					match := string(scanBytes[loc[0]:loc[1]])
					if d.category == personalPath && isSharedPathMatch(match) {
						continue
					}
					f := finding{path: rel, line: 1 + bytes.Count(scanBytes[:loc[0]], []byte{'\n'}), start: loc[0], match: match, category: d.category}
					if d.category == private {
						f.privateIndex = d.privateIndex
					}
					for i := range rules {
						rule := &rules[i]
						if rule.skipOnly || rule.category != f.category || !rule.pathRE.MatchString(rel) {
							continue
						}
						if (rule.matcher != nil && rule.matcher.MatchString(match)) || (rule.matcher == nil && rule.exact == match) {
							f.allowed = true
							rule.useCount++
							break
						}
					}
					if f.allowed {
						result.allowed[f.category]++
					} else {
						result.violations[f.category]++
					}
					result.findings = append(result.findings, f)
				}
			}
		}
		scanData(data)
		if len(targetData) > 0 {
			scanData(targetData)
		}
	}
	sort.SliceStable(result.findings, func(i, j int) bool {
		a, b := result.findings[i], result.findings[j]
		if a.path != b.path {
			return a.path < b.path
		}
		if a.start != b.start {
			return a.start < b.start
		}
		return a.category < b.category
	})
	return result
}

func maskMatch(s string) string {
	if s == "" {
		return "…"
	}
	runes := []rune(s)
	if len(runes) > 4 {
		runes = runes[:4]
	}
	return string(runes) + "…"
}

func isSharedPathMatch(s string) bool {
	if strings.HasPrefix(s, "/Users/") {
		name := strings.TrimPrefix(s, "/Users/")
		return name == "Shared"
	}
	return false
}

func maskedFinding(f finding) string {
	if f.category == hosts {
		return "…ts.net"
	}
	return maskMatch(f.match)
}

func printResult(out io.Writer, result scanResult, rules []allowRule, strict bool) bool {
	for _, f := range result.findings {
		if f.allowed {
			continue
		}
		if f.category == private {
			fmt.Fprintf(out, "%s:%d: private: pattern #%d\n", f.path, f.line, f.privateIndex)
		} else if f.category == skip {
			fmt.Fprintf(out, "%s:%d: skip: %s\n", f.path, f.line, f.reason)
		} else if f.category == hosts {
			fmt.Fprintf(out, "%s:%d: hosts: …ts.net\n", f.path, f.line)
		} else {
			fmt.Fprintf(out, "%s:%d: %s: %s\n", f.path, f.line, f.category, maskedFinding(f))
		}
	}
	unused := 0
	for i, rule := range rules {
		if rule.useCount == 0 {
			unused++
			fmt.Fprintf(out, "warning: unused allow rule #%d (line %d, category=%s)\n", i+1, rule.line, rule.category)
		}
	}
	fmt.Fprintln(out, "category        violations  allowed")
	for _, c := range categories {
		fmt.Fprintf(out, "%-15s %10d  %7d\n", c, result.violations[c], result.allowed[c])
	}
	fmt.Fprintf(out, "skipped: binary=%d large=%d other=%d total=%d tracked=%d\n", result.skippedBinary, result.skippedLarge, result.skippedOther, result.skippedBinary+result.skippedLarge+result.skippedOther, result.tracked)
	if strict && unused > 0 {
		return false
	}
	for _, n := range result.violations {
		if n > 0 {
			return false
		}
	}
	return true
}

func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("leakscan", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := "."
	allowPath := "tools/leakscan/allow.txt"
	strict := false
	fs.StringVar(&root, "root", root, "repository root (default current directory)")
	fs.StringVar(&allowPath, "allow", allowPath, "allow-list path, relative to root unless absolute")
	fs.BoolVar(&strict, "strict-allow", false, "fail when an allow rule is unused")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(errOut, "leakscan: at most one repository root may be supplied")
		return 2
	}
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	if !filepath.IsAbs(allowPath) {
		allowPath = filepath.Join(root, allowPath)
	}
	rules, err := loadAllow(allowPath)
	if err != nil {
		fmt.Fprintln(errOut, "leakscan:", err)
		return 2
	}
	detectors := defaultDetectors()
	if privatePath := os.Getenv("LEAKSCAN_PRIVATE_PATTERNS"); privatePath != "" {
		privateDetectors, err := loadPrivate(privatePath)
		if err != nil {
			fmt.Fprintln(errOut, "leakscan:", err)
			return 2
		}
		detectors = append(detectors, privateDetectors...)
	}
	files, err := gitFiles(root)
	if err != nil {
		fmt.Fprintln(errOut, "leakscan:", err)
		return 2
	}
	result := scan(root, files, detectors, rules)
	if printResult(out, result, rules, strict) {
		return 0
	}
	return 1
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
