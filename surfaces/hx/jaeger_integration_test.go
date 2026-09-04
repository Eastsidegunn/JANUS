//go:build jaegerintegration

package main

// This is the T14 §8-7 Linux gate. It uses a real Jaeger all-in-one receiver
// and the production OTel projection/export path; no fake exporter is evidence
// here. Pull, container, and readiness failures are infrastructure failures,
// while mismatched traces are verification failures.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/observe"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
)

// The digest is the immutable multi-architecture manifest for Jaeger 1.74.0.
// A tag is deliberately not used: the gate must not silently change images.
const jaegerIntegrationImage = "docker.io/jaegertracing/all-in-one@sha256:c87fc1d9b22766284168abb2ac57ac2160dfc26484e4f965ff2dcc6b849b263a"

const (
	jaegerTraceID = "11111111111111111111111111111111"
	jaegerRootID  = "2222222222222222"
	jaegerChildID = "3333333333333333"
)

type jaegerReference struct {
	RefType string `json:"refType"`
	TraceID string `json:"traceID"`
	SpanID  string `json:"spanID"`
}

type jaegerTag struct {
	Key   string          `json:"key"`
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

type jaegerSpan struct {
	TraceID       string            `json:"traceID"`
	SpanID        string            `json:"spanID"`
	OperationName string            `json:"operationName"`
	References    []jaegerReference `json:"references"`
	Tags          []jaegerTag       `json:"tags"`
}

type jaegerTrace struct {
	TraceID string       `json:"traceID"`
	Spans   []jaegerSpan `json:"spans"`
}

type jaegerTraceResponse struct {
	Data []jaegerTrace `json:"data"`
}

func TestJaegerIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatalf("INFRASTRUCTURE: Jaeger gate requires Linux (got %s); skip forbidden", runtime.GOOS)
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("INFRASTRUCTURE: podman unavailable: %v; skip forbidden", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()

	// Pull failure is reported separately from a receiver or assertion failure.
	if out, err := exec.CommandContext(ctx, "podman", "pull", "--quiet", jaegerIntegrationImage).CombinedOutput(); err != nil {
		t.Fatalf("INFRASTRUCTURE: Jaeger image pull failed (%s): %v\n%s", jaegerIntegrationImage, err, out)
	}
	queryPort := freeTCPPort(t)
	otlpPort := freeTCPPort(t)
	name := fmt.Sprintf("hx-jaeger-%d", time.Now().UnixNano())
	run := exec.CommandContext(ctx, "podman", "run", "-d", "--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:16686/tcp", queryPort),
		"-p", fmt.Sprintf("127.0.0.1:%d:4318/tcp", otlpPort), jaegerIntegrationImage)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("INFRASTRUCTURE: Jaeger container start failed: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, "podman", "rm", "-f", name).Run()
	})
	queryURL := fmt.Sprintf("http://127.0.0.1:%d", queryPort)
	if err := waitJaegerReady(ctx, queryURL); err != nil {
		t.Fatalf("INFRASTRUCTURE: Jaeger readiness failed: %v", err)
	}

	events := jaegerSnapshot()
	versions := observe.AdapterVersions{"codex": "0.147.0"}
	spans, err := observe.Project(events, versions)
	if err != nil {
		t.Fatalf("VERIFICATION: project snapshot: %v", err)
	}
	spansAgain, err := observe.Project(events, versions)
	if err != nil || !reflect.DeepEqual(spansAgain, spans) {
		t.Fatalf("VERIFICATION: same snapshot projected different spans: err=%v", err)
	}

	exporter, err := observe.NewOTLPHTTPExporter(ctx,
		otlptracehttp.WithEndpoint(fmt.Sprintf("127.0.0.1:%d", otlpPort)),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		t.Fatalf("VERIFICATION: create OTLP exporter: %v", err)
	}
	var exportErr error
	observe.ExportBestEffort(ctx, exporter, spans, func(err error) { exportErr = errors.Join(exportErr, err) })
	if exportErr != nil {
		t.Fatalf("VERIFICATION: OTLP export failed: %v", exportErr)
	}
	first, err := fetchJaegerTrace(ctx, queryURL, jaegerTraceID)
	if err != nil {
		t.Fatalf("VERIFICATION: first Jaeger trace query: %v", err)
	}

	// Export the identical deterministic snapshot a second time. Jaeger may
	// retain duplicate span rows, so canonicalTrace deduplicates equal IDs and
	// rejects any differing duplicate before comparing the two observations.
	observe.ExportBestEffort(ctx, exporter, spansAgain, func(err error) { exportErr = errors.Join(exportErr, err) })
	if exportErr != nil {
		t.Fatalf("VERIFICATION: second OTLP export failed: %v", exportErr)
	}
	second, err := fetchJaegerTrace(ctx, queryURL, jaegerTraceID)
	if err != nil {
		t.Fatalf("VERIFICATION: second Jaeger trace query: %v", err)
	}
	if first != second {
		t.Fatalf("VERIFICATION: non-deterministic Jaeger trace\nfirst=%s\nsecond=%s", first, second)
	}
	assertJaegerTrace(t, first)
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("INFRASTRUCTURE: reserve port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitJaegerReady(ctx context.Context, base string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	for {
		resp, err := client.Get(base + "/api/services")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("readiness timeout")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func fetchJaegerTrace(ctx context.Context, base, traceID string) (string, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/traces/"+traceID, nil)
		resp, err := client.Do(req)
		if err == nil {
			var body jaegerTraceResponse
			decodeErr := json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if decodeErr == nil {
				canonical, ok, canonicalErr := canonicalTrace(body, traceID)
				if canonicalErr != nil {
					return "", canonicalErr
				}
				if ok {
					return canonical, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("Jaeger trace did not arrive before timeout")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func canonicalTrace(response jaegerTraceResponse, wantTraceID string) (string, bool, error) {
	var spans []jaegerSpan
	for _, trace := range response.Data {
		if !strings.EqualFold(trace.TraceID, wantTraceID) {
			continue
		}
		spans = append(spans, trace.Spans...)
	}
	if len(spans) == 0 {
		return "", false, nil
	}
	byID := map[string]jaegerSpan{}
	for _, span := range spans {
		if previous, exists := byID[span.SpanID]; exists && !reflect.DeepEqual(canonicalSpan(previous), canonicalSpan(span)) {
			return "", false, fmt.Errorf("duplicate span %s differs", span.SpanID)
		}
		byID[span.SpanID] = span
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ordered := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		ordered = append(ordered, canonicalSpan(byID[id]))
	}
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return "", false, fmt.Errorf("canonical trace: %w", err)
	}
	return string(encoded), true, nil
}

func canonicalSpan(span jaegerSpan) map[string]any {
	refs := append([]jaegerReference(nil), span.References...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].RefType != refs[j].RefType {
			return refs[i].RefType < refs[j].RefType
		}
		return refs[i].SpanID < refs[j].SpanID
	})
	tags := append([]jaegerTag(nil), span.Tags...)
	sort.Slice(tags, func(i, j int) bool { return tags[i].Key < tags[j].Key })
	return map[string]any{"traceID": span.TraceID, "spanID": span.SpanID, "operationName": span.OperationName, "references": refs, "tags": tags}
}

func assertJaegerTrace(t *testing.T, canonical string) {
	t.Helper()
	var spans []jaegerSpan
	if err := json.Unmarshal([]byte(canonical), &spans); err != nil {
		t.Fatalf("VERIFICATION: decode canonical trace: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("VERIFICATION: span count=%d want 2 (root + spawn child)", len(spans))
	}
	byID := map[string]jaegerSpan{}
	for _, span := range spans {
		if span.TraceID != jaegerTraceID {
			t.Fatalf("VERIFICATION: trace ID=%s want %s", span.TraceID, jaegerTraceID)
		}
		byID[span.SpanID] = span
	}
	root, ok := byID[jaegerRootID]
	if !ok || root.OperationName != "hx.session" || len(root.References) != 0 {
		t.Fatalf("VERIFICATION: root span malformed: %+v", root)
	}
	child, ok := byID[jaegerChildID]
	if !ok || child.OperationName != "hx.subagent" || len(child.References) != 1 || child.References[0].RefType != "CHILD_OF" || child.References[0].TraceID != jaegerTraceID || child.References[0].SpanID != jaegerRootID {
		t.Fatalf("VERIFICATION: spawn child parent malformed: %+v", child)
	}
	tags := map[string]jaegerTag{}
	for _, tag := range child.Tags {
		tags[tag.Key] = tag
	}
	for _, key := range []string{"hx.adapter.name", "hx.adapter.version", "hx.profile.id", "hx.status", "hx.usage.input_tokens", "hx.usage.output_tokens"} {
		if _, ok := tags[key]; !ok {
			t.Fatalf("VERIFICATION: child attribute %q missing", key)
		}
	}
	if string(tags["hx.adapter.name"].Value) != `"codex"` || string(tags["hx.adapter.version"].Value) != `"0.147.0"` || string(tags["hx.profile.id"].Value) != `"sandbox"` || string(tags["hx.status"].Value) != `"ok"` || string(tags["hx.usage.input_tokens"].Value) != "7" || string(tags["hx.usage.output_tokens"].Value) != "11" {
		t.Fatalf("VERIFICATION: child attributes have unexpected values: %+v", tags)
	}
}

func jaegerSnapshot() []gen.EventRecord {
	parent, profile, image, raw := jaegerRootID, "sandbox", "sha256:"+strings.Repeat("a", 64), ""
	spawn, _ := json.Marshal(gen.SubagentSpawnPayload{
		Adapter: "codex", Instruction: "test", Budget: gen.SpawnBudget{Tokens: 10, TimeMs: 20, MaxDepth: 2},
		WorldBackend: gen.SubagentSpawnPayloadWorldBackendLocalPodman, ProfileID: &profile, ImageDigest: &image,
		Mounts: []gen.SubagentSpawnMount{{SourcePath: "/tmp/work", TargetPath: gen.SubagentSpawnMountTargetPathWorkspace, Mode: gen.SubagentSpawnMountModeOverlay, UpperRef: "u"}},
	})
	done, _ := json.Marshal(gen.SubagentDonePayload{Status: gen.SubagentDonePayloadStatusOk, Result: "done"})
	in, out := int64(7), int64(11)
	return []gen.EventRecord{
		{Seq: 1, Ts: 1000, TraceID: jaegerTraceID, SpanID: jaegerRootID, Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`), Raw: &raw},
		{Seq: 2, Ts: 1010, TraceID: jaegerTraceID, SpanID: jaegerChildID, ParentSpanID: &parent, Kind: gen.KindSubagentSpawn, Actor: "parent", Payload: spawn, Raw: &raw},
		{Seq: 3, Ts: 1020, TraceID: jaegerTraceID, SpanID: jaegerChildID, ParentSpanID: &parent, Kind: gen.KindSubagentUsage, Actor: "subagent:codex:1", Payload: json.RawMessage(`{"input_tokens":7,"output_tokens":11}`), Raw: &raw, UsageIn: &in, UsageOut: &out},
		{Seq: 4, Ts: 1030, TraceID: jaegerTraceID, SpanID: jaegerChildID, ParentSpanID: &parent, Kind: gen.KindSubagentDone, Actor: "subagent:codex:1", Payload: done, Raw: &raw},
		{Seq: 5, Ts: 1040, TraceID: jaegerTraceID, SpanID: jaegerRootID, Kind: gen.KindSessionEnd, Actor: "parent", Payload: json.RawMessage(`{}`), Raw: &raw},
	}
}
