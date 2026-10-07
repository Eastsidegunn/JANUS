package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
)

func initRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "-C", root, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	return root
}

func trackedFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", name).CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, out)
	}
}

func writeAllow(t *testing.T, root, content string) string {
	t.Helper()
	path := filepath.Join(root, "allow.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func invoke(t *testing.T, root, allow string, strict bool) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	args := []string{"-root", root, "-allow", allow}
	if strict {
		args = append(args, "-strict-allow")
	}
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDetectsAllCategoriesAndMasksMatches(t *testing.T) {
	root := initRepo(t)
	aws := "AKIA" + strings.Repeat("A", 16)
	ant := "sk-ant-" + strings.Repeat("a", 10)
	generic := "sk-" + strings.Repeat("b", 32)
	gh := "ghp_" + strings.Repeat("c", 30)
	slack := "xoxb-" + strings.Repeat("d", 10)
	jwt := "eyJ" + strings.Repeat("e", 10) + ".eyJ" + strings.Repeat("f", 10) + ".signature"
	pem := "-----BEGIN " + "PRIVATE KEY-----"
	privateIPs := strings.Join([]string{
		"10" + ".0.0.1", "172" + ".16.0.2", "192.168" + ".1.3", "100.64" + ".0.4",
	}, " ")
	personalPaths := strings.Join([]string{
		"/Users/" + "alice" + "/project/", "/home/" + "bob" + "/work/", "C:\\Users\\" + "Carol" + "\\repo\\",
	}, " ")
	internalIDs := "q-" + "0123abcd" + " and https://example.invalid/actions/" + "runs/42"
	tailnet := "node" + ".ts.net"
	content := strings.Join([]string{
		aws, ant, generic, gh, slack,
		pem, jwt, privateIPs, personalPaths, internalIDs, tailnet,
	}, "\n")
	trackedFile(t, root, "sample.txt", content)
	allow := writeAllow(t, root, "")
	code, out, errOut := invoke(t, root, allow, false)
	if code != 1 {
		t.Fatalf("exit %d, want violation exit 1; stdout=%s stderr=%s", code, out, errOut)
	}
	for _, c := range []string{"secrets", "private_ip", "personal_path", "internal_id", "hosts"} {
		if !strings.Contains(out, c) {
			t.Fatalf("category %q absent from output: %s", c, out)
		}
	}
	if strings.Contains(out, aws) || strings.Contains(out, ant) || strings.Contains(out, jwt) {
		t.Fatalf("full match leaked in output: %s", out)
	}
	if !strings.Contains(out, "sample.txt:1: secrets: ") || !strings.Contains(out, "AKIA…") {
		t.Fatalf("masked location/match missing: %s", out)
	}
}

func TestStrengthenedDetectorsAndPathBoundaries(t *testing.T) {
	root := initRepo(t)
	githubPat := "github_pat_" + strings.Repeat("a", 20)
	ghOAuth := "gho_" + strings.Repeat("b", 20)
	pem := "-----BEGIN " + "RSA PRIVATE KEY BLOCK-----"
	paths := "/Users/" + "A_user-1" + " /Users/Shared /home/" + "worker_1" + " C:\\Users\\Worker-1"
	ids := "Run #:" + strings.Repeat("1", 9) + " job " + strings.Repeat("2", 9)
	content := strings.Join([]string{githubPat, ghOAuth, pem, paths, ids, "/Users/<placeholder> $HOME ~"}, "\n")
	trackedFile(t, root, "patterns.txt", content)
	allow := writeAllow(t, root, "")
	code, out, _ := invoke(t, root, allow, false)
	if code != 1 {
		t.Fatalf("strengthened detectors should find values, code=%d output=%s", code, out)
	}
	for _, want := range []string{"secrets", "personal_path", "internal_id"} {
		if !strings.Contains(out, want) {
			t.Fatalf("category %q missing: %s", want, out)
		}
	}
	if strings.Contains(out, "Shared") || strings.Contains(out, "placeholder") {
		t.Fatalf("excluded path values were reported: %s", out)
	}
}

func TestHostPatternIsCaseInsensitiveAndExcludesLookalikes(t *testing.T) {
	root := initRepo(t)
	content := "TAILNET" + ".TS.NET and " + "node-7" + ".ts.net; notts.net.example"
	trackedFile(t, root, "hosts.txt", content)
	allow := writeAllow(t, root, "")
	code, out, errOut := invoke(t, root, allow, false)
	if code != 1 {
		t.Fatalf("exit %d, want host violation; stdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "hosts.txt:1: hosts: …ts.net") {
		t.Fatalf("masked host finding missing: %s", out)
	}
	if strings.Contains(out, "notts") {
		t.Fatalf("lookalike host was reported: %s", out)
	}
}

func TestAllowRulesAndUnusedStrictMode(t *testing.T) {
	root := initRepo(t)
	testIP := "10" + ".0.0.1"
	trackedFile(t, root, "fixture.txt", testIP+"\n")
	allow := writeAllow(t, root, "private_ip **/*.txt "+testIP+" # documented test address\n")
	if code, out, errOut := invoke(t, root, allow, false); code != 0 {
		t.Fatalf("allowed hit exit %d, stdout=%s stderr=%s", code, out, errOut)
	}
	unusedAllow := "private_ip **/*.txt " + "10" + ".0.0.2 # reserved for a future fixture\n"
	if err := os.WriteFile(allow, []byte(unusedAllow), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := invoke(t, root, allow, false)
	if code != 1 || !strings.Contains(out, "unused allow rule") {
		t.Fatalf("unused rule should warn and permit content result, code=%d output=%s", code, out)
	}
	code, out, _ = invoke(t, root, allow, true)
	if code != 1 || !strings.Contains(out, "unused allow rule") {
		t.Fatalf("strict unused rule should fail, code=%d output=%s", code, out)
	}
}

func TestUnusedAllowRuleOnlyWarnsUnlessStrict(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "clean.txt", "clean\n")
	allow := writeAllow(t, root, "private_ip **/*.txt "+"10"+".0.0.2 # reserved test address\n")
	code, out, errOut := invoke(t, root, allow, false)
	if code != 0 || errOut != "" || !strings.Contains(out, "unused allow rule") {
		t.Fatalf("non-strict unused rule should warn only, code=%d stdout=%s stderr=%s", code, out, errOut)
	}
	code, out, errOut = invoke(t, root, allow, true)
	if code != 1 || errOut != "" || !strings.Contains(out, "unused allow rule") {
		t.Fatalf("strict unused rule should fail, code=%d stdout=%s stderr=%s", code, out, errOut)
	}
}

func TestPersonalPathPlaceholdersAreIgnored(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "paths.txt", "/Users/<placeholder>/ $HOME/ /home/<placeholder>/")
	allow := writeAllow(t, root, "")
	code, out, errOut := invoke(t, root, allow, false)
	if code != 0 || errOut != "" || !strings.Contains(out, "personal_path            0") {
		t.Fatalf("placeholder paths should be ignored, code=%d stdout=%s stderr=%s", code, out, errOut)
	}
}

func TestAllowRuleRequiresReason(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "clean.txt", "clean\n")
	allow := writeAllow(t, root, "secrets **/*.txt re:foo\n")
	code, _, errOut := invoke(t, root, allow, false)
	if code != 2 || !strings.Contains(errOut, "reason is required") {
		t.Fatalf("bad allow list exit=%d stderr=%s", code, errOut)
	}
}

func TestPrivatePatternsAreMaskedAndNumbered(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "private.txt", "operator-super-12345\n")
	allow := writeAllow(t, root, "")
	privatePath := filepath.Join(t.TempDir(), "private-patterns.txt")
	if err := os.WriteFile(privatePath, []byte("super-[0-9]+\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEAKSCAN_PRIVATE_PATTERNS", privatePath)
	code, out, _ := invoke(t, root, allow, false)
	if code != 1 || !strings.Contains(out, "private.txt:1: private: pattern #1") {
		t.Fatalf("private hit output=%s code=%d", out, code)
	}
	if strings.Contains(out, "super-12345") || strings.Contains(out, "super-") {
		t.Fatalf("private match leaked: %s", out)
	}
}

func TestSkippedFilesFailClosedAndBinaryDetectorsStillRun(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "clean.txt", "clean\n")
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("AKIA"+strings.Repeat("A", 16)), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := append([]byte{'x', 0, 'A'}, []byte("AKIA"+strings.Repeat("A", 16))...)
	trackedFile(t, root, "binary.dat", string(binary))
	large := bytes.Repeat([]byte{'x'}, maxFileSize+1)
	copy(large, []byte("AKIA"+strings.Repeat("A", 16)))
	path := filepath.Join(root, "large.dat")
	if err := os.WriteFile(path, large, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", "binary.dat", "large.dat").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, out)
	}
	allow := writeAllow(t, root, "")
	code, out, _ := invoke(t, root, allow, false)
	if code != 1 {
		t.Fatalf("skipped files should fail closed, code=%d output=%s", code, out)
	}
	if !strings.Contains(out, "skipped: binary=1 large=1") {
		t.Fatalf("skip counts absent: %s", out)
	}
	if !strings.Contains(out, "binary.dat:1: skip: binary") || !strings.Contains(out, "binary.dat:1: secrets: AKIA…") {
		t.Fatalf("binary skip or detector finding absent: %s", out)
	}
	if strings.Contains(out, "untracked.txt") {
		t.Fatalf("untracked file was scanned: %s", out)
	}
}

func TestSkipAllowRuleOnlyCoversSkipFinding(t *testing.T) {
	root := initRepo(t)
	binary := append([]byte{'x', 0}, []byte("AKIA"+strings.Repeat("A", 16))...)
	trackedFile(t, root, "binary.dat", string(binary))
	allow := writeAllow(t, root, "skip binary.dat # documented binary fixture\n")
	code, out, errOut := invoke(t, root, allow, true)
	if code != 1 || errOut != "" {
		t.Fatalf("skip allow must not suppress content findings, code=%d stdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "skip                     0") || !strings.Contains(out, "allowed") {
		t.Fatalf("skip allowance summary missing: %s", out)
	}
	if !strings.Contains(out, "binary.dat:1: secrets: AKIA…") {
		t.Fatalf("secret in skip-allowed binary was suppressed: %s", out)
	}
}

func TestDeletedTrackedFileIsViolation(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "gone.txt", "clean\n")
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	allow := writeAllow(t, root, "")
	code, out, _ := invoke(t, root, allow, false)
	if code != 1 || !strings.Contains(out, "gone.txt:1: skip: other") {
		t.Fatalf("deleted tracked file should fail closed, code=%d output=%s", code, out)
	}
}

func TestSymlinkTargetIsScanned(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "target.txt", "AKIA"+strings.Repeat("A", 16))
	if err := os.Symlink("target.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", "link.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, out)
	}
	allow := writeAllow(t, root, "")
	code, out, _ := invoke(t, root, allow, false)
	if code != 1 || !strings.Contains(out, "link.txt:1: secrets: AKIA…") {
		t.Fatalf("symlink target was not scanned, code=%d output=%s", code, out)
	}
}

func TestSymlinkOutsideRootScansLinkTextOnly(t *testing.T) {
	root := initRepo(t)
	personalTarget := "/Users/" + "alice" + "/private/project"
	if err := os.Symlink(personalTarget, filepath.Join(root, "personal-link.txt")); err != nil {
		t.Fatal(err)
	}
	outsideRoot := t.TempDir()
	outsideTarget := filepath.Join(outsideRoot, "target.txt")
	secret := "AKIA" + strings.Repeat("A", 16)
	if err := os.WriteFile(outsideTarget, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideTarget, filepath.Join(root, "outside-link.txt")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", "personal-link.txt", "outside-link.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, out)
	}
	allow := writeAllow(t, root, "")
	code, out, errOut := invoke(t, root, allow, false)
	if code != 1 || errOut != "" || !strings.Contains(out, "personal-link.txt:1: personal_path") {
		t.Fatalf("absolute personal path in tracked symlink was not detected, code=%d stdout=%s stderr=%s", code, out, errOut)
	}
	if strings.Contains(out, "outside-link.txt:1: secrets") {
		t.Fatalf("outside symlink target content was read: %s", out)
	}
}

func TestPrivatePatternsIgnoreCommentsAndNumberPatterns(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "private.txt", "super-12345\n")
	allow := writeAllow(t, root, "")
	privatePath := filepath.Join(t.TempDir(), "private-patterns.txt")
	if err := os.WriteFile(privatePath, []byte("# comment\nx+\nsuper-[0-9]+\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEAKSCAN_PRIVATE_PATTERNS", privatePath)
	code, out, _ := invoke(t, root, allow, false)
	if code != 1 || !strings.Contains(out, "private.txt:1: private: pattern #2") {
		t.Fatalf("comment/pattern numbering failed, code=%d output=%s", code, out)
	}
	if strings.Contains(out, "super-") {
		t.Fatalf("private match leaked: %s", out)
	}
}

func TestPrivatePatternEmptyMatchIsError(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "clean.txt", "clean\n")
	allow := writeAllow(t, root, "")
	privatePath := filepath.Join(t.TempDir(), "private-patterns.txt")
	if err := os.WriteFile(privatePath, []byte("x*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEAKSCAN_PRIVATE_PATTERNS", privatePath)
	code, _, errOut := invoke(t, root, allow, false)
	if code != 2 || !strings.Contains(errOut, "private pattern 1 matches the empty string") {
		t.Fatalf("empty private pattern should be a configuration error, code=%d stderr=%s", code, errOut)
	}
}

func TestPrivatePatternReadErrorOmitsPath(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "clean.txt", "clean\n")
	allow := writeAllow(t, root, "")
	missing := filepath.Join(t.TempDir(), "sensitive-pattern-file")
	t.Setenv("LEAKSCAN_PRIVATE_PATTERNS", missing)
	code, _, errOut := invoke(t, root, allow, false)
	if code != 2 || !strings.Contains(errOut, "read private patterns:") || strings.Contains(errOut, missing) {
		t.Fatalf("private read error leaked path or wrong exit: code=%d stderr=%s", code, errOut)
	}
}

func TestPrivateRegexErrorOmitsPattern(t *testing.T) {
	root := initRepo(t)
	trackedFile(t, root, "clean.txt", "clean\n")
	allow := writeAllow(t, root, "")
	privatePath := filepath.Join(t.TempDir(), "private-patterns.txt")
	bad := "([sensitive-private-regex"
	if err := os.WriteFile(privatePath, []byte(bad+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEAKSCAN_PRIVATE_PATTERNS", privatePath)
	code, _, errOut := invoke(t, root, allow, false)
	if code != 2 || strings.Contains(errOut, bad) {
		t.Fatalf("private regex error exposed pattern or wrong exit: code=%d stderr=%s", code, errOut)
	}
}

func TestPathGlobAndRegexAllowBoundaries(t *testing.T) {
	root := initRepo(t)
	ip1 := "10" + ".0.0.1"
	ip2 := "10" + ".0.0.2"
	trackedFile(t, root, "one.txt", ip1+"\n")
	trackedFile(t, root, "nested/two.txt", ip2+"\n")
	allow := writeAllow(t, root, "private_ip *.txt re:^10[.]0[.]0[.]1$ # one file only\n")
	code, out, _ := invoke(t, root, allow, false)
	if code != 1 || !strings.Contains(out, "nested/two.txt:1: private_ip") {
		t.Fatalf("single-star or anchored regex boundary broken, code=%d output=%s", code, out)
	}
	if strings.Contains(out, ip1) {
		t.Fatalf("full private match leaked: %s", out)
	}
	allow = writeAllow(t, root, "private_ip **/*.txt re:^10[.]0[.]0[.][12]$ # recursive test files\n")
	code, out, _ = invoke(t, root, allow, true)
	if code != 0 || strings.Contains(out, "unused allow rule") {
		t.Fatalf("recursive glob/regex rule failed, code=%d output=%s", code, out)
	}
}

func TestEnhancedSecretAndInternalDetectors(t *testing.T) {
	root := initRepo(t)
	sk := "sk-" + "proj_" + strings.Repeat("a", 20)
	pat := "github_pat_" + strings.Repeat("b", 20)
	gho := "gho_" + strings.Repeat("c", 20)
	pem := "-----BEGIN " + "RSA PRIVATE KEY BLOCK-----"
	gate := "q-" + "abcdef"
	run := "run " + strings.Repeat("1", 9)
	job := "JOB#" + strings.Repeat("2", 9)
	runID := "run_id=" + strings.Repeat("3", 9)
	jobID := "job_id:" + strings.Repeat("4", 9)
	content := strings.Join([]string{sk, pat, gho, pem, gate, run, job, runID, jobID}, "\n")
	trackedFile(t, root, "patterns.txt", content)
	allow := writeAllow(t, root, "")
	code, out, _ := invoke(t, root, allow, false)
	if code != 1 {
		t.Fatalf("enhanced detectors did not report, code=%d output=%s", code, out)
	}
	if strings.Count(out, "patterns.txt:") < 9 || !strings.Contains(out, "secrets") || !strings.Contains(out, "internal_id") {
		t.Fatalf("enhanced detector categories missing: %s", out)
	}
	if strings.Contains(out, sk) || strings.Contains(out, pat) || strings.Contains(out, gho) {
		t.Fatalf("secret value leaked in output: %s", out)
	}
}

func TestPersonalPathBoundariesAndSharedExclusion(t *testing.T) {
	root := initRepo(t)
	mac := "/Users/" + "User.Name" + "/work"
	shared := "/Users/Shared/docs"
	linux := "/home/" + "operator" + "/repo"
	windows := "C:\\Users\\" + "Operator" + "\\repo"
	placeholders := "/Users/<placeholder> $HOME ~ /home/<placeholder>"
	trackedFile(t, root, "paths.txt", strings.Join([]string{mac, shared, linux, windows, placeholders}, "\n"))
	allow := writeAllow(t, root, "")
	code, out, _ := invoke(t, root, allow, false)
	if code != 1 || strings.Count(out, "personal_path") < 4 {
		t.Fatalf("personal path boundary handling failed, code=%d output=%s", code, out)
	}
	if strings.Contains(out, "Shared") || strings.Contains(out, "placeholder") {
		t.Fatalf("shared or placeholder path was reported: %s", out)
	}
}

func TestPropertyMaskKeepsOnlyFourRunes(t *testing.T) {
	property := func(s string) bool {
		runes := []rune(s)
		if len(runes) <= 4 {
			return maskMatch(s) == string(runes)+"…"
		}
		masked := []rune(maskMatch(s))
		if len(masked) != 5 || masked[4] != '…' {
			return false
		}
		for i := 0; i < 4; i++ {
			if masked[i] != runes[i] {
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPropertySingleStarNeverCrossesDirectory(t *testing.T) {
	glob, err := compileGlob("*.txt")
	if err != nil {
		t.Fatal(err)
	}
	property := func(name string) bool {
		if strings.ContainsAny(name, "/\x00") {
			return true
		}
		return !glob.MatchString("nested/" + name + ".txt")
	}
	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}
