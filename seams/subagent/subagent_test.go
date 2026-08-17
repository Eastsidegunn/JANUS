package subagent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// T7 재리뷰 차단 1의 회귀: §5.2 시퀀스 위반은 전부 거부된다 (FR-ADP-03).
func TestSequenceViolationsRejected(t *testing.T) {
	cases := map[string]string{
		"ready 없는 done": `read line
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'`,
		"ready 중복": `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'`,
		"done 이후 출력": `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/message","payload":{"text":"유령"},"raw":""}'`,
		"ready 전 중간 이벤트": `read line
printf '%s\n' '{"v":1,"kind":"subagent/message","payload":{"text":"x"},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'`,
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
				t.Fatal("시퀀스 위반 어댑터가 정상 완료로 처리됨")
			}
		})
	}
}

// T7 재리뷰 차단 2의 회귀 (1): done 이후 프로세스가 늘어져도 Wait(ctx)는
// deadline을 지킨다 — kill 후 즉시 반환, reap은 펌프가 수행.
func TestWaitHonorsContextAfterDone(t *testing.T) {
	script := `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'
sleep 5`
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
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, waitErr := sub.Wait(ctx)
	elapsed := time.Since(start)
	if waitErr == nil {
		t.Fatal("deadline이 무시되고 성공 반환됨")
	}
	if elapsed > time.Second {
		t.Fatalf("Wait가 %v 소요 — deadline(100ms) 무시", elapsed)
	}
}

// T7 재리뷰 차단 2의 회귀 (2): 검증 실패 후 프로세스가 늘어져도 kill되어
// 빠르게 오류가 반환된다 (fail 후 hang 없음).
func TestInvalidEventKillsLingeringProcess(t *testing.T) {
	script := `read line
printf 'not json\n'
sleep 5`
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
	start := time.Now()
	_, waitErr := sub.Wait(context.Background())
	if waitErr == nil {
		t.Fatal("검증 실패가 성공으로 처리됨")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("kill이 동작하지 않아 %v 소요", time.Since(start))
	}
}

// T7 재리뷰 차단 2의 회귀 (3): done 이후 비정상 exit 코드는 보존된다.
func TestAbnormalExitAfterDonePreserved(t *testing.T) {
	script := `read line
printf '%s\n' '{"v":1,"kind":"subagent/ready","payload":{},"raw":""}'
printf '%s\n' '{"v":1,"kind":"subagent/done","payload":{"status":"ok","result":"r"},"raw":""}'
exit 3`
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
	if _, err := sub.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "비정상 종료") {
		t.Fatalf("exit 오류가 보존되지 않음: %v", err)
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
