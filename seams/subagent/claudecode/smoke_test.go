//go:build smoke

// 사람 smoke ([H], 실 자격증명 필요) — 제안서 §7.4의 6개 확인점을 기계적으로
// 실행하고 결과를 표로 남긴다.
//
//	go test -tags smoke -count=1 -v -timeout 10m ./seams/subagent/claudecode
//
// CI에는 절대 들어가지 않는다(빌드 태그로 격리). `make lint`가
// `go vet -tags smoke`로 컴파일만 확인해 코드 부패를 막는다.
//
// 중지 조건: C안(--setting-sources project,local + 인라인 훅)이 실패하면
// API key로 우회하지 않는다(3차 리뷰 미승인). 정지하고 재제안한다.
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
)

const (
	smokeMarkerFile = "smoke-approved.txt"
	smokeTimeout    = 5 * time.Minute
)

// TestSmokeApprovalHandshake는 실 Claude 세션으로 승인 handshake를 검증한다.
// deny 실행과 allow 실행을 각각 돌려 툴 실행 여부를 파일로 확인한다.
func TestSmokeApprovalHandshake(t *testing.T) {
	claudeBin := preflight(t)
	bins := buildAdapterBinaries(t)
	reportEnvironment(t)

	for _, tc := range []struct {
		name        string
		decision    gen.ApprovalResponsePayloadDecision
		wantCreated bool
	}{
		{"deny", gen.ApprovalResponsePayloadDecisionDeny, false},
		{"allow", gen.ApprovalResponsePayloadDecisionAllow, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runSmoke(t, claudeBin, bins, tc.decision)
			marker := filepath.Join(run.workspace, smokeMarkerFile)
			_, statErr := os.Stat(marker)
			created := statErr == nil

			t.Logf("확인점 2 (approval_request 방출): %v", run.sawApprovalRequest)
			t.Logf("확인점 3·4 (%s 응답 후 툴 실행): created=%v", tc.decision, created)
			t.Logf("이벤트 순서: %s", strings.Join(run.kinds, " → "))
			t.Logf("done: %s", run.done)

			if !run.sawApprovalRequest {
				t.Fatalf("확인점 2 실패: approval_request가 방출되지 않음 — 훅이 발화하지 않았다(격리·플래그 조합 재검토, API key 우회 금지)\nstderr:\n%s", run.stderr)
			}
			if created != tc.wantCreated {
				t.Fatalf("확인점 실패: %s인데 파일 생성=%v (기대 %v)\nstderr:\n%s", tc.decision, created, tc.wantCreated, run.stderr)
			}
		})
	}
}

// preflight는 실행 전제를 확인한다. 실패는 skip이 아니라 fatal이다 —
// smoke를 돌렸는데 조용히 건너뛰면 "확인했다"는 착각이 남는다.
func preflight(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("HX_CLAUDE_BIN")
	if bin == "" {
		bin = defaultClaudeExecutable
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		t.Fatalf("claude 실행 파일을 찾지 못함(%q): %v — HX_CLAUDE_BIN으로 지정하라", bin, err)
	}
	out, err := exec.Command(resolved, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("claude --version 실패: %v\n%s", err, out)
	}
	t.Logf("claude 버전: %s", strings.TrimSpace(string(out)))
	return resolved
}

// reportEnvironment는 확인점 5와, 이 머신의 사용자 설정 현황을 기록한다.
// 확인점 1의 증명 자체는 TestSmokeUserSettingIsolation이 맡는다 —
// 개인 설정에 훅이 있든 없든 그 테스트는 결론을 낼 수 있다.
func reportEnvironment(t *testing.T) {
	t.Helper()
	home, _ := os.UserHomeDir()
	userSettings := filepath.Join(home, ".claude", "settings.json")
	switch data, err := os.ReadFile(userSettings); {
	case err != nil:
		t.Logf("확인점 1 (사용자 설정): %s 없음 (참고) — 격리 증명은 TestSmokeUserSettingIsolation이 한다", userSettings)
	case strings.Contains(string(data), `"hooks"`):
		t.Logf("확인점 1 (사용자 설정): %s에 hooks 있음 (참고) — 격리 증명은 TestSmokeUserSettingIsolation이 한다", userSettings)
	default:
		t.Logf("확인점 1 (사용자 설정): %s에 hooks 없음 (참고) — 격리 증명은 TestSmokeUserSettingIsolation이 한다", userSettings)
	}
	// 확인점 5: managed policy는 --setting-sources 통제 밖일 수 있다.
	for _, p := range managedPolicyPaths() {
		if _, err := os.Stat(p); err == nil {
			t.Logf("확인점 5 (managed policy): %s 존재 — 격리 가정이 깨질 수 있다. 재제안 판단 근거로 PR에 기록하라", p)
		} else {
			t.Logf("확인점 5 (managed policy): %s 없음", p)
		}
	}
}

func managedPolicyPaths() []string {
	return []string{
		"/Library/Application Support/ClaudeCode/managed-settings.json",
		"/etc/claude-code/managed-settings.json",
	}
}

type smokeRun struct {
	workspace          string
	kinds              []string
	done               string
	sawApprovalRequest bool
	stderr             string
}

// runSmoke는 코어 역할을 대신한다: task를 보내고, approval_request가 오면
// 지정한 판정으로 approval_response를 돌려준 뒤 done까지 읽는다.
func runSmoke(t *testing.T, claudeBin string, bins adapterBinaries, decision gen.ApprovalResponsePayloadDecision) smokeRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), smokeTimeout)
	defer cancel()

	workspace := t.TempDir() // pristine — project/local 설정 없음
	cmd := exec.CommandContext(ctx, bins.adapter)
	cmd.Env = append(os.Environ(), "HX_CLAUDE_BIN="+claudeBin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write(smokeTaskLine(t, workspace)); err != nil {
		t.Fatal(err)
	}

	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	run := smokeRun{workspace: workspace}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if err := vals.ValidateEvent(line); err != nil {
			t.Fatalf("어댑터가 §5.2 위반 이벤트를 냄: %v\n%s", err, line)
		}
		var ev struct {
			Kind    gen.EventKind   `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatal(err)
		}
		run.kinds = append(run.kinds, string(ev.Kind))
		switch ev.Kind {
		case gen.EventKindSubagentApprovalRequest:
			run.sawApprovalRequest = true
			var req gen.ApprovalRequestPayload
			if err := json.Unmarshal(ev.Payload, &req); err != nil {
				t.Fatal(err)
			}
			t.Logf("approval_request: tool=%s call_id=%s args=%s", req.Name, req.CallID, req.Args)
			if _, err := stdin.Write(approvalResponseLine(t, req.RequestID, decision)); err != nil {
				t.Fatal(err)
			}
		case gen.EventKindSubagentDone:
			run.done = string(ev.Payload)
		}
	}
	stdin.Close()
	waitErr := cmd.Wait()
	run.stderr = stderr.String()
	if run.done == "" {
		t.Fatalf("done 없이 종료 (err=%v)\nstderr:\n%s", waitErr, run.stderr)
	}
	return run
}

func smokeTaskLine(t *testing.T, workspace string) []byte {
	t.Helper()
	instruction := fmt.Sprintf(
		"Write 도구를 정확히 한 번 사용해 현재 디렉토리에 %s 파일을 만들고 내용은 approved 로 해라. 다른 도구는 쓰지 마라.",
		smokeMarkerFile)
	payload, err := json.Marshal(gen.TaskPayload{
		Instruction: instruction, Workspace: workspace,
		Budget: gen.Budget{Tokens: 200000, TimeMs: 300000, MaxDepth: 1}, Depth: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(gen.Command{V: 1, Cmd: gen.CommandCmdTask, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

const smokeSessionStartMarker = "session-start-fired.txt"

// TestSmokeUserSettingIsolation은 확인점 1을 증명한다: 고정 플래그
// --setting-sources project,local이 사용자 설정을 실제로 배제하는가.
//
// 개인 ~/.claude은 건드리지 않는다. CLAUDE_CONFIG_DIR로 사용자 설정
// 디렉터리를 임시 경로로 옮기고 거기에 마커 훅을 심는다.
//
// SessionStart 훅을 쓴다. PreToolUse가 아니다 — 세션 시작 시점은 첫 API 호출
// 전이라 인증 없이도 발화한다. 임시 config 디렉터리에서는 인증이 깨지지만
// (2026-08-19 실측: authentication_failed, "Not logged in"), 설정 로딩은 그보다
// 먼저 끝나므로 판정에 영향이 없다. 결과적으로 이 테스트는 자격증명을 쓰지
// 않고 API 호출도 하지 않는다.
//
// 두 실행이 필요하다. 대조군 없이 "마커가 없다"만 보는 것은 증명이 아니다 —
// 훅이 잘못 배선됐거나 CLAUDE_CONFIG_DIR이 무시돼도 똑같이 마커가 없고,
// 그러면 격리를 증명한 게 아니라 아무것도 증명하지 못한 것이다.
//
//	A(대조군) user,project,local → 마커가 있어야 한다 (기법 자체가 유효한가)
//	B(실제)   어댑터 그대로       → 마커가 없어야 한다 (격리가 성립하는가)
//
// B는 어댑터를 실제로 띄운다. 상수를 직접 읽어 claude를 부르면, 누군가
// argv 조립부에 다른 값을 박아 넣어도 테스트가 통과하는 구멍이 생긴다.
func TestSmokeUserSettingIsolation(t *testing.T) {
	claudeBin := preflight(t)
	bins := buildAdapterBinaries(t)

	control := newUserHookConfig(t)
	runControlSession(t, claudeBin, control, "user,project,local")
	if _, err := os.Stat(control.marker); err != nil {
		t.Fatalf("대조군 실패: user 소스를 포함했는데도 사용자 훅이 발화하지 않았다 (%v).\n"+
			"기법이 성립하지 않으므로 아래 격리 실행은 어떤 결과가 나오든 증거가 되지\n"+
			"못한다. 위 대조군 출력에서 원인을 확인하고, 못 잡으면 확인점 1은 미증명으로\n"+
			"두고 docs/t9-smoke-runbook.md §5.2의 개인 설정 절차로 대체하라.", err)
	}
	t.Logf("대조군 통과: user 소스 포함 시 사용자 훅 발화 — 기법이 유효하다")

	iso := newUserHookConfig(t)
	t.Setenv("CLAUDE_CONFIG_DIR", iso.dir) // 어댑터가 os.Environ()으로 전파한다
	sawReady := runAdapterSession(t, claudeBin, bins, iso.workspace)
	// 세션이 시작조차 못 했다면 마커 부재는 격리의 증거가 아니다.
	if !sawReady {
		t.Fatal("어댑터가 ready를 내지 못했다 — 세션이 시작되지 않았으므로 격리를 판정할 수 없다")
	}
	if _, err := os.Stat(iso.marker); err == nil {
		t.Fatalf("확인점 1 실패: --setting-sources project,local인데 사용자 훅이 발화했다 (%s 존재).\n"+
			"격리 가정이 깨진다 — 우회하지 말고 정지·재제안하라.", iso.marker)
	}
	t.Logf("확인점 1 통과: 사용자 훅 미발화(%s 부재), 세션은 정상 시작", smokeSessionStartMarker)
}

// userHookConfig는 임시 사용자 설정 디렉터리 한 벌이다.
type userHookConfig struct {
	dir       string // CLAUDE_CONFIG_DIR
	workspace string
	marker    string
}

func newUserHookConfig(t *testing.T) userHookConfig {
	t.Helper()
	c := userHookConfig{dir: t.TempDir(), workspace: t.TempDir()}
	c.marker = filepath.Join(c.dir, smokeSessionStartMarker)

	writeJSONFile(t, filepath.Join(c.dir, "settings.json"), map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": fmt.Sprintf("touch %q", c.marker),
					"timeout": 30,
				}},
			}},
		},
	})

	// 신규 config 디렉터리는 온보딩·작업공간 신뢰 상태가 비어 있다. 그대로
	// 두면 세션이 그 게이트에서 멈춘다. 최소 플래그만 심는다 — 개인 설정을
	// 복사하지 않는다. macOS의 t.TempDir()는 /var/folders/…를 주지만 claude는
	// 심링크를 푼 /private/var/folders/…를 프로젝트 키로 쓰므로 둘 다 넣는다.
	entry := map[string]any{"hasTrustDialogAccepted": true, "projectOnboardingSeenCount": 1}
	projects := map[string]any{c.workspace: entry}
	if resolved, err := filepath.EvalSymlinks(c.workspace); err == nil && resolved != c.workspace {
		projects[resolved] = entry
	}
	writeJSONFile(t, filepath.Join(c.dir, ".claude.json"), map[string]any{
		"hasCompletedOnboarding": true,
		"projects":               projects,
	})
	return c
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// runControlSession은 claude를 직접 띄운다. 어댑터를 거치지 않는다 —
// 어댑터의 고정 플래그를 테스트용으로 흔들지 않기 위해서다. 판정은 종료
// 코드가 아니라 마커 파일이다(인증 실패로 비정상 종료가 정상이다).
func runControlSession(t *testing.T, claudeBin string, c userHookConfig, sources string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), smokeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, claudeBin,
		"-p", "hi",
		"--output-format", "stream-json", "--verbose",
		"--no-session-persistence",
		"--setting-sources", sources,
	)
	cmd.Dir = c.workspace
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+c.dir)
	out, err := cmd.CombinedOutput() // Stdin nil = 즉시 EOF

	// 출력 전문을 남긴다. 앞부분만 로그에 찍으면 정작 오류가 있는 뒷부분을
	// 잃는다 — 1차 시도에서 실제로 진단을 놓쳤다.
	logPath := filepath.Join(t.TempDir(), "control-output.txt")
	if writeErr := os.WriteFile(logPath, out, 0o600); writeErr != nil {
		t.Logf("대조군 출력 저장 실패: %v", writeErr)
	}
	t.Logf("대조군(sources=%s) 종료: err=%v (출력 전문: %s, %d bytes)", sources, err, logPath, len(out))
	const tail = 2000
	if len(out) > tail {
		t.Logf("대조군 출력 끝 %d바이트:\n%s", tail, out[len(out)-tail:])
	} else {
		t.Logf("대조군 출력:\n%s", out)
	}
}

// runAdapterSession은 어댑터를 실제 경로 그대로 띄우고 ready 관측 여부만
// 돌려준다. 임시 config에서는 인증이 깨져 done이 오류로 끝나지만, 설정 로딩과
// SessionStart 훅은 그 전에 이미 끝났으므로 격리 판정에는 영향이 없다.
func runAdapterSession(t *testing.T, claudeBin string, bins adapterBinaries, workspace string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), smokeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bins.adapter)
	cmd.Env = append(os.Environ(), "HX_CLAUDE_BIN="+claudeBin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write(smokeTaskLine(t, workspace)); err != nil {
		t.Fatal(err)
	}

	var kinds []string
	sawReady := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	for scanner.Scan() {
		var ev struct {
			Kind gen.EventKind `json:"kind"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("어댑터 출력 파싱 실패: %v\n%s", err, scanner.Bytes())
		}
		kinds = append(kinds, string(ev.Kind))
		if ev.Kind == gen.EventKindSubagentReady {
			sawReady = true
		}
	}
	stdin.Close()
	waitErr := cmd.Wait()
	t.Logf("격리 실행 이벤트: %s (err=%v)", strings.Join(kinds, " → "), waitErr)
	if s := stderr.String(); s != "" {
		t.Logf("격리 실행 stderr:\n%s", s)
	}
	return sawReady
}
