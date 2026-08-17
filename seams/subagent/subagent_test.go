package subagent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
)

// FakeStore는 테스트 전용 인메모리 store다.
type FakeStore struct {
	mu      sync.Mutex
	events  []gen.EventRecord
	lastSeq int64
}

func (s *FakeStore) LastSeq(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq, nil
}

func (s *FakeStore) Append(ctx context.Context, rec gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, rec)
	s.lastSeq = rec.Seq
	return nil
}

func (s *FakeStore) AppendBatch(ctx context.Context, recs []gen.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range recs {
		s.events = append(s.events, rec)
		s.lastSeq = rec.Seq
	}
	return nil
}

func (s *FakeStore) ReadFrom(ctx context.Context, fromSeq int64) ([]gen.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]gen.EventRecord{}, s.events...)
	return out, nil
}

func (s *FakeStore) Close() error { return nil }

func buildNullAdapter(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "nulladapter")
	if out, err := exec.Command("go", "build", "-o", bin, "./nulladapter").CombinedOutput(); err != nil {
		t.Fatalf("빌드 실패: %v\n%s", err, out)
	}
	return bin
}

func spawnSpec(cmd []string, instruction string) Spec {
	return Spec{
		Adapter: "null", Command: cmd, Instruction: instruction,
		Workspace: "/workspace",
		Budget:    gen.Budget{Tokens: 1000, TimeMs: 1000, MaxDepth: 2},
		Depth:     0,
	}
}

// FR-ADP-02의 stop: 어댑터는 done(stopped)으로 응답하고 결과가 기록된다.
func TestStopPath(t *testing.T) {
	// stop만 보내는 시나리오: 어댑터가 task 없이 stop을 받도록 별도 spawn
	// 흐름이 필요하므로, task 후 즉시 stop이 아닌 stop-우선 시나리오는
	// null 어댑터의 결정적 각본상 불가 — 대신 sh 기반 즉답 어댑터로 검증.
	script := `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{},"raw":""}'
read line
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"stopped","result":"중단됨"},"raw":""}'`
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
		spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Stop(gen.StopPayloadReasonUser); err != nil {
		t.Fatal(err)
	}
	done, err := sub.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != gen.DonePayloadStatusStopped {
		t.Fatalf("status = %s (stopped 기대)", done.Status)
	}
}

// §5.2 위반 어댑터(비 NDJSON, raw 누락 등)는 거부된다 — 계약을 어기는
// 어댑터는 등록될 수 없다의 런타임 판.
func TestContractViolatingAdapterRejected(t *testing.T) {
	cases := map[string]string{
		"비 JSON 출력": `read line
printf 'not json at all\n'`,
		"raw 누락": `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{}}'`,
		"미지 kind": `read line
printf '%s\n' '{"v":1,"kind":"subagent/spawned","payload":{},"raw":""}'`,
		"done 없이 종료": `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{},"raw":""}'`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			store := &FakeStore{}
			w, err := logd.NewWriter(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			sub, err := Spawn(context.Background(), w, logd.NewTraceID(), logd.NewSpanID(), 1,
				spawnSpec([]string{"/bin/sh", "-c", script}, "지시"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sub.Wait(context.Background()); err == nil {
				t.Fatal("계약 위반 어댑터가 정상 완료로 처리됨")
			}
		})
	}
}

// null 어댑터 전 이벤트가 child span + subagent actor로 기록되고
// usage가 envelope에 집계된다 (FR-ADP-03/04/07, FR-LOG-10 전제).
func TestNullAdapterNormalization(t *testing.T) {
	bin := buildNullAdapter(t)
	store := &FakeStore{}
	w, err := logd.NewWriter(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	trace, root := logd.NewTraceID(), logd.NewSpanID()
	sub, err := Spawn(context.Background(), w, trace, root, 1, spawnSpec([]string{bin}, "요청"))
	if err != nil {
		t.Fatal(err)
	}
	done, err := sub.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != gen.DonePayloadStatusOk || !strings.Contains(done.Result, "요청") {
		t.Fatalf("done 이상: %+v", done)
	}
	events, _ := store.ReadFrom(context.Background(), 1)
	if events[0].Kind != gen.KindSubagentSpawn {
		t.Fatalf("첫 이벤트 %s (spawn 기대)", events[0].Kind)
	}
	child := events[0].SpanID
	if child == root {
		t.Fatal("child span이 발급되지 않음")
	}
	var sawUsage bool
	for _, e := range events[1:] {
		if e.SpanID != child || e.ParentSpanID == nil || *e.ParentSpanID != root {
			t.Fatalf("%s: span 귀속 이상", e.Kind)
		}
		if !strings.HasPrefix(e.Actor, "subagent:null:1") {
			t.Fatalf("%s: actor = %s", e.Kind, e.Actor)
		}
		if e.Raw == nil {
			t.Fatalf("%s: raw 누락 (FR-ADP-04)", e.Kind)
		}
		if e.Kind == gen.KindSubagentUsage {
			if e.UsageIn == nil || *e.UsageIn != 12 || e.UsageOut == nil || *e.UsageOut != 34 {
				t.Fatalf("usage envelope 집계 안 됨: %+v", e)
			}
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Fatal("usage 이벤트 없음")
	}
}
