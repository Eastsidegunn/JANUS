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

// reportEnvironment는 확인점 1·5의 근거를 기록한다. 판단은 사람이 한다.
func reportEnvironment(t *testing.T) {
	t.Helper()
	home, _ := os.UserHomeDir()
	userSettings := filepath.Join(home, ".claude", "settings.json")
	switch data, err := os.ReadFile(userSettings); {
	case err != nil:
		t.Logf("확인점 1 (사용자 설정): %s 없음 — 사용자 훅이 없으므로 격리는 이번 실행으로 증명되지 않는다", userSettings)
	case strings.Contains(string(data), `"hooks"`):
		t.Logf("확인점 1 (사용자 설정): %s에 hooks 있음 — 아래 이벤트에 그 훅의 흔적이 없어야 격리 성립", userSettings)
	default:
		t.Logf("확인점 1 (사용자 설정): %s에 hooks 없음 — 격리는 이번 실행으로 증명되지 않는다", userSettings)
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

	workspace := t.TempDir() // pristine — .claude 없음(확인점 1)
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
