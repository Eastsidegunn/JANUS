package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/processwire"
)

// SCP-T25-001 / T25 (e): the oneshot argv produced through the mode-aware
// single source is byte-identical to the pinned T24 argv, for both the
// explicit oneshot value and the absent (empty) mode.
func TestContainerArgvForOneshotIsByteIdenticalToT24(t *testing.T) {
	instruction := "ls /workspace\n한글 'quoted' $HOME"
	want := []string{"/opt/bin/claude", "-p", instruction, "--output-format", "stream-json", "--verbose", "--no-session-persistence", "--permission-mode", "manual", "--setting-sources", "project,local", "--settings", `{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"hxapprove || exit 2","timeout":600}]}]}}`}
	for _, mode := range []gen.SubagentSpawnPayloadSessionMode{"", gen.SubagentSpawnPayloadSessionModeOneshot} {
		if got := ContainerArgvFor(want[0], instruction, mode); !reflect.DeepEqual(got, want) {
			t.Fatalf("mode %q argv drift:\ngot:  %q\nwant: %q", mode, got, want)
		}
	}
	if got := ContainerArgv(want[0], instruction); !reflect.DeepEqual(got, want) {
		t.Fatalf("ContainerArgv drift: %q", got)
	}
}

// multiturn: instruction leaves argv (it is the first stdin turn), stream-json
// input is enabled, and every isolation/approval flag is unchanged.
func TestContainerArgvForMultiturnKeepsIsolationFlags(t *testing.T) {
	instruction := "첫 지시 -- --dangerous"
	got := ContainerArgvFor("/opt/bin/claude", instruction, gen.SubagentSpawnPayloadSessionModeMultiturn)
	want := []string{"/opt/bin/claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--no-session-persistence", "--permission-mode", "manual", "--setting-sources", "project,local", "--settings", `{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"hxapprove || exit 2","timeout":600}]}]}}`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multiturn argv:\ngot:  %q\nwant: %q", got, want)
	}
	oneshot := ContainerArgv("/opt/bin/claude", instruction)
	if !reflect.DeepEqual(got[4:], oneshot[3:]) {
		t.Fatalf("isolation tail drift: multiturn=%q oneshot=%q", got[4:], oneshot[3:])
	}
	for _, arg := range got {
		if strings.Contains(arg, instruction) {
			t.Fatalf("multiturn argv carries instruction: %q", got)
		}
	}
}

func TestUserMessageLineIsStreamJSONUserTurn(t *testing.T) {
	line, err := userMessageLine("둘째 \"턴\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(line, []byte{'\n'}) {
		t.Fatalf("line contains raw newline: %q", line)
	}
	want := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"둘째 \"턴\"\n"}]}}`
	if string(line) != want {
		t.Fatalf("line=%s", line)
	}
}

func fixtureLines(t *testing.T, name string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSpace(b), []byte{'\n'})
}

func kindsOf(events []Event) []gen.EventKind {
	out := make([]gen.EventKind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

// multiturn parser: a turn result yields only usage (existing kind, no done);
// a later turn is parsed with the unchanged message/tool_call mapping; the
// session's single done is projected from the last turn result.
func TestMultiturnParserTurnResultIsNotTerminal(t *testing.T) {
	for _, stop := range []bool{false, true} {
		p := NewMultiturnParser()
		var all []Event
		first := fixtureLines(t, "01-simple-text.ndjson")
		second := fixtureLines(t, "02-single-tool.ndjson")
		for _, line := range first {
			events, err := p.ParseLine(line)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, events...)
		}
		if p.Done() || p.Turns() != 1 {
			t.Fatalf("turn 1 result terminated session: done=%v turns=%d", p.Done(), p.Turns())
		}
		// Re-announced init of the same native session at a turn boundary is
		// consumed without a second ready.
		initLine := first[0]
		events, err := p.ParseLine(initLine)
		if err != nil || len(events) != 0 || p.Disposition() != "consumed:turn-init" {
			t.Fatalf("same-session turn init: events=%v err=%v disposition=%q", events, err, p.Disposition())
		}
		for _, line := range second {
			if bytes.Contains(line, []byte(`"subtype":"init"`)) {
				continue
			}
			events, err := p.ParseLine(line)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, events...)
		}
		want := []gen.EventKind{
			gen.EventKindSubagentReady, gen.EventKindSubagentMessage, gen.EventKindSubagentUsage,
			gen.EventKindSubagentToolCall, gen.EventKindSubagentToolResult, gen.EventKindSubagentMessage, gen.EventKindSubagentUsage,
		}
		if got := kindsOf(all); !reflect.DeepEqual(got, want) {
			t.Fatalf("kinds=%v want=%v", got, want)
		}
		if stop {
			p.NoteStop()
		}
		done, err := p.TurnDone()
		if err != nil || done == nil {
			t.Fatalf("TurnDone: %v %v", done, err)
		}
		var payload gen.DonePayload
		if err := json.Unmarshal(done.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		wantStatus := gen.DonePayloadStatusOk
		if stop {
			wantStatus = gen.DonePayloadStatusStopped
		}
		if payload.Status != wantStatus || payload.Result != "a.txt\nb.txt" {
			t.Fatalf("done=%+v", payload)
		}
		if !p.Done() {
			t.Fatal("TurnDone did not close the parser")
		}
		if again, _ := p.TurnDone(); again != nil {
			t.Fatal("TurnDone emitted a second done")
		}
		if _, err := p.ParseLine(second[1]); err == nil {
			t.Fatal("output after terminal done accepted")
		}
	}
}

func TestMultiturnParserRejectsForeignOrMidTurnInit(t *testing.T) {
	first := fixtureLines(t, "01-simple-text.ndjson")
	foreign := fixtureLines(t, "02-single-tool.ndjson")[0]

	midTurn := NewMultiturnParser()
	if _, err := midTurn.ParseLine(first[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := midTurn.ParseLine(first[0]); err == nil {
		t.Fatal("init before any turn result accepted")
	}

	other := NewMultiturnParser()
	for _, line := range first {
		if _, err := other.ParseLine(line); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := other.ParseLine(foreign); err == nil {
		t.Fatal("init of a different native session accepted")
	}

	oneshot := NewParser()
	if done, err := oneshot.TurnDone(); done != nil || err != nil {
		t.Fatalf("oneshot TurnDone=%v %v", done, err)
	}
}

func TestRunRejectsMultiturnOutsideWorldAndUnknownMode(t *testing.T) {
	for mode, want := range map[gen.SubagentSpawnPayloadSessionMode]string{
		gen.SubagentSpawnPayloadSessionModeMultiturn: "local-podman process endpoint",
		"interactive": "미지 session_mode",
	} {
		var out, stderr bytes.Buffer
		err := Run(context.Background(), io.NopCloser(bytes.NewReader(taskCommandLine(t, t.TempDir()))), &out, &stderr, Config{ClaudeBin: "/does/not/execute", SessionMode: mode})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("mode %q: err=%v", mode, err)
		}
		if out.Len() != 0 {
			t.Fatalf("mode %q emitted output: %s", mode, out.String())
		}
	}
}

func TestConfigFromEnvReadsSessionModeWithoutPassingItToAgent(t *testing.T) {
	t.Setenv(sessionModeEnv, "multiturn")
	cfg := ConfigFromEnv()
	if cfg.SessionMode != gen.SubagentSpawnPayloadSessionModeMultiturn || !cfg.multiturn() {
		t.Fatalf("session mode=%q", cfg.SessionMode)
	}
	for _, item := range cfg.Env {
		if strings.HasPrefix(item, sessionModeEnv+"=") {
			t.Fatalf("session mode leaked to native env: %q", item)
		}
	}
}

// multiturnBroker is a test-only process broker that keeps the container
// stdin open: it streams native stdout as it is produced, forwards every
// StdinData frame, and reports whether a StdinClose was ever requested.
type multiturnBroker struct {
	listener   net.Listener
	stdinClose chan struct{}
	stdinData  chan []byte
	finished   chan error
}

func startMultiturnBroker(t *testing.T, ctx context.Context, argv []string, env []string) *multiturnBroker {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hx-t25-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "process.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	b := &multiturnBroker{listener: listener, stdinClose: make(chan struct{}, 1), stdinData: make(chan []byte, 16), finished: make(chan error, 1)}
	go func() { b.finished <- b.serve(ctx, argv, env) }()
	return b
}

func (b *multiturnBroker) serve(ctx context.Context, argv []string, env []string) error {
	output, err := b.listener.Accept()
	if err != nil {
		return err
	}
	defer output.Close()
	out := processwire.NewEncoder(output)
	if _, err := processwire.NewDecoder(output).Read(); err != nil {
		return err
	}
	if _, err := out.Write(processwire.KindHelloAck, processwire.StreamControl, 0, nil); err != nil {
		return err
	}
	control, err := b.listener.Accept()
	if err != nil {
		return err
	}
	defer control.Close()
	dec := processwire.NewDecoder(control)
	var encMu sync.Mutex
	enc := processwire.NewEncoder(control)
	write := func(kind processwire.Kind, payload []byte) error {
		encMu.Lock()
		defer encMu.Unlock()
		_, err := enc.Write(kind, processwire.StreamControl, 0, payload)
		return err
	}
	if _, err := dec.Read(); err != nil {
		return err
	}
	if err := write(processwire.KindHelloAck, nil); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	for {
		frame, err := dec.Read()
		if err != nil {
			// The adapter closes control after its terminal event.
			select {
			case werr := <-exited:
				return werr
			case <-time.After(5 * time.Second):
				return fmt.Errorf("control closed before exit: %v", err)
			}
		}
		switch frame.Kind {
		case processwire.KindStart:
			if err := cmd.Start(); err != nil {
				return err
			}
			go func() {
				buf := make([]byte, 32*1024)
				for {
					n, rerr := stdout.Read(buf)
					if n > 0 {
						if _, err := out.Write(processwire.KindStdoutData, processwire.StreamStdout, 0, append([]byte(nil), buf[:n]...)); err != nil {
							exited <- err
							return
						}
					}
					if rerr != nil {
						break
					}
				}
				_, _ = out.Write(processwire.KindStreamEnd, processwire.StreamControl, 0, nil)
				werr := cmd.Wait()
				code := 0
				if werr != nil {
					code = 137
				}
				exit, _ := processwire.Marshal(processwire.ExitObserved{Code: code, Reason: "test"})
				_ = write(processwire.KindExitObserved, exit)
				exited <- nil
			}()
		case processwire.KindStdinData:
			b.stdinData <- append([]byte(nil), frame.Payload...)
			if _, err := stdin.Write(frame.Payload); err != nil {
				return err
			}
		case processwire.KindStdinClose:
			b.stdinClose <- struct{}{}
			_ = stdin.Close()
		case processwire.KindStop:
			_ = cmd.Process.Kill()
		case processwire.KindWait:
		default:
			return fmt.Errorf("unexpected frame kind %d", frame.Kind)
		}
		ack, _ := processwire.Marshal(processwire.Ack{RequestSeq: frame.Seq})
		if err := write(processwire.KindAck, ack); err != nil {
			return err
		}
	}
}

type eventStream struct {
	events chan gen.Event
	errs   chan error
}

func streamEvents(r io.Reader) *eventStream {
	s := &eventStream{events: make(chan gen.Event, 64), errs: make(chan error, 1)}
	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
		for scanner.Scan() {
			var e gen.Event
			if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
				s.errs <- err
				return
			}
			s.events <- e
		}
		close(s.events)
	}()
	return s
}

func (s *eventStream) until(t *testing.T, kind gen.EventKind) []gen.EventKind {
	t.Helper()
	var kinds []gen.EventKind
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e, ok := <-s.events:
			if !ok {
				t.Fatalf("stream closed before %s; saw %v", kind, kinds)
			}
			kinds = append(kinds, e.Kind)
			if e.Kind == kind {
				return kinds
			}
		case err := <-s.errs:
			t.Fatal(err)
		case <-deadline:
			t.Fatalf("timeout waiting for %s; saw %v", kind, kinds)
		}
	}
}

func commandLine(t *testing.T, cmd gen.CommandCmd, payload any) []byte {
	t.Helper()
	p, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(gen.Command{V: 1, Cmd: cmd, Payload: p})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

// T25 (a)(b)(c) at the adapter boundary: in a running multiturn world session
// a message command becomes a stream-json user turn on the still-open
// container stdin; the next turn's native output is normalized with the
// existing message/tool_call kinds; the follow-up tool_use is registered with
// the world approval relay exactly like a first-turn intent; stdin is never
// closed; stop ends the session with a single stopped done.
func TestWorldProcessMultiturnInjectsFollowUpTurn(t *testing.T) {
	bins := buildAdapterBinaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	approvals := newFakeWorldApprovalBroker(t)
	defer approvals.Close()
	first, err := filepath.Abs(filepath.Join(fixtureDir, "01-simple-text.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := filepath.Abs(filepath.Join(fixtureDir, "02-single-tool.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	turnsOut := filepath.Join(t.TempDir(), "turns.ndjson")
	argv := ContainerArgvFor(bins.fake, "fixture replay", gen.SubagentSpawnPayloadSessionModeMultiturn)
	expected, _ := json.Marshal(argv[1:])
	broker := startMultiturnBroker(t, ctx, argv, []string{
		"HX_CLAUDE_MULTITURN_FIXTURES=" + first + "," + second,
		"HX_CLAUDE_EXPECT_ARGS=" + string(expected),
		"HX_CLAUDE_TURNS_OUT=" + turnsOut,
	})

	input, commands := io.Pipe()
	defer commands.Close()
	outR, outW := io.Pipe()
	stream := streamEvents(outR)
	runErr := make(chan error, 1)
	var stderr bytes.Buffer
	go func() {
		err := Run(ctx, input, outW, &stderr, Config{
			ClaudeBin:        "not-a-host-executable",
			ProcessEndpoint:  world.NewProcessEndpoint("unix", broker.listener.Addr().String(), "lease", "control", "output"),
			ApprovalEndpoint: world.NewApprovalEndpoint("unix", approvals.listener.Addr().String(), "capability"),
			WorldSpanID:      "2222222222222222",
			SessionMode:      gen.SubagentSpawnPayloadSessionModeMultiturn,
		})
		_ = outW.CloseWithError(err)
		runErr <- err
	}()
	if _, err := commands.Write(taskCommandLine(t, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	turn1 := stream.until(t, gen.EventKindSubagentUsage)
	if want := []gen.EventKind{gen.EventKindSubagentReady, gen.EventKindSubagentMessage, gen.EventKindSubagentUsage}; !reflect.DeepEqual(turn1, want) {
		t.Fatalf("turn1=%v want=%v", turn1, want)
	}
	select {
	case intent := <-approvals.intents:
		t.Fatalf("turn 1 registered an unexpected intent: %+v", intent)
	default:
	}
	if _, err := commands.Write(commandLine(t, gen.CommandCmdMessage, gen.MessagePayload{Text: "둘째 턴"})); err != nil {
		t.Fatal(err)
	}
	turn2 := stream.until(t, gen.EventKindSubagentUsage)
	if want := []gen.EventKind{gen.EventKindSubagentToolCall, gen.EventKindSubagentToolResult, gen.EventKindSubagentMessage, gen.EventKindSubagentUsage}; !reflect.DeepEqual(turn2, want) {
		t.Fatalf("turn2=%v want=%v", turn2, want)
	}
	select {
	case intent := <-approvals.intents:
		if intent.CallID != "toolu_01Nn8KS7Rke53sMr4xJMyFCc" || intent.Name != "Glob" {
			t.Fatalf("follow-up intent: %+v", intent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow-up turn tool intent never reached the approval relay")
	}
	if _, err := commands.Write(stopCommandLine(t)); err != nil {
		t.Fatal(err)
	}
	var last gen.Event
	for e := range stream.events {
		last = e
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run: %v; stderr=%s", err, stderr.String())
	}
	if last.Kind != gen.EventKindSubagentDone {
		t.Fatalf("last=%s", last.Kind)
	}
	var done gen.DonePayload
	if err := json.Unmarshal(last.Payload, &done); err != nil {
		t.Fatal(err)
	}
	if done.Status != gen.DonePayloadStatusStopped {
		t.Fatalf("done=%+v", done)
	}
	select {
	case <-broker.stdinClose:
		t.Fatal("multiturn session closed container stdin")
	default:
	}
	turns, err := os.ReadFile(turnsOut)
	if err != nil {
		t.Fatal(err)
	}
	if string(turns) != "\"fixture replay\"\n\"둘째 턴\"\n" {
		t.Fatalf("stdin turns=%q", turns)
	}
	if err := <-broker.finished; err != nil {
		t.Fatalf("broker: %v", err)
	}
}
