//go:build t15integration || t15smoke

package main

// T17 Linux 실물 게이트: runProduction이 실제 production world(rootless
// Podman)·승인된 어댑터(claudecode)·pin된 정책으로 조립되는지 검증한다.
// 자격증명 없는 실 Claude 이미지를 쓰므로 세션은 인증 실패(error)로
// 끝나야 하고, 그 전에 접수 binding·launch claim·world spawn 이벤트가
// durable해야 한다. 같은 key 재요청은 두 번째 컨테이너를 만들지 않는다.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/seams/accept"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

func TestProductionRunClaudeIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatalf("VERIFICATION: T17 Linux gate requires Linux; skip 금지 (현재 %s)", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	requirePodmanPreconditions(t, ctx)
	artifacts := buildIntegrationArtifacts(t, ctx)
	claudeRepo, claudeDigest := buildClaudeImage(t, ctx, artifacts.root)
	claudeAdapter := buildT15ClaudeAdapter(t, ctx, artifacts.root)
	lower, stateRoot := integrationPaths(t)

	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profile.yaml")
	profileYAML := fmt.Sprintf(`id: t17-claude
fs_scope:
  - %s
egress:
  - example.com
budget:
  tokens: 100000
  time_ms: 180000
  max_depth: 2
approval: manual
`, lower)
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	profileHash, err := pinnedProfileHash(profilePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := runRequest{
		Version: 1, OperationID: "op-t17-linux", Scope: "rhizome-ci/t17",
		IdempotencyKey: "t17-claude-1", RequestFingerprint: strings.Repeat("ab", 32),
		RhizomeExecutionID: "exec-t17", AdapterID: "claudecode",
		WorkspaceRef: lower, ProfileID: "t17-claude", ProfileHash: profileHash,
		TaskRef: runTaskRef{Instruction: "Respond with exactly OK."},
		Budget:  requestBudget{Tokens: 100000, TimeMs: 180000, MaxDepth: 2},
	}
	requestBytes, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := newWorldLauncher(worldConfig{
		StateRoot: stateRoot,
		ProxyImage: worldImageConfig{
			Repository: artifacts.proxyRepository, Digest: artifacts.proxyDigest, UID: 1000, GID: 1000,
		},
		Adapters: map[string]worldAdapterConfig{
			"claudecode": {
				Bin:       claudeAdapter,
				Image:     worldImageConfig{Repository: claudeRepo, Digest: claudeDigest, UID: 1000, GID: 1000},
				AgentArgv: []string{"claude"}, ControlMode: "tool_approval",
			},
		},
	})
	if err != nil {
		t.Fatalf("VERIFICATION: production backend 조립 실패: %v", err)
	}
	acceptRoot := filepath.Join(dir, "acceptance")
	var out bytes.Buffer
	runErr := runProduction(ctx, productionRun{
		RequestBytes: requestBytes, ProfilePath: profilePath,
		AcceptRoot: acceptRoot, Launcher: launcher, Stdout: &out,
	})
	msgs := decodeControls(t, out.String())
	if len(msgs) != 2 {
		t.Fatalf("VERIFICATION: 접수+terminal 두 메시지가 필요: %d\n%s", len(msgs), out.String())
	}
	acc := msgs[0]
	if acc.Status != "accepted" || acc.SessionRef == nil || acc.PolicyHash == "" {
		t.Fatalf("VERIFICATION: 접수 메시지 위반: %+v", acc)
	}
	terminal := msgs[1]
	// 자격증명이 없으므로 실행은 error로 끝나야 한다 — 접수 사실과 최종
	// 실패의 구분이 이 게이트의 요점이다(계약 §3.1).
	if runErr == nil {
		t.Fatal("VERIFICATION: tokenless Claude 실행이 성공으로 보고되면 안 됨")
	}
	if terminal.Status != "terminal" || terminal.Done == nil || terminal.Done.Status != "error" {
		t.Fatalf("VERIFICATION: terminal 메시지가 done status=error를 보고해야 함: %+v", terminal)
	}

	// durable 검증: 접수 binding + world spawn 이벤트 + session/end.
	log, err := sqlite.Open(ctx, acc.SessionRef.SessionDB)
	if err != nil {
		t.Fatal(err)
	}
	events, err := log.Reader.ReadFrom(ctx, 1)
	log.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Kind != gen.KindSessionStart {
		t.Fatalf("VERIFICATION: 첫 이벤트가 session/start여야 함")
	}
	var binding struct {
		B executionBinding `json:"execution_binding"`
	}
	if err := json.Unmarshal(events[0].Payload, &binding); err != nil || binding.B.PolicyHash != acc.PolicyHash {
		t.Fatalf("VERIFICATION: binding payload 불일치: %v %+v", err, binding.B)
	}
	childSpan := ""
	for _, event := range events {
		if event.Kind != gen.KindSubagentSpawn {
			continue
		}
		var payload gen.SubagentSpawnPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.WorldBackend == gen.SubagentSpawnPayloadWorldBackendLocalPodman {
			childSpan = event.SpanID
		}
	}
	if childSpan == "" {
		t.Fatal("VERIFICATION: local-podman world spawn 이벤트가 없음 — 샌드박스 없는 시작 경로 금지")
	}
	registry, err := accept.Open(acceptRoot)
	if err != nil {
		t.Fatal(err)
	}
	status, err := registry.Lookup(req.Scope, req.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "accepted" || !status.Launched {
		t.Fatalf("VERIFICATION: claim 상태 위반: %+v", status)
	}

	// 같은 key 재요청: 무spawn 중복 접수. 컨테이너·컨테이너 흔적이 새로
	// 생기지 않아야 한다.
	var dup bytes.Buffer
	if err := runProduction(ctx, productionRun{
		RequestBytes: requestBytes, ProfilePath: profilePath,
		AcceptRoot: acceptRoot, Launcher: launcher, Stdout: &dup,
	}); err != nil {
		t.Fatalf("VERIFICATION: 동일 key 재요청은 무spawn 성공이어야 함: %v\n%s", err, dup.String())
	}
	dupMsgs := decodeControls(t, dup.String())
	if len(dupMsgs) != 1 || dupMsgs[0].Status != "accepted" || !dupMsgs[0].Duplicate ||
		dupMsgs[0].SessionRef.TraceID != acc.SessionRef.TraceID {
		t.Fatalf("VERIFICATION: 중복 접수 응답 위반: %+v", dupMsgs)
	}
	assertNoRuntimeArtifacts(t, ctx, claudeRepo+"@"+claudeDigest, childSpan)
}
