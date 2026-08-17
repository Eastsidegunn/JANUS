// hx는 HX의 CLI 표면이다 (조립 지점, §3.1).
//
//	hx run    --session <db> --adapter <실행파일> [--workspace <경로>] <instruction>
//	hx replay --session <db> [--to <seq>]
//
// FR-CLI-06: 이벤트는 stdout NDJSON, 진단은 stderr, 파이프라인 전제.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "사용법: hx <run|replay> …")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "replay":
		err = replayCmd(os.Args[2:])
	default:
		err = fmt.Errorf("미지의 하위 명령 %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hx:", err)
		os.Exit(1)
	}
}

// runCmd는 새 세션을 시작한다 (FR-CLI-01):
// session/start → 어댑터 spawn → NDJSON 정규화·기록(child span) → session/end.
func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	session := fs.String("session", "", "세션 로그 파일 경로 (필수)")
	adapter := fs.String("adapter", "", "어댑터 실행 파일 경로 (필수)")
	workspace := fs.String("workspace", "/workspace", "어댑터에 전달할 워크스페이스 경로")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *session == "" || *adapter == "" || fs.NArg() != 1 {
		return fmt.Errorf("사용법: hx run --session <db> --adapter <실행파일> [--workspace <경로>] <instruction>")
	}
	instruction := fs.Arg(0)

	ctx := context.Background()
	log, err := sqlite.Open(ctx, *session)
	if err != nil {
		return err
	}
	defer log.Close()

	traceID := logd.NewTraceID()
	rootSpan := logd.NewSpanID()
	emit := func(kind gen.Kind) error {
		_, err := log.Writer.Submit(ctx, gen.EventRecord{
			Ts: nowMs(), TraceID: traceID, SpanID: rootSpan,
			Kind: kind, Actor: "parent", Payload: json.RawMessage(`{}`),
		})
		return err
	}
	if err := emit(gen.KindSessionStart); err != nil {
		return err
	}

	sub, err := subagent.Spawn(ctx, log.Writer, traceID, rootSpan, 1, subagent.Spec{
		Adapter:     "null",
		Command:     []string{*adapter},
		Instruction: instruction,
		Workspace:   *workspace,
		Budget:      gen.Budget{Tokens: 1_000_000, TimeMs: 600_000, MaxDepth: 2},
		Depth:       0,
	})
	if err != nil {
		return err
	}
	done, err := sub.Wait(ctx)
	if err != nil {
		return err
	}
	if err := emit(gen.KindSessionEnd); err != nil {
		return err
	}
	if err := printEvents(ctx, log.Reader, 0); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "hx run: 세션 완료 status=%s result=%q\n", done.Status, done.Result)
	if done.Status != gen.DonePayloadStatusOk {
		return fmt.Errorf("서브에이전트 status=%s", done.Status)
	}
	return nil
}

// replayCmd는 세션을 재생한다 (FR-CLI-02, --to 지원). 이벤트는 stdout
// NDJSON, 파생 상태 요약은 stderr — 동일 세션의 재생은 결정론적이다(FR-LOG-06).
func replayCmd(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	session := fs.String("session", "", "세션 로그 파일 경로 (필수)")
	to := fs.Int64("to", 0, "이 seq까지만 재생 (0 = 전체)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *session == "" {
		return fmt.Errorf("사용법: hx replay --session <db> [--to <seq>]")
	}
	ctx := context.Background()
	log, err := sqlite.Open(ctx, *session)
	if err != nil {
		return err
	}
	defer log.Close()

	if err := printEvents(ctx, log.Reader, *to); err != nil {
		return err
	}
	events, err := readTo(ctx, log.Reader, *to)
	if err != nil {
		return err
	}
	state, err := logd.Replay(events)
	if err != nil {
		return fmt.Errorf("재생: %w", err)
	}
	fmt.Fprintf(os.Stderr,
		"hx replay: trace=%s seq=%d turns=%d steps=%d spawns=%d messages=%d usage=%d/%d ended=%v\n",
		state.TraceID, state.LastSeq, state.Turns, state.Steps, state.Spawns,
		len(state.Messages), state.UsageIn, state.UsageOut, state.Ended)
	return nil
}

func nowMs() int64 { return time.Now().UnixMilli() }

func readTo(ctx context.Context, r logd.Reader, to int64) ([]gen.EventRecord, error) {
	events, err := r.ReadFrom(ctx, 1)
	if err != nil {
		return nil, err
	}
	if to <= 0 {
		return events, nil
	}
	cut := events[:0:0]
	for _, e := range events {
		if e.Seq <= to {
			cut = append(cut, e)
		}
	}
	return cut, nil
}

func printEvents(ctx context.Context, r logd.Reader, to int64) error {
	events, err := readTo(ctx, r, to)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}
