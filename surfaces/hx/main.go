// hx는 HX의 CLI 표면이다 (조립 지점, §3.1).
//
//	hx run    --session <db> --adapter <실행파일> [--workspace <경로>] <instruction>
//	hx replay --session <db> [--to <seq>]
//	hx dump-config --profile <file> [--overlay <file> ...]
//
// FR-CLI-06: 이벤트는 stdout NDJSON, 진단은 stderr, 파이프라인 전제.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/audit"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/policy"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
	"github.com/Eastsidegunn/JANUS/seams/subagent"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "사용법: hx <run|replay|audit|dump-config> …")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "replay":
		err = replayCmd(os.Args[2:])
	case "audit":
		err = auditCmd(os.Args[2:])
	case "dump-config":
		err = dumpConfigCmd(os.Args[2:])
	default:
		err = fmt.Errorf("미지의 하위 명령 %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hx:", err)
		os.Exit(1)
	}
}

type auditQuery struct {
	Session string
	Span    string
	Actor   string
	Cost    bool
	AtSeq   int64
}

// auditCmd is intentionally a write-after-success wrapper. auditSession builds
// the complete output in memory, so malformed/incomplete logs cannot leak a
// partial table to stdout.
func auditCmd(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	session := fs.String("session", "", "세션 로그 파일 경로 (필수)")
	span := fs.String("span", "", "특정 child span으로 제한")
	actor := fs.String("actor", "", "특정 actor로 제한")
	cost := fs.Bool("cost", false, "usage 비용 집계 포함")
	atSeq := fs.Int64("at-seq", 0, "이 seq까지 포함 (0 = 전체)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *session == "" && fs.NArg() == 1 {
		*session = fs.Arg(0)
	}
	if *session == "" || fs.NArg() > 1 || *atSeq < 0 {
		return fmt.Errorf("사용법: hx audit --session <db> [--span <id>] [--actor <name>] [--cost] [--at-seq <seq>]")
	}
	var out bytes.Buffer
	if err := auditSession(context.Background(), auditQuery{Session: *session, Span: *span, Actor: *actor, Cost: *cost, AtSeq: *atSeq}, &out); err != nil {
		return err
	}
	_, err := os.Stdout.Write(out.Bytes())
	return err
}

// auditSession is the sole surface assembly point: one Reader snapshot feeds
// both logd.Replay (cost/context) and audit.DecodeEvents (comparison). It never
// obtains a writer or mutates the session log.
func auditSession(ctx context.Context, query auditQuery, out io.Writer) error {
	log, err := sqlite.Open(ctx, query.Session)
	if err != nil {
		return err
	}
	defer log.Close()
	events, err := log.Reader.ReadFrom(ctx, 1)
	if err != nil {
		return err
	}
	events = prefixEvents(events, query.AtSeq)
	if len(events) == 0 {
		return fmt.Errorf("audit: 빈 세션 또는 지정 seq 이전에 이벤트가 없음")
	}
	if query.Span != "" && !eventHasSpan(events, query.Span) {
		return fmt.Errorf("audit: span %q를 찾을 수 없음", query.Span)
	}
	if query.Actor != "" && !eventHasActor(events, query.Actor) {
		return fmt.Errorf("audit: actor %q를 찾을 수 없음", query.Actor)
	}
	state, err := logd.Replay(events)
	if err != nil {
		return fmt.Errorf("audit replay: %w", err)
	}
	_, _, report, err := audit.DecodeEvents(events, "/workspace")
	if err != nil {
		return err
	}
	if query.Span != "" || query.Actor != "" {
		filtered := report
		filtered.Rows = filtered.Rows[:0]
		for _, row := range report.Rows {
			if query.Span != "" && row.SpanID != query.Span {
				continue
			}
			if query.Actor != "" && row.Actor != query.Actor {
				continue
			}
			filtered.Rows = append(filtered.Rows, row)
		}
		report = filtered
	}
	opts := audit.RenderOptions{IncludeCost: query.Cost}
	if query.Cost {
		usageByActor := state.UsageByActor
		if query.Span != "" {
			spanUsage := state.UsageBySpan[query.Span]
			opts.UsageIn, opts.UsageOut = spanUsage.In, spanUsage.Out
			usageByActor = state.UsageBySpanActor[query.Span]
		} else {
			opts.UsageIn, opts.UsageOut = state.UsageIn, state.UsageOut
		}
		opts.UsageByActor = make(map[string]audit.UsageTotals, len(usageByActor))
		for actor, usage := range usageByActor {
			if query.Actor != "" && actor != query.Actor {
				continue
			}
			opts.UsageByActor[actor] = audit.UsageTotals{In: usage.In, Out: usage.Out}
		}
		if query.Actor != "" {
			usage := usageByActor[query.Actor]
			opts.UsageIn, opts.UsageOut = usage.In, usage.Out
		}
	}
	if query.AtSeq > 0 {
		opts.AtSeqContext = make([]audit.ContextEntry, 0, len(state.Messages))
		for _, message := range state.Messages {
			opts.AtSeqContext = append(opts.AtSeqContext, audit.ContextEntry{
				Seq: message.Seq, Role: string(message.Role), SpanID: message.SpanID,
				Summary: contextSummary(message.Role, message.Content),
			})
		}
	}
	rendered, err := audit.RenderWithOptions(report, opts)
	if err != nil {
		return err
	}
	_, err = out.Write(rendered)
	return err
}

func prefixEvents(events []gen.EventRecord, to int64) []gen.EventRecord {
	if to <= 0 {
		return events
	}
	cut := make([]gen.EventRecord, 0, len(events))
	for _, event := range events {
		if event.Seq <= to {
			cut = append(cut, event)
		}
	}
	return cut
}

func eventHasSpan(events []gen.EventRecord, span string) bool {
	for _, event := range events {
		if event.SpanID == span {
			return true
		}
	}
	return false
}

func eventHasActor(events []gen.EventRecord, actor string) bool {
	for _, event := range events {
		if event.Actor == actor {
			return true
		}
	}
	return false
}

func contextSummary(role logd.Role, payload json.RawMessage) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return string(role)
	}
	if raw, ok := fields["text"]; ok {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return text
		}
	}
	if raw, ok := fields["name"]; ok {
		var name string
		if json.Unmarshal(raw, &name) == nil {
			return "tool:" + name
		}
	}
	if raw, ok := fields["status"]; ok {
		var status string
		if json.Unmarshal(raw, &status) == nil {
			return "status:" + status
		}
	}
	if raw, ok := fields["result"]; ok {
		var result string
		if json.Unmarshal(raw, &result) == nil {
			return result
		}
	}
	return strings.TrimSpace(string(role))
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
	// session/start는 배타적 초기화 배치로 기록한다 — 공백 확인과 기록이
	// writer 루프 안에서 원자적이라, 같은 파일에 두 번째 run이 겹쳐 로그를
	// 복수 trace로 오염시키는 경로가 없다(기존 로그는 불변으로 거부).
	if err := log.Writer.InitBatch(ctx, []gen.EventRecord{{
		Ts: nowMs(), TraceID: traceID, SpanID: rootSpan,
		Kind: gen.KindSessionStart, Actor: "parent", Payload: json.RawMessage(`{}`),
	}}); err != nil {
		if errors.Is(err, logd.ErrDestinationNotEmpty) {
			return fmt.Errorf("세션 파일 %s에 이미 로그가 있음 — 기존 세션은 불변이며, 새 세션은 새 파일로 시작하라", *session)
		}
		return err
	}
	emit := func(kind gen.Kind) error {
		_, err := log.Writer.Submit(ctx, gen.EventRecord{
			Ts: nowMs(), TraceID: traceID, SpanID: rootSpan,
			Kind: kind, Actor: "parent", Payload: json.RawMessage(`{}`),
		})
		return err
	}

	profile := policy.Profile{
		ID: "hx-default", FSScope: []string{"/"},
		Budget:   gen.Budget{Tokens: 1_000_000, TimeMs: 600_000, MaxDepth: 2},
		Approval: policy.ApprovalManual,
	}
	sandbox, denial := policy.Evaluate(profile, policy.SpawnRequest{
		Adapter: "null", Workspace: *workspace, Depth: 0,
	})
	if denial != nil {
		return denial
	}
	sub, err := subagent.Spawn(ctx, log.Writer, traceID, rootSpan, 1, subagent.Spec{
		Adapter:     "null",
		Command:     []string{*adapter},
		Instruction: instruction,
		Workspace:   sandbox.Workspace,
		Budget:      sandbox.Budget,
		Depth:       0,
		ProfileID:   sandbox.ProfileID,
		Approval:    sandbox.Approval,
		Decider:     policy.DenyAll{},
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
	finalEvents, err := readTo(ctx, log.Reader, 0)
	if err != nil {
		return err
	}
	if err := printSnapshot(finalEvents); err != nil {
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

	// 로그는 한 번만 읽고, Replay가 성공한 뒤에만 같은 snapshot을 stdout에
	// 출력한다 — 손상 로그에서 이벤트를 흘린 뒤 실패하거나, 동시 append로
	// 출력과 요약의 snapshot이 어긋나는 경로가 없다.
	events, err := readTo(ctx, log.Reader, *to)
	if err != nil {
		return err
	}
	state, err := logd.Replay(events)
	if err != nil {
		return fmt.Errorf("재생: %w", err)
	}
	if err := printSnapshot(events); err != nil {
		return err
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
	return prefixEvents(events, to), nil
}

func printSnapshot(events []gen.EventRecord) error {
	enc := json.NewEncoder(os.Stdout)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}
