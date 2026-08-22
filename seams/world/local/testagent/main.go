// testagent is a real container integration artifact. It speaks §5.2 directly
// and exercises filesystem, network, approval, backpressure, and lifecycle
// boundaries from inside a rootless Podman container. It is not a production
// agent and is never published to a registry.
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/world/approvalrelaywire"
)

const approvalSocketEnv = "HX_APPROVAL_SOCKET"

type scenario struct {
	Mode          string `json:"mode"`
	AllowURL      string `json:"allow_url"`
	ForbiddenURL  string `json:"forbidden_url"`
	DirectAddress string `json:"direct_address"`
	FloodCount    int    `json:"flood_count"`
	Secret        string `json:"secret"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--descendant" {
		time.Sleep(30 * time.Second)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "testagent:", err)
		os.Exit(1)
	}
}

func run() error {
	startedNS := time.Now().UnixNano()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	if !scanner.Scan() {
		return fmt.Errorf("task 없음: %v", scanner.Err())
	}
	var command gen.Command
	if err := json.Unmarshal(scanner.Bytes(), &command); err != nil || command.Cmd != gen.CommandCmdTask {
		return fmt.Errorf("첫 command가 task가 아님")
	}
	var task gen.TaskPayload
	if err := json.Unmarshal(command.Payload, &task); err != nil {
		return err
	}
	var cfg scenario
	if err := json.Unmarshal([]byte(task.Instruction), &cfg); err != nil {
		return fmt.Errorf("scenario decode: %w", err)
	}
	if err := emit(gen.EventKindSubagentReady, gen.ReadyPayload{
		Grade: gen.ReadyPayloadGradeObservable, Tools: []string{"filesystem", "network", "approval"},
	}); err != nil {
		return err
	}

	switch cfg.Mode {
	case "normal":
		return runNormal(task.Workspace, cfg, startedNS)
	case "abnormal":
		os.Exit(7)
	case "stop":
		select {}
	case "orphan":
		cmd := exec.Command(os.Args[0], "--descendant")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("미지 mode %q", cfg.Mode)
	}
	return nil
}

func runNormal(workspace string, cfg scenario, startedNS int64) error {
	if workspace != "/workspace" {
		return fmt.Errorf("workspace=%q", workspace)
	}
	if err := os.WriteFile(filepath.Join(workspace, "created.txt"), []byte("created\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workspace, "modified.txt"), []byte("modified\n"), 0o600); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(workspace, "deleted.txt")); err != nil {
		return err
	}
	exe, _ := os.Readlink("/proc/self/exe")
	if err := emitMessage(fmt.Sprintf("container-evidence pid=%d exe=%s started_ns=%d", os.Getpid(), exe, startedNS)); err != nil {
		return err
	}

	direct, err := net.DialTimeout("tcp", cfg.DirectAddress, 2*time.Second)
	if err == nil {
		direct.Close()
		return fmt.Errorf("direct external IP가 연결됨")
	}
	if err := emitMessage("direct-ip-denied"); err != nil {
		return err
	}

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}, Timeout: 10 * time.Second}
	forbidden, err := client.Get(cfg.ForbiddenURL)
	if err != nil {
		return fmt.Errorf("forbidden proxy 응답 없음: %w", err)
	}
	forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		return fmt.Errorf("forbidden status=%d", forbidden.StatusCode)
	}
	if err := emitMessage("forbidden-domain-denied"); err != nil {
		return err
	}

	request, err := http.NewRequest(http.MethodPost, cfg.AllowURL, strings.NewReader("body="+cfg.Secret))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.Secret)
	request.Header.Set("X-HX-Credential", cfg.Secret)
	allowed, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("allow proxy 요청 실패: %w", err)
	}
	allowed.Body.Close()
	if err := emitMessage("allowed-domain-status=" + strconv.Itoa(allowed.StatusCode)); err != nil {
		return err
	}
	httpsClient := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// The scratch integration image intentionally has no CA bundle. The
		// gate proves CONNECT routing/audit, not remote certificate policy.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- integration transport probe only
	}, Timeout: 10 * time.Second}
	httpsResponse, err := httpsClient.Get("https://example.com/")
	if err != nil {
		return fmt.Errorf("allow CONNECT 요청 실패: %w", err)
	}
	httpsResponse.Body.Close()
	if err := emitMessage("allowed-connect-status=" + strconv.Itoa(httpsResponse.StatusCode)); err != nil {
		return err
	}

	args := json.RawMessage(`{"path":"/workspace/approved-marker.txt"}`)
	if err := emit(gen.EventKindSubagentToolCall, gen.AgentToolCallPayload{
		CallID: "call-allow", Name: "Write", Args: args,
	}); err != nil {
		return err
	}
	raw, err := json.Marshal(approvalrelaywire.NativeInput{
		HookEventName: "PreToolUse", ToolUseID: "call-allow", ToolName: "Write", ToolInput: args,
	})
	if err != nil {
		return err
	}
	first, err := requestApproval(raw)
	if err != nil {
		return err
	}
	if first.Decision != "allow" {
		return fmt.Errorf("첫 approval=%s", first.Decision)
	}
	if err := os.WriteFile(filepath.Join(workspace, "approved-marker.txt"), []byte("allowed\n"), 0o600); err != nil {
		return err
	}
	if err := emit(gen.EventKindSubagentToolResult, gen.AgentToolResultPayload{
		CallID: "call-allow", Status: gen.AgentToolResultPayloadStatusOk,
		Output: json.RawMessage(`{"marker":true}`),
	}); err != nil {
		return err
	}

	duplicate, err := requestApproval(raw)
	if err != nil {
		return err
	}
	if duplicate.Decision != "deny" || duplicate.Reason == nil || *duplicate.Reason != "duplicate tool intent" {
		return fmt.Errorf("duplicate approval=%+v", duplicate)
	}
	if err := emit(gen.EventKindSubagentToolResult, gen.AgentToolResultPayload{
		CallID: "call-allow", Status: gen.AgentToolResultPayloadStatusRejected, Reason: duplicate.Reason,
	}); err != nil {
		return err
	}

	if err := emitMessage("flood-start"); err != nil {
		return err
	}
	for i := 0; i < cfg.FloodCount; i++ {
		if err := emitMessage(fmt.Sprintf("flood-%05d-%s", i, strings.Repeat("x", 2048))); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "flood-complete.txt"), []byte("complete\n"), 0o600); err != nil {
		return err
	}
	if err := emit(gen.EventKindSubagentUsage, gen.UsagePayload{InputTokens: 7, OutputTokens: 11}); err != nil {
		return err
	}
	return emit(gen.EventKindSubagentDone, gen.DonePayload{Status: gen.DonePayloadStatusOk, Result: "world integration complete"})
}

func requestApproval(raw []byte) (approvalrelaywire.Decision, error) {
	path := os.Getenv(approvalSocketEnv)
	if path == "" {
		return approvalrelaywire.Decision{}, fmt.Errorf("%s 없음", approvalSocketEnv)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return approvalrelaywire.Decision{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(approvalrelaywire.Request{Raw: raw}); err != nil {
		return approvalrelaywire.Decision{}, err
	}
	var decision approvalrelaywire.Decision
	if err := json.NewDecoder(conn).Decode(&decision); err != nil {
		return decision, err
	}
	if err := json.NewEncoder(conn).Encode(approvalrelaywire.Ack{Delivered: true}); err != nil {
		return decision, err
	}
	return decision, nil
}

func emitMessage(text string) error {
	return emit(gen.EventKindSubagentMessage, gen.AgentMessagePayload{Text: text})
}

func emit(kind gen.EventKind, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(gen.Event{V: 1, Kind: kind, Payload: encoded, Raw: ""})
}
