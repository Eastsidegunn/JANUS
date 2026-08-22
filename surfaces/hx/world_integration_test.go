//go:build worldintegration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
	localworld "github.com/Eastsidegunn/JANUS/seams/world/local"
)

const integrationFloodCount = 2048

type integrationArtifacts struct {
	root, adapter                string
	agentRepository, agentDigest string
	proxyRepository, proxyDigest string
}

type integrationStore struct {
	mu          sync.Mutex
	file        *os.File
	records     []gen.EventRecord
	committedAt map[int64]time.Time
	block       bool
	floodSeen   chan struct{}
	release     chan struct{}
	floodOnce   sync.Once
}

func newIntegrationStore(t *testing.T, path string, gate bool) *integrationStore {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	store := &integrationStore{file: file, committedAt: map[int64]time.Time{}}
	if gate {
		store.floodSeen, store.release = make(chan struct{}), make(chan struct{})
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func (s *integrationStore) LastSeq(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		return 0, nil
	}
	return s.records[len(s.records)-1].Seq, nil
}

func (s *integrationStore) Append(ctx context.Context, rec gen.EventRecord) error {
	s.mu.Lock()
	wait := s.block
	release := s.release
	s.mu.Unlock()
	if wait {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.appendDurable(rec)
}

func (s *integrationStore) AppendBatch(_ context.Context, records []gen.EventRecord) error {
	var batch []byte
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		batch = append(batch, encoded...)
		batch = append(batch, '\n')
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	offset, err := s.file.Seek(0, 1)
	if err != nil {
		return err
	}
	if _, err := s.file.Write(batch); err != nil {
		_ = s.file.Truncate(offset)
		return err
	}
	if err := s.file.Sync(); err != nil {
		_ = s.file.Truncate(offset)
		return err
	}
	committed := time.Now()
	for _, record := range records {
		s.records = append(s.records, record)
		s.committedAt[record.Seq] = committed
	}
	return nil
}

func (s *integrationStore) appendDurable(rec gen.EventRecord) error {
	encoded, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.file.Write(append(encoded, '\n')); err != nil {
		return err
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	s.records = append(s.records, rec)
	s.committedAt[rec.Seq] = time.Now()
	if s.floodSeen != nil && isMessage(rec, "flood-start") {
		s.block = true
		s.floodOnce.Do(func() { close(s.floodSeen) })
	}
	return nil
}

func (s *integrationStore) ReadFrom(_ context.Context, from int64) ([]gen.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []gen.EventRecord
	for _, record := range s.records {
		if record.Seq >= from {
			out = append(out, record)
		}
	}
	return out, nil
}

func (s *integrationStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func (s *integrationStore) snapshot() []gen.EventRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gen.EventRecord(nil), s.records...)
}

func (s *integrationStore) releaseFlood() { close(s.release) }

type allowOnceDecider struct {
	mu    sync.Mutex
	calls int
}

func (d *allowOnceDecider) Decide(context.Context, policy.ApprovalRequest) (policy.ApprovalDecision, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return policy.ApprovalDecision{Allow: true}, nil
}

func (d *allowOnceDecider) Calls() int { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }

func TestWorldIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatalf("world-integration은 Linux 실물 게이트다 (현재 %s); skip 금지", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	requirePodmanPreconditions(t, ctx)
	artifacts := buildIntegrationArtifacts(t, ctx)
	t.Run("surface-overlay-egress-approval-backpressure", func(t *testing.T) {
		runNormalIntegration(t, ctx, artifacts)
	})
	for _, mode := range []string{"abnormal", "stop", "orphan"} {
		t.Run("lifecycle-"+mode, func(t *testing.T) {
			runLifecycleIntegration(t, ctx, artifacts, mode)
		})
	}
}

func requirePodmanPreconditions(t *testing.T, ctx context.Context) {
	t.Helper()
	out := runOutput(t, ctx, "podman", "info", "--format", "json")
	var info struct {
		Host struct {
			Security struct {
				Rootless bool `json:"rootless"`
			} `json:"security"`
		} `json:"host"`
		Store struct {
			GraphDriverName string            `json:"graphDriverName"`
			GraphStatus     map[string]string `json:"graphStatus"`
		} `json:"store"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("podman info JSON: %v", err)
	}
	if !info.Host.Security.Rootless || info.Store.GraphDriverName != "overlay" || info.Store.GraphStatus["Native Overlay Diff"] != "true" {
		t.Fatalf("rootless/native-overlay 전제 불충족: rootless=%v driver=%q native=%q", info.Host.Security.Rootless, info.Store.GraphDriverName, info.Store.GraphStatus["Native Overlay Diff"])
	}
}

func buildIntegrationArtifacts(t *testing.T, ctx context.Context) integrationArtifacts {
	t.Helper()
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join(packageDir, "../.."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	build := func(name, pkg string) string {
		path := filepath.Join(dir, name)
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", path, pkg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, out)
		}
		return path
	}
	adapter := build("worldadapter", "./seams/subagent/worldadapter/cmd/worldadapter")
	agent := build("testagent", "./seams/world/local/testagent")
	proxy := build("hxegressproxy", "./seams/world/local/egressproxy/cmd/hxegressproxy")
	agentRepo, agentDigest := buildScratchImage(t, ctx, dir, "agent", agent, "/testagent", "localhost/hx-world-testagent")
	proxyRepo, proxyDigest := buildScratchImage(t, ctx, dir, "proxy", proxy, "/hxegressproxy", "localhost/hx-world-egressproxy")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, ref := range []string{agentRepo + "@" + agentDigest, proxyRepo + "@" + proxyDigest, agentRepo + ":integration", proxyRepo + ":integration"} {
			_ = exec.CommandContext(cleanup, "podman", "image", "rm", "--force", ref).Run()
		}
	})
	return integrationArtifacts{root: root, adapter: adapter, agentRepository: agentRepo, agentDigest: agentDigest, proxyRepository: proxyRepo, proxyDigest: proxyDigest}
}

func buildScratchImage(t *testing.T, ctx context.Context, dir, name, binary, destination, repository string) (string, string) {
	t.Helper()
	contextDir := filepath.Join(dir, name+"-image")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, filepath.Base(destination)), data, 0o755); err != nil {
		t.Fatal(err)
	}
	containerfile := fmt.Sprintf("FROM scratch\nCOPY %s %s\nUSER 1000:1000\nENTRYPOINT [\"%s\"]\n", filepath.Base(destination), destination, destination)
	if err := os.WriteFile(filepath.Join(contextDir, "Containerfile"), []byte(containerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	tag := repository + ":integration"
	runCommand(t, ctx, "podman", "build", "--pull=never", "-t", tag, "-f", filepath.Join(contextDir, "Containerfile"), contextDir)
	digest := strings.TrimSpace(string(runOutput(t, ctx, "podman", "image", "inspect", "--format", "{{.Digest}}", tag)))
	if len(digest) != 71 || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("image digest 형식 오류: %q", digest)
	}
	runCommand(t, ctx, "podman", "image", "inspect", repository+"@"+digest)
	return repository, digest
}

func runNormalIntegration(t *testing.T, parent context.Context, artifacts integrationArtifacts) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()
	lower, stateRoot := integrationPaths(t)
	mustWrite(t, filepath.Join(lower, "modified.txt"), "original-modified\n")
	mustWrite(t, filepath.Join(lower, "deleted.txt"), "original-deleted\n")
	mustWrite(t, filepath.Join(lower, "untouched.txt"), "original-untouched\n")
	store := newIntegrationStore(t, filepath.Join(t.TempDir(), "events.ndjson"), true)
	writer, err := logd.NewWriter(ctx, store, logd.WithQueueCap(1))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	traceID, parentSpan, childSpan := logd.NewTraceID(), logd.NewSpanID(), logd.NewSpanID()
	if err := writer.InitBatch(ctx, []gen.EventRecord{{Ts: time.Now().UnixMilli(), TraceID: traceID, SpanID: parentSpan, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	backend := newIntegrationBackend(t, stateRoot, artifacts)
	budget := gen.Budget{Tokens: 1_000_000, TimeMs: 240_000, MaxDepth: 2}
	effective := world.NewEffectivePolicy(policy.SandboxConfig{
		ProfileID: "world-integration", Workspace: lower, FSScope: []string{lower},
		Egress: []string{"example.com"}, Budget: budget, Approval: policy.ApprovalManual,
	})
	scenarioBytes, _ := json.Marshal(map[string]any{
		"mode": "normal", "allow_url": "http://example.com/", "forbidden_url": "http://forbidden.invalid/",
		"direct_address": "1.1.1.1:80", "flood_count": integrationFloodCount, "secret": "integration-secret-must-not-appear",
	})
	spawnSpec := world.NewSpawnSpec(effective, world.NewImageReference(artifacts.agentRepository, artifacts.agentDigest), []string{"integration"}, 0, traceID, childSpan, world.AgentIdentity{UID: 1000, GID: 1000}, nil)
	decider := &allowOnceDecider{}
	adapterHashBefore := fileSHA256(t, artifacts.adapter)
	active, err := startProductionWorld(ctx, worldLaunch{
		Backend: backend, SpawnSpec: spawnSpec, Writer: writer, TraceID: traceID, ParentSpan: parentSpan,
		AdapterCommand: []string{artifacts.adapter}, AdapterName: "world-testagent",
		AdapterStderr: os.Stderr,
		Instruction:   string(scenarioBytes), Workspace: "/workspace", Budget: budget, Depth: 0,
		ProfileID: "world-integration", Approval: subagent.Spec{Approval: policy.ApprovalManual, Decider: decider},
	})
	if err != nil {
		t.Fatal(err)
	}
	var effectsMu sync.Mutex
	var effects []world.EffectAttempt
	effectsDone := make(chan struct{})
	go func() {
		defer close(effectsDone)
		for effect := range active.Lease.Effects() {
			effectsMu.Lock()
			effects = append(effects, effect)
			effectsMu.Unlock()
		}
	}()

	waitSignal(t, store.floodSeen, 45*time.Second, "flood-start durable event")
	agentCID := findAgentContainer(t, ctx, artifacts.agentRepository+"@"+artifacts.agentDigest)
	assertContainerIsolation(t, ctx, agentCID, artifacts.adapter, active.Lease)
	if _, err := os.Stat(filepath.Join(active.Lease.UpperDir(), "flood-complete.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writer gate가 닫혔는데 helper flood가 완료됨: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(active.Lease.UpperDir(), "flood-complete.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backpressure가 container write까지 전파되지 않음: %v", err)
	}
	store.releaseFlood()
	waitCtx, waitCancel := context.WithTimeout(ctx, 90*time.Second)
	done, err := active.Subagent.Wait(waitCtx)
	waitCancel()
	if err != nil || done.Status != gen.DonePayloadStatusOk {
		t.Fatalf("normal subagent: done=%+v err=%v", done, err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := active.Lease.Close(closeCtx); err != nil {
		closeCancel()
		t.Fatal(err)
	}
	closeCancel()
	waitSignal(t, effectsDone, 2*time.Second, "effect drain")
	if got := fileSHA256(t, artifacts.adapter); got != adapterHashBefore {
		t.Fatalf("host adapter binary가 변조됨: before=%s after=%s", adapterHashBefore, got)
	}
	assertLowerAndUpper(t, lower, active.Lease.UpperDir())
	records := store.snapshot()
	assertSpawnBeforeStart(t, records, store)
	assertApprovalAndFlood(t, records, decider)
	effectsMu.Lock()
	gotEffects := append([]world.EffectAttempt(nil), effects...)
	effectsMu.Unlock()
	assertEgressEffects(t, gotEffects)
	assertNoContainers(t, ctx, artifacts.agentRepository+"@"+artifacts.agentDigest)
}

func runLifecycleIntegration(t *testing.T, parent context.Context, artifacts integrationArtifacts, mode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	lower, stateRoot := integrationPaths(t)
	store := newIntegrationStore(t, filepath.Join(t.TempDir(), "events.ndjson"), false)
	writer, err := logd.NewWriter(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	traceID, parentSpan, childSpan := logd.NewTraceID(), logd.NewSpanID(), logd.NewSpanID()
	_ = writer.InitBatch(ctx, []gen.EventRecord{{Ts: time.Now().UnixMilli(), TraceID: traceID, SpanID: parentSpan, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`)}})
	budget := gen.Budget{Tokens: 1000, TimeMs: 60_000, MaxDepth: 2}
	effective := world.NewEffectivePolicy(policy.SandboxConfig{ProfileID: "lifecycle-" + mode, Workspace: lower, FSScope: []string{lower}, Egress: []string{"example.com"}, Budget: budget, Approval: policy.ApprovalManual})
	instruction, _ := json.Marshal(map[string]any{"mode": mode})
	active, err := startProductionWorld(ctx, worldLaunch{
		Backend:   newIntegrationBackend(t, stateRoot, artifacts),
		SpawnSpec: world.NewSpawnSpec(effective, world.NewImageReference(artifacts.agentRepository, artifacts.agentDigest), []string{"integration"}, 0, traceID, childSpan, world.AgentIdentity{UID: 1000, GID: 1000}, nil),
		Writer:    writer, TraceID: traceID, ParentSpan: parentSpan, AdapterCommand: []string{artifacts.adapter},
		AdapterStderr: os.Stderr,
		AdapterName:   "world-testagent", Instruction: string(instruction), Workspace: "/workspace",
		Budget: budget, ProfileID: "lifecycle-" + mode, Approval: subagent.Spec{Approval: policy.ApprovalManual, Decider: policy.DenyAll{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	effectsDone := make(chan struct{})
	go func() {
		for range active.Lease.Effects() {
		}
		close(effectsDone)
	}()
	if mode == "stop" {
		waitRecord(t, store, gen.KindSubagentReady, 30*time.Second)
		if err := active.Subagent.Stop(gen.StopPayloadReasonUser); err != nil {
			t.Fatal(err)
		}
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	done, waitErr := active.Subagent.Wait(waitCtx)
	waitCancel()
	if mode == "stop" {
		if waitErr != nil || done.Status != gen.DonePayloadStatusStopped {
			t.Fatalf("stop result=%+v err=%v", done, waitErr)
		}
	} else if waitErr != nil || done.Status != gen.DonePayloadStatusError {
		t.Fatalf("%s result=%+v err=%v", mode, done, waitErr)
	}
	started := time.Now()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Second)
	err = active.Lease.Close(closeCtx)
	closeCancel()
	if err != nil || time.Since(started) > 20*time.Second {
		t.Fatalf("%s bounded cleanup: elapsed=%v err=%v", mode, time.Since(started), err)
	}
	waitSignal(t, effectsDone, 2*time.Second, mode+" effect drain")
	assertNoContainers(t, ctx, artifacts.agentRepository+"@"+artifacts.agentDigest)
}

func newIntegrationBackend(t *testing.T, stateRoot string, artifacts integrationArtifacts) world.Backend {
	t.Helper()
	backend, err := localworld.NewBackend(localworld.Config{
		StateRoot: stateRoot, ProxyImageRepository: artifacts.proxyRepository,
		ProxyImageDigest: artifacts.proxyDigest, ProxyIdentity: world.AgentIdentity{UID: 1000, GID: 1000},
		AuditQueueCapacity: 8, ApprovalCapacity: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func integrationPaths(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	lower, state := filepath.Join(root, "workspace"), filepath.Join(root, "state")
	for _, path := range []string{lower, state} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return lower, state
}

func assertContainerIsolation(t *testing.T, ctx context.Context, cid, adapter string, lease world.ActiveLease) {
	t.Helper()
	pidText := strings.TrimSpace(string(runOutput(t, ctx, "podman", "inspect", "--format", "{{.State.Pid}}", cid)))
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		t.Fatalf("container PID가 host runtime에서 관측되지 않음: %q", pidText)
	}
	var mounts []struct{ Source, Destination string }
	if err := json.Unmarshal(runOutput(t, ctx, "podman", "inspect", "--format", "{{json .Mounts}}", cid), &mounts); err != nil {
		t.Fatal(err)
	}
	for _, mount := range mounts {
		joined := mount.Source + "\x00" + mount.Destination
		for _, forbidden := range []string{
			adapter, lease.ProcessEndpoint().Address(), lease.ApprovalEndpoint().Address(),
			"podman.sock", "/run/podman", "/run/user/1000/podman",
		} {
			if forbidden != "" && strings.Contains(joined, forbidden) {
				t.Fatalf("agent mount에 host-only capability 노출: mount=%+v forbidden=%q", mount, forbidden)
			}
		}
	}
}

func assertLowerAndUpper(t *testing.T, lower, upper string) {
	t.Helper()
	for path, want := range map[string]string{"modified.txt": "original-modified\n", "deleted.txt": "original-deleted\n", "untouched.txt": "original-untouched\n"} {
		data, err := os.ReadFile(filepath.Join(lower, path))
		if err != nil || string(data) != want {
			t.Fatalf("lower가 변함 path=%s got=%q err=%v", path, data, err)
		}
	}
	for path, want := range map[string]string{"created.txt": "created\n", "modified.txt": "modified\n", "approved-marker.txt": "allowed\n", "flood-complete.txt": "complete\n"} {
		data, err := os.ReadFile(filepath.Join(upper, path))
		if err != nil || string(data) != want {
			t.Fatalf("upper file path=%s got=%q err=%v", path, data, err)
		}
	}
	info, err := os.Lstat(filepath.Join(upper, "deleted.txt"))
	if err != nil {
		t.Fatalf("delete whiteout lstat: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("delete whiteout stat 형식=%T", info.Sys())
	}
	if info.Mode()&os.ModeCharDevice == 0 || stat.Rdev != 0 {
		t.Fatalf("delete가 owner-independent char-device whiteout이 아님: mode=%v stat=%T rdev=%d", info.Mode(), info.Sys(), stat.Rdev)
	}
}

func assertSpawnBeforeStart(t *testing.T, records []gen.EventRecord, store *integrationStore) {
	t.Helper()
	var spawnSeq int64
	var started int64
	for _, record := range records {
		if record.Kind == gen.KindSubagentSpawn {
			spawnSeq = record.Seq
		}
		if record.Kind == gen.KindSubagentMessage {
			var payload gen.SubagentMessagePayload
			_ = json.Unmarshal(record.Payload, &payload)
			if strings.HasPrefix(payload.Text, "container-evidence ") {
				for _, field := range strings.Fields(payload.Text) {
					if strings.HasPrefix(field, "started_ns=") {
						started, _ = strconv.ParseInt(strings.TrimPrefix(field, "started_ns="), 10, 64)
					}
				}
				if !strings.Contains(payload.Text, "pid=1") || !strings.Contains(payload.Text, "exe=/testagent") {
					t.Fatalf("helper가 container PID 1이 아님: %s", payload.Text)
				}
			}
		}
	}
	if spawnSeq == 0 || started == 0 || !store.committedAt[spawnSeq].Before(time.Unix(0, started)) {
		t.Fatalf("spawn durable ACK가 process start보다 앞서지 않음: seq=%d commit=%v start=%v", spawnSeq, store.committedAt[spawnSeq], time.Unix(0, started))
	}
}

func assertApprovalAndFlood(t *testing.T, records []gen.EventRecord, decider *allowOnceDecider) {
	t.Helper()
	var requests []gen.SubagentApprovalRequestPayload
	var decisions []gen.PolicyDecisionPayload
	flood := 0
	rejected, done := false, false
	for _, record := range records {
		switch record.Kind {
		case gen.KindSubagentApprovalRequest:
			var payload gen.SubagentApprovalRequestPayload
			if err := json.Unmarshal(record.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if record.Raw == nil || *record.Raw == "" {
				t.Fatal("approval_request exact hook raw가 없음")
			}
			requests = append(requests, payload)
		case gen.KindPolicyDecision:
			var payload gen.PolicyDecisionPayload
			_ = json.Unmarshal(record.Payload, &payload)
			decisions = append(decisions, payload)
		case gen.KindSubagentMessage:
			var payload gen.SubagentMessagePayload
			_ = json.Unmarshal(record.Payload, &payload)
			if strings.HasPrefix(payload.Text, "flood-") && payload.Text != "flood-start" {
				flood++
			}
		case gen.KindSubagentToolResult:
			var payload gen.SubagentToolResultPayload
			_ = json.Unmarshal(record.Payload, &payload)
			if payload.Status == gen.SubagentToolResultPayloadStatusRejected && payload.Reason != nil && *payload.Reason == "duplicate tool intent" {
				rejected = true
			}
		case gen.KindSubagentDone:
			done = true
		}
	}
	if len(requests) != 2 || requests[0].CallID != requests[1].CallID || requests[0].RequestID == requests[1].RequestID || requests[0].Reason != nil || requests[1].Reason == nil || *requests[1].Reason != "duplicate tool intent" {
		t.Fatalf("approval allow/duplicate 재구성 실패: %+v", requests)
	}
	if len(decisions) != 2 || decisions[0].Decision != gen.PolicyDecisionPayloadDecisionAllow || decisions[1].Decision != gen.PolicyDecisionPayloadDecisionDeny || decisions[1].Reason == nil || *decisions[1].Reason != "duplicate tool intent" {
		t.Fatalf("policy allow/duplicate-deny 재구성 실패: %+v", decisions)
	}
	if decider.Calls() != 1 || !rejected || !done || flood != integrationFloodCount {
		t.Fatalf("approval/flood/done 불변식: decider=%d rejected=%v done=%v flood=%d", decider.Calls(), rejected, done, flood)
	}
}

func assertEgressEffects(t *testing.T, effects []world.EffectAttempt) {
	t.Helper()
	var allowHTTP, allowConnect, deny bool
	encoded, _ := json.Marshal(effects)
	for _, forbidden := range []string{"integration-secret-must-not-appear", "Authorization", "X-HX-Credential", "body="} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("audit에 body/header/credential 유출: %q", forbidden)
		}
	}
	for _, effect := range effects {
		switch {
		case effect.Target == "example.com" && effect.Method == httpMethodPost && effect.Decision == world.EffectDecisionAllow:
			allowHTTP = true
		case effect.Target == "example.com" && effect.Method == "CONNECT" && effect.Decision == world.EffectDecisionAllow:
			allowConnect = true
		case effect.Target == "forbidden.invalid" && effect.Decision == world.EffectDecisionDeny:
			deny = true
		}
	}
	if !allowHTTP || !allowConnect || !deny {
		t.Fatalf("allow/deny audit 누락: %+v", effects)
	}
}

const httpMethodPost = "POST"

func findAgentContainer(t *testing.T, ctx context.Context, image string) string {
	t.Helper()
	out := strings.Fields(string(runOutput(t, ctx, "podman", "ps", "--filter", "ancestor="+image, "--format", "{{.ID}}")))
	if len(out) != 1 {
		t.Fatalf("running agent container=%v", out)
	}
	return out[0]
}

func assertNoContainers(t *testing.T, ctx context.Context, image string) {
	t.Helper()
	out := strings.TrimSpace(string(runOutput(t, ctx, "podman", "ps", "-a", "--filter", "ancestor="+image, "--format", "{{.ID}}")))
	if out != "" {
		t.Fatalf("cleanup 뒤 container 잔존: %s", out)
	}
}

func waitRecord(t *testing.T, store *integrationStore, kind gen.Kind, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, record := range store.snapshot() {
			if record.Kind == kind {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %s timeout", kind)
}

func waitSignal(t *testing.T, signal <-chan struct{}, timeout time.Duration, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(timeout):
		t.Fatalf("%s timeout", name)
	}
}

func isMessage(record gen.EventRecord, want string) bool {
	if record.Kind != gen.KindSubagentMessage {
		return false
	}
	var payload gen.SubagentMessagePayload
	return json.Unmarshal(record.Payload, &payload) == nil && payload.Text == want
}

func mustWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func runCommand(t *testing.T, ctx context.Context, name string, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func runOutput(t *testing.T, ctx context.Context, name string, args ...string) []byte {
	t.Helper()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}
