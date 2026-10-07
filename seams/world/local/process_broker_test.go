package local

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/quick"
	"time"

	"github.com/Eastsidegunn/JANUS/core/world/processwire"
)

type fakeStartedCommand struct {
	stdinR, stdoutW, stderrW *os.File
	stdinW, stdoutR, stderrR *os.File
	done                     chan struct{}
	mu                       sync.Mutex
	exitErr                  error
	finishOnce               sync.Once
	closeOnce                sync.Once
}

func newFakeStartedCommand(t *testing.T) *fakeStartedCommand {
	t.Helper()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return &fakeStartedCommand{stdinR: stdinR, stdinW: stdinW, stdoutR: stdoutR, stdoutW: stdoutW, stderrR: stderrR, stderrW: stderrW, done: make(chan struct{})}
}
func (p *fakeStartedCommand) Stdin() io.WriteCloser { return p.stdinW }
func (p *fakeStartedCommand) Stdout() io.ReadCloser { return p.stdoutR }
func (p *fakeStartedCommand) Stderr() io.ReadCloser { return p.stderrR }
func (p *fakeStartedCommand) Done() <-chan struct{} { return p.done }
func (p *fakeStartedCommand) ExitErr() error        { p.mu.Lock(); defer p.mu.Unlock(); return p.exitErr }
func (p *fakeStartedCommand) Kill()                 { p.finish(errors.New("killed")); p.closeWriters() }
func (p *fakeStartedCommand) ClosePipes() {
	p.closeOnce.Do(func() { _ = p.stdinW.Close(); _ = p.stdoutR.Close(); _ = p.stderrR.Close(); _ = p.stdinR.Close() })
}
func (p *fakeStartedCommand) finish(err error) {
	p.finishOnce.Do(func() { p.mu.Lock(); p.exitErr = err; p.mu.Unlock(); close(p.done) })
}
func (p *fakeStartedCommand) completeWait(code string) {
	_, _ = io.WriteString(p.stdoutW, code+"\n")
	p.closeWriters()
	p.finish(nil)
}
func (p *fakeStartedCommand) closeWriters() { _ = p.stdoutW.Close(); _ = p.stderrW.Close() }

type fakeProcessRuntime struct {
	mu             sync.Mutex
	waiter, attach *fakeStartedCommand
	starts         [][]string
	runs           [][]string
	onStop         func()
}

func (r *fakeProcessRuntime) Start(_ context.Context, args ...string) (startedCommand, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "wait" {
		return r.waiter, nil
	}
	return r.attach, nil
}
func (r *fakeProcessRuntime) Run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.runs = append(r.runs, append([]string(nil), args...))
	hook := r.onStop
	r.mu.Unlock()
	if len(args) > 0 && args[0] == "stop" && hook != nil {
		hook()
	}
	return nil, nil
}
func (r *fakeProcessRuntime) counts() (waitStarts, attachStarts, stops, kills int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.starts {
		if len(a) > 0 && a[0] == "wait" {
			waitStarts++
		} else if len(a) > 0 && a[0] == "start" {
			attachStarts++
		}
	}
	for _, a := range r.runs {
		if len(a) > 0 && a[0] == "stop" {
			stops++
		}
		if len(a) > 0 && a[0] == "kill" {
			kills++
		}
	}
	return
}
func (r *fakeProcessRuntime) hasRun(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.runs {
		if len(a) > 0 && a[0] == name {
			return true
		}
	}
	return false
}
func (r *fakeProcessRuntime) startSnapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.starts))
	for i := range r.starts {
		out[i] = append([]string(nil), r.starts[i]...)
	}
	return out
}

type processClient struct {
	control    net.Conn
	output     net.Conn
	controlEnc *processwire.Encoder
	controlDec *processwire.Decoder
	outputDec  *processwire.Decoder
}

func connectProcessClient(t *testing.T, b *processBroker) *processClient {
	t.Helper()
	ep := b.Endpoint()
	out, err := net.Dial(ep.Network(), ep.Address())
	if err != nil {
		t.Fatal(err)
	}
	outEnc := processwire.NewEncoder(out)
	outDec := processwire.NewDecoder(out)
	helloOut, _ := processwire.Marshal(processwire.Hello{Version: processwire.Version, Role: processwire.RoleOutput, LeaseID: b.leaseID, SpanID: b.spanID, Capability: ep.OutputCapability()})
	if _, err = outEnc.Write(processwire.KindHello, processwire.StreamControl, 0, helloOut); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, out, outDec, "output hello"); f.Kind != processwire.KindHelloAck {
		t.Fatalf("output hello kind=%d", f.Kind)
	}
	control, err := net.Dial(ep.Network(), ep.Address())
	if err != nil {
		t.Fatal(err)
	}
	controlEnc := processwire.NewEncoder(control)
	controlDec := processwire.NewDecoder(control)
	helloControl, _ := processwire.Marshal(processwire.Hello{Version: processwire.Version, Role: processwire.RoleControl, LeaseID: b.leaseID, SpanID: b.spanID, Capability: ep.ControlCapability()})
	if _, err = controlEnc.Write(processwire.KindHello, processwire.StreamControl, 0, helloControl); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, control, controlDec, "control hello"); f.Kind != processwire.KindHelloAck {
		t.Fatalf("control hello kind=%d", f.Kind)
	}
	return &processClient{control: control, output: out, controlEnc: controlEnc, controlDec: controlDec, outputDec: outDec}
}
func (c *processClient) close() { _ = c.control.Close(); _ = c.output.Close() }
func (c *processClient) send(t *testing.T, kind processwire.Kind, payload []byte) {
	t.Helper()
	if _, err := c.controlEnc.Write(kind, processwire.StreamControl, 0, payload); err != nil {
		t.Fatal(err)
	}
}
func (c *processClient) ack(t *testing.T, what string) {
	t.Helper()
	f := readFrame(t, c.control, c.controlDec, what)
	if f.Kind != processwire.KindAck {
		t.Fatalf("%s kind=%d payload=%s", what, f.Kind, f.Payload)
	}
}

func TestProcessBrokerExitObservationIsIndependentOfOutputEOF(t *testing.T) {
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	// This models a container that has already exited before the wait command's
	// observer goroutine runs. No --rm or early remove may erase exit status.
	waiter.completeWait("7")
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start ack")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait ack")
	exitFrame := readFrame(t, client.control, client.controlDec, "exit_observed")
	if exitFrame.Kind != processwire.KindExitObserved {
		t.Fatalf("kind=%d", exitFrame.Kind)
	}
	var exit processwire.ExitObserved
	if err := processwire.Unmarshal(exitFrame.Payload, &exit); err != nil {
		t.Fatal(err)
	}
	if exit.Code != 7 {
		t.Fatalf("authoritative exit=%d", exit.Code)
	}
	_ = client.output.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	if _, err := client.outputDec.Read(); err == nil {
		t.Fatal("descendant-held output EOF 전에 stream_end가 도착함")
	}
	_ = client.output.SetReadDeadline(time.Time{})
	_, _ = io.WriteString(attach.stdoutW, "late-output")
	attach.closeWriters()
	attach.finish(nil)
	var output []byte
	for {
		f := readFrame(t, client.output, client.outputDec, "output drain")
		if f.Kind == processwire.KindStreamEnd {
			break
		}
		output = append(output, f.Payload...)
	}
	if string(output) != "late-output" {
		t.Fatalf("drained output=%q", output)
	}
	waitStarts, attachStarts, _, _ := runtime.counts()
	if waitStarts != 1 || attachStarts != 1 {
		t.Fatalf("starts wait=%d attach=%d", waitStarts, attachStarts)
	}
	starts := runtime.startSnapshot()
	if strings.Join(starts[0], " ") != "wait --condition exited "+fakeAgentID || strings.Join(starts[1], " ") != "start --attach --interactive --sig-proxy=false "+fakeAgentID {
		t.Fatalf("process lifecycle commands=%v", starts)
	}
	if runtime.hasRun("rm") {
		t.Fatal("exit 상태 관측 전에 container rm이 실행됨")
	}
	shutdownBroker(t, b)
	if _, err := attach.Stdin().Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed stdin err=%v", err)
	}
	if _, err := attach.Stdout().Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed stdout err=%v", err)
	}
}

func TestProcessBrokerRedactsSecretFromOutputFrames(t *testing.T) {
	secret := []byte("synthetic-oauth-token")
	input := []byte(`{"result":"synthetic-oauth-token", "stderr":"synthetic-oauth-token"}`)
	got := redactPayload(input, secret)
	if bytes.Contains(got, secret) || !bytes.Contains(got, []byte("<redacted>")) {
		t.Fatalf("output frame secret redaction 실패: %q", got)
	}
	b := &processBroker{redaction: secret, redactionTail: make(map[processwire.Stream][]byte)}
	part1 := b.redactChunk(processwire.StreamStdout, []byte(`{"result":"synthetic-oauth-`))
	part2 := b.redactChunk(processwire.StreamStdout, []byte(`token"}`))
	tail := b.redactionTail[processwire.StreamStdout]
	joined := append(append(append([]byte(nil), part1...), part2...), redactPayloads(tail, b.redactionValues())...)
	if bytes.Contains(joined, secret) || !bytes.Contains(joined, []byte("<redacted>")) {
		t.Fatalf("분할 output frame secret redaction 실패: %q", joined)
	}
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	b.outputMu = sync.Mutex{}
	b.streamEnded = false
	encoded := processwire.NewEncoder(server)
	read := make(chan processwire.Frame, 1)
	go func() {
		frame, err := processwire.NewDecoder(client).Read()
		if err == nil {
			read <- frame
		}
	}()
	if err := b.writeOutput(encoded, processwire.KindStdoutData, processwire.StreamStdout, input); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-read:
		if bytes.Contains(frame.Payload, secret) || !bytes.Contains(frame.Payload, []byte("<redacted>")) {
			t.Fatalf("writeOutput 경로에서 secret이 노출됨: %q", frame.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("redacted output frame 대기 시간 초과")
	}
}

func TestProcessBrokerRedactionDoesNotDuplicateSecretFreeChunks(t *testing.T) {
	secret := "gateway-access-sentinel"
	b := &processBroker{
		redactions:      normalizeRedactions([]string{secret}),
		redactionMaxLen: len(secret),
		redactionTail:   make(map[processwire.Stream][]byte),
	}
	chunks := [][]byte{
		[]byte("{\"type\":\"assistant\",\"text\":\"hello world\"}\n"),
		[]byte("{\"type\":\"result\"}\n"),
	}
	var got []byte
	for _, chunk := range chunks {
		got = append(got, b.redactChunk(processwire.StreamStdout, chunk)...)
	}
	got = append(got, redactPayloads(b.redactionTail[processwire.StreamStdout], b.redactionValues())...)
	want := bytes.Join(chunks, nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("secret-free chunks changed or duplicated: got=%q want=%q", got, want)
	}
}

func TestProcessBrokerRedactsMultipleSecretsAcrossChunksAndPrefixes(t *testing.T) {
	longSecret := "gateway-access-long"
	shortSecret := "gateway-access"
	b := &processBroker{
		redactions:      normalizeRedactions([]string{shortSecret, longSecret}),
		redactionTail:   make(map[processwire.Stream][]byte),
		redactionMaxLen: len(longSecret),
	}
	var got []byte
	for _, chunk := range [][]byte{
		[]byte("before gateway-access-"),
		[]byte("long and "),
		[]byte("gateway-"),
		[]byte("access after"),
	} {
		got = append(got, b.redactChunk(processwire.StreamStdout, chunk)...)
	}
	got = append(got, redactPayloads(b.redactionTail[processwire.StreamStdout], b.redactionValues())...)
	if bytes.Contains(got, []byte(longSecret)) || bytes.Contains(got, []byte(shortSecret)) {
		t.Fatalf("multiple secret leaked across chunks: %q", got)
	}
	if bytes.Count(got, []byte("<redacted>")) != 2 {
		t.Fatalf("redaction count=%d output=%q", bytes.Count(got, []byte("<redacted>")), got)
	}
}

func TestProcessBrokerRedactionNonPrefixOverlapAcrossChunks(t *testing.T) {
	secrets := []string{"ccacbb", "abbbca", "accab"}
	text := []byte("prefix-ccacbbaccabbbcaY")
	b := &processBroker{
		redactions:      normalizeRedactions(secrets),
		redactionMaxLen: len("ccacbb"),
		redactionTail:   make(map[processwire.Stream][]byte),
	}
	var got []byte
	for i := 0; i < len(text); {
		n := 1 + (i % 5)
		if i+n > len(text) {
			n = len(text) - i
		}
		got = append(got, b.redactChunk(processwire.StreamStdout, text[i:i+n])...)
		i += n
	}
	got = append(got, redactPayloads(b.redactionTail[processwire.StreamStdout], b.redactionValues())...)
	want := []byte("prefix-<redacted>Y")
	if !bytes.Equal(got, want) {
		t.Fatalf("non-prefix overlap crossed chunk boundary: got=%q want=%q", got, want)
	}
}

func TestProcessBrokerRedactionTailHasBoundedOverlapChain(t *testing.T) {
	b := &processBroker{
		redactions:      normalizeRedactions([]string{"aaaaaaaa", "aaaaaaa"}),
		redactionMaxLen: len("aaaaaaaa"),
		redactionTail:   make(map[processwire.Stream][]byte),
	}
	chunk := bytes.Repeat([]byte("a"), 4096)
	var got []byte
	for i := 0; i < 32; i++ {
		got = append(got, b.redactChunk(processwire.StreamStdout, chunk)...)
		if got := len(b.redactionTail[processwire.StreamStdout]); got > maxRedactionTailBytes {
			t.Fatalf("overlap chain retained %d bytes, cap=%d", got, maxRedactionTailBytes)
		}
	}
	got = append(got, redactIntervalsCovered(b.redactionTail[processwire.StreamStdout], b.redactionValues(), len(b.redactionTail[processwire.StreamStdout]), b.redactionCovered[processwire.StreamStdout])...)
	for _, secret := range b.redactionValues() {
		for n := 4; n <= len(secret); n++ {
			for i := 0; i+n <= len(secret); i++ {
				if bytes.Contains(got, secret[i:i+n]) {
					t.Fatalf("overlap chain leaked secret substring len=%d: %q", n, got)
				}
			}
		}
	}
}

func TestProcessBrokerRedactionTailBoundedFlushHasNoSecretSubstrings(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		input   []byte
		sizes   []int
	}{
		{name: "repeated-key", secrets: []string{"Kq7#vX9pLm2Z"}, input: append(bytes.Repeat([]byte("Kq7#vX9pLm2Z"), 6000), []byte("|done")...), sizes: []int{6144, 4096, 7, 67200}},
		{name: "aaaa-qr", secrets: []string{"aaaaaaaa", "aaaaaaQR"}, input: append(bytes.Repeat([]byte("a"), 70000), []byte("QR")...), sizes: []int{6144}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range tc.sizes {
				b := &processBroker{
					redactions:    normalizeRedactions(tc.secrets),
					redactionTail: make(map[processwire.Stream][]byte),
				}
				var got []byte
				for i := 0; i < len(tc.input); {
					n := size
					if n > len(tc.input)-i {
						n = len(tc.input) - i
					}
					got = append(got, b.redactChunk(processwire.StreamStdout, tc.input[i:i+n])...)
					i += n
				}
				got = append(got, redactIntervalsCovered(b.redactionTail[processwire.StreamStdout], b.redactionValues(), len(b.redactionTail[processwire.StreamStdout]), b.redactionCovered[processwire.StreamStdout])...)
				for _, secret := range tc.secrets {
					for n := 4; n <= len(secret); n++ {
						for i := 0; i+n <= len(secret); i++ {
							if bytes.Contains(got, []byte(secret[i:i+n])) {
								t.Fatalf("chunk=%d leaked secret substring len=%d", size, n)
							}
						}
					}
				}
				want := redactPayloads(tc.input, b.redactionValues())
				fold := func(v []byte) []byte {
					for bytes.Contains(v, []byte("<redacted><redacted>")) {
						v = bytes.ReplaceAll(v, []byte("<redacted><redacted>"), []byte("<redacted>"))
					}
					return v
				}
				if !bytes.Equal(fold(got), fold(want)) {
					t.Fatalf("chunk=%d output differs after marker folding: got len=%d want len=%d", size, len(got), len(want))
				}
			}
		})
	}
}

func TestProcessBrokerRedactionPropertyRandomSecretsAndChunks(t *testing.T) {
	property := func(seed []byte) bool {
		if len(seed) < 16 {
			return true
		}
		toSecret := func(offset, n int) []byte {
			out := make([]byte, n)
			for i := range out {
				out[i] = 'a' + seed[(offset+i)%len(seed)]%26
			}
			return out
		}
		first := toSecret(0, 5+int(seed[0]%3))
		second := append([]byte(nil), first[2:]...)
		second = append(second, toSecret(7, 3+int(seed[1]%3))...)
		third := toSecret(9, 6+int(seed[2]%4))
		prefix := append([]byte(nil), first[:2]...)
		secrets := [][]byte{first, second, third}
		text := append([]byte("prefix:"), first[:2]...)
		text = append(text, second...)
		text = append(text, []byte("|plain|")...)
		text = append(text, third[:2]...)
		text = append(text, third[2:]...)
		text = append(text, []byte("|middle|")...)
		text = append(text, prefix...)
		text = append(text, []byte("tail")...)
		secretStrings := make([]string, 0, len(secrets)+1)
		for _, secret := range append(secrets, prefix) {
			secretStrings = append(secretStrings, string(secret))
		}
		b := &processBroker{redactions: normalizeRedactions(secretStrings), redactionTail: make(map[processwire.Stream][]byte)}
		var got []byte
		for i := 0; i < len(text); {
			n := 1 + int(seed[i%len(seed)]%byte(minInt(9, len(text)-i)))
			got = append(got, b.redactChunk(processwire.StreamStdout, text[i:i+n])...)
			i += n
		}
		got = append(got, redactPayloads(b.redactionTail[processwire.StreamStdout], b.redactionValues())...)
		want := redactPayloads(text, b.redactionValues())
		if !bytes.Equal(got, want) {
			return false
		}
		// Also generate a single overlapping chain beyond the tail bound. This
		// keeps the long-chain generation in the property while avoiding a
		// quadratic number of tiny chunks in every case.
		longChain := bytes.Repeat(first, maxRedactionTailBytes/len(first)+2)
		chainBroker := &processBroker{
			redactions:      normalizeRedactions([]string{string(first)}),
			redactionMaxLen: len(first),
			redactionTail:   make(map[processwire.Stream][]byte),
		}
		chainGot := chainBroker.redactChunk(processwire.StreamStdout, longChain)
		chainGot = append(chainGot, redactIntervalsCovered(chainBroker.redactionTail[processwire.StreamStdout], chainBroker.redactionValues(), len(chainBroker.redactionTail[processwire.StreamStdout]), chainBroker.redactionCovered[processwire.StreamStdout])...)
		if bytes.Contains(chainGot, first) || len(chainBroker.redactionTail[processwire.StreamStdout]) > maxRedactionTailBytes {
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 512}); err != nil {
		t.Fatal(err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestProcessBrokerStopIsIdempotentAndLateStopDoesNotSignalCompletedCID(t *testing.T) {
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	b := mustProcessBroker(t, context.Background(), runtime)
	// Force exit observation to finish while the stop handler still owns the
	// request. ExitObserved must not overtake the stop ACK.
	runtime.onStop = func() {
		waiter.completeWait("143")
		<-b.containerDone
		attach.closeWriters()
		attach.finish(errors.New("attach stopped"))
	}
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	stopPayload, _ := processwire.Marshal(processwire.Stop{Reason: "test stop"})
	client.send(t, processwire.KindStop, stopPayload)
	client.ack(t, "stop")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	if f := readFrame(t, client.control, client.controlDec, "exit"); f.Kind != processwire.KindExitObserved {
		t.Fatalf("exit kind=%d", f.Kind)
	}
	client.send(t, processwire.KindStop, stopPayload)
	client.ack(t, "late stop")
	_, _, stops, kills := runtime.counts()
	if stops != 1 || kills != 0 {
		t.Fatalf("late stop re-signaled CID: stop=%d kill=%d", stops, kills)
	}
	for {
		if f := readFrame(t, client.output, client.outputDec, "stream end"); f.Kind == processwire.KindStreamEnd {
			break
		}
	}
	shutdownBroker(t, b)
}

func TestProcessBrokerExitObservationCannotOvertakeWaitAck(t *testing.T) {
	waiter := newFakeStartedCommand(t)
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	b := &processBroker{
		controlEncoder: processwire.NewEncoder(server),
		waitRequested:  true,
		containerDone:  make(chan struct{}),
	}
	waiter.completeWait("0")
	b.wg.Add(1)
	go b.observeWait(waiter)
	select {
	case <-b.containerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("exit observation timeout")
	}

	decoder := processwire.NewDecoder(client)
	_ = client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := decoder.Read(); err == nil {
		t.Fatal("wait ACK 전 exit frame이 노출됨")
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("wait ACK 전 read err=%v", err)
	}
	_ = client.SetReadDeadline(time.Time{})

	b.mu.Lock()
	b.waitAcked = true
	b.mu.Unlock()
	sent := make(chan error, 1)
	go func() { sent <- b.sendExit() }()
	if frame := readFrame(t, client, decoder, "acked exit"); frame.Kind != processwire.KindExitObserved {
		t.Fatalf("acked exit kind=%d payload=%s", frame.Kind, frame.Payload)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	b.wg.Wait()
}

func TestProcessBrokerOutputBackpressureReachesContainerPipeWithoutLoss(t *testing.T) {
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	want := bytes.Repeat([]byte("b"), 8<<20)
	written := make(chan error, 1)
	go func() {
		data := want
		for len(data) > 0 {
			n, err := attach.stdoutW.Write(data)
			if err != nil {
				written <- err
				return
			}
			if n == 0 {
				written <- io.ErrShortWrite
				return
			}
			data = data[n:]
		}
		written <- nil
	}()
	select {
	case err := <-written:
		t.Fatalf("output consumer 정지 전에 container write가 완료됨: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	got := make([]byte, 0, len(want))
	for len(got) < len(want) {
		f := readFrame(t, client.output, client.outputDec, "backpressure drain")
		if f.Kind != processwire.KindStdoutData {
			t.Fatalf("drain kind=%d", f.Kind)
		}
		got = append(got, f.Payload...)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer 재개 뒤 container write timeout")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("backpressure drain bytes=%d want=%d", len(got), len(want))
	}
	waiter.completeWait("0")
	attach.closeWriters()
	attach.finish(nil)
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	_ = readFrame(t, client.control, client.controlDec, "exit")
	for {
		if f := readFrame(t, client.output, client.outputDec, "stream end"); f.Kind == processwire.KindStreamEnd {
			break
		}
	}
	shutdownBroker(t, b)
}

func TestProcessBrokerResourceExhaustionIsFatalBeforeContainerStart(t *testing.T) {
	runtime := &fakeProcessRuntime{waiter: newFakeStartedCommand(t), attach: newFakeStartedCommand(t)}
	b := mustProcessBroker(t, context.Background(), runtime)
	conn, err := net.Dial(b.Endpoint().Network(), b.Endpoint().Address())
	if err != nil {
		t.Fatal(err)
	}
	enc := processwire.NewEncoder(conn)
	dec := processwire.NewDecoder(conn)
	hello, _ := processwire.Marshal(processwire.Hello{Version: processwire.Version, Role: processwire.RoleControl, LeaseID: b.leaseID, SpanID: b.spanID, Capability: b.Endpoint().ControlCapability()})
	_, _ = enc.Write(processwire.KindHello, processwire.StreamControl, 0, hello)
	_ = readFrame(t, conn, dec, "hello")
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(processwire.MaxFrameBytes+1))
	_, _ = conn.Write(prefix[:])
	err = waitBrokerError(t, b)
	if !errors.Is(err, ErrProcessBrokerFatal) || !errors.Is(err, processwire.ErrResourceExhausted) {
		t.Fatalf("fatal err=%v", err)
	}
	waitStarts, attachStarts, stops, kills := runtime.counts()
	if waitStarts+attachStarts+stops+kills != 0 {
		t.Fatalf("exhaustion side effects: %d %d %d %d", waitStarts, attachStarts, stops, kills)
	}
	_ = conn.Close()
	shutdownBrokerAllowError(t, b)
}

func TestProcessBrokerParentCancellationStopsAndWaits(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	runtime.onStop = func() {
		waiter.completeWait("137")
		attach.closeWriters()
		attach.finish(errors.New("attach canceled"))
	}
	b := mustProcessBroker(t, parent, runtime)
	client := connectProcessClient(t, b)
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	cancel()
	err := waitBrokerError(t, b)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel fatal=%v", err)
	}
	select {
	case <-b.containerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel 뒤 authoritative wait timeout")
	}
	_, _, stops, _ := runtime.counts()
	if stops != 1 {
		t.Fatalf("cancel stop calls=%d", stops)
	}
	client.close()
	shutdownBrokerAllowError(t, b)
}

func TestProcessBrokerShutdownLabelsContainerWaitStage(t *testing.T) {
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := b.Shutdown(ctx)
	if err == nil {
		t.Fatal("shutdown with stalled lifecycle unexpectedly succeeded")
	}
	message := err.Error()
	if !strings.Contains(message, "container exit observation") {
		t.Fatalf("container wait stage missing from error: %v", err)
	}
	client.close()
}

func TestProcessBrokerShutdownLabelsStreamWaitStage(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "hxt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	listener, err := net.Listen("unix", filepath.Join(root, "process.sock"))
	if err != nil {
		t.Fatal(err)
	}
	b := &processBroker{
		ctx: context.Background(), cancel: func() {}, listener: listener, rootDir: root,
		started: true, containerDone: make(chan struct{}), streamDone: make(chan struct{}),
		done: make(chan struct{}), runner: &fakeProcessRuntime{},
	}
	close(b.containerDone)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = b.Shutdown(ctx)
	if err == nil || !strings.Contains(err.Error(), "output stream drain") {
		t.Fatalf("stream wait stage missing from error: %v", err)
	}
}

func TestWaitStartedCommandExitKillsStalledAttachAfterOutputDrain(t *testing.T) {
	attach := newFakeStartedCommand(t)
	attach.closeWriters()
	if err := waitStartedCommandExit(attach, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-attach.Done():
	default:
		t.Fatal("stalled attach was not killed")
	}
}

func TestProcessBrokerRejectsRoleReconnectBeforeContainerStart(t *testing.T) {
	runtime := &fakeProcessRuntime{waiter: newFakeStartedCommand(t), attach: newFakeStartedCommand(t)}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	defer client.close()
	conn, err := net.Dial(b.Endpoint().Network(), b.Endpoint().Address())
	if err != nil {
		t.Fatal(err)
	}
	enc := processwire.NewEncoder(conn)
	hello, _ := processwire.Marshal(processwire.Hello{Version: processwire.Version, Role: processwire.RoleOutput, LeaseID: b.leaseID, SpanID: b.spanID, Capability: b.Endpoint().OutputCapability()})
	_, _ = enc.Write(processwire.KindHello, processwire.StreamControl, 0, hello)
	err = waitBrokerError(t, b)
	if !errors.Is(err, ErrProcessBrokerFatal) || !errors.Is(err, processwire.ErrProtocol) {
		t.Fatalf("reconnect err=%v", err)
	}
	w, a, s, k := runtime.counts()
	if w+a+s+k != 0 {
		t.Fatalf("reconnect side effects=%d/%d/%d/%d", w, a, s, k)
	}
	_ = conn.Close()
	shutdownBrokerAllowError(t, b)
}

func TestProcessBrokerOutputDisconnectIsFatalBeforeContainerStart(t *testing.T) {
	runtime := &fakeProcessRuntime{waiter: newFakeStartedCommand(t), attach: newFakeStartedCommand(t)}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	_ = client.output.Close()
	err := waitBrokerError(t, b)
	if !errors.Is(err, ErrProcessBrokerFatal) {
		t.Fatalf("disconnect err=%v", err)
	}
	w, a, s, k := runtime.counts()
	if w+a+s+k != 0 {
		t.Fatalf("disconnect side effects=%d/%d/%d/%d", w, a, s, k)
	}
	_ = client.control.Close()
	shutdownBrokerAllowError(t, b)
}

func TestStopControlCloseIsExpectedOnlyAfterFinalExitAttempt(t *testing.T) {
	b := &processBroker{stopReason: "user stop"}
	if b.expectedStopControlGone(syscall.EPIPE) {
		t.Fatal("exit_observed 전 control close가 정상 종말로 분류됨")
	}
	b.exitSent = true
	if !b.expectedStopControlGone(syscall.EPIPE) {
		t.Fatal("최종 exit_observed 쓰기의 peer close가 정상 종말로 분류되지 않음")
	}
}

func TestConsumerGoneRequiresExplicitStop(t *testing.T) {
	b := &processBroker{outputPeerGone: true}
	if b.expectedConsumerGone() {
		t.Fatal("output peer 이탈만으로 정상 consumer-gone 종말을 허용함")
	}
	b.stopReason = "user stop"
	if !b.expectedConsumerGone() {
		t.Fatal("명시 stop 뒤 consumer-gone을 정상 종말로 분류하지 않음")
	}
}

func TestStopConsumerGoneRejectsTimeout(t *testing.T) {
	b := &processBroker{stopReason: "user stop"}
	timeout := &net.DNSError{Err: "synthetic timeout", IsTimeout: true}
	if b.expectedStopConsumerGone(timeout) {
		t.Fatal("timeout을 consumer-gone 정상 종말로 분류함")
	}
}

func TestProcessBrokerStopConsumerCloseIsExpectedTerminal(t *testing.T) {
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	runtime.onStop = func() {
		waiter.completeWait("143")
		attach.closeWriters()
	}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start ack")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait ack")
	stopPayload, err := processwire.Marshal(processwire.Stop{Reason: "user stop"})
	if err != nil {
		t.Fatal(err)
	}
	client.send(t, processwire.KindStop, stopPayload)
	client.ack(t, "stop ack")
	if f := readFrame(t, client.control, client.controlDec, "exit observed"); f.Kind != processwire.KindExitObserved {
		t.Fatalf("exit kind=%d", f.Kind)
	}
	// This is the adapter's explicit terminal close after it has consumed the
	// native stop result. Neither output nor the now-complete control plane may
	// become a fatal broker error.
	_ = client.output.Close()
	_ = client.control.Close()
	select {
	case <-b.streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("expected stop stream termination")
	}
	if err := b.Err(); err != nil {
		t.Fatalf("expected non-fatal stop consumer close, got %v", err)
	}
	shutdownBroker(t, b)
}

func TestProcessBrokerRepeatedUnusedLeaseDoesNotAccumulateGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for range 5 {
		r := &fakeProcessRuntime{waiter: newFakeStartedCommand(t), attach: newFakeStartedCommand(t)}
		b := mustProcessBroker(t, context.Background(), r)
		shutdownBroker(t, b)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+3 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+3 {
		t.Fatalf("goroutine baseline=%d after=%d", baseline, got)
	}
}

func TestDrainedPeerCloseIsExpectedOnlyAfterOutputDrain(t *testing.T) {
	b := &processBroker{}
	// (나) exit 전: 아직 forward되지 않은 출력을 유실시킬 수 있으므로 fatal 유지.
	if b.expectedDrainedPeerGone(io.EOF) {
		t.Fatal("exit 전 peer close가 정상 종말로 분류됨")
	}
	b.exitSent = true
	// (나) exit는 전송됐지만 출력 미완: post-exit라는 이유만으로 benign이면 안 됨.
	if b.expectedDrainedPeerGone(io.EOF) {
		t.Fatal("출력 drain 전 post-exit peer close가 정상 종말로 분류됨")
	}
	b.streamEndReached = true
	// (가) exit 전송 + 출력 완전 drain: consumer-gone-after-done 정상 종말.
	if !b.expectedDrainedPeerGone(io.EOF) {
		t.Fatal("출력 drain 완료 후 peer close가 정상 종말로 분류되지 않음")
	}
	if !b.expectedDrainedPeerGone(net.ErrClosed) || !b.expectedDrainedPeerGone(os.ErrClosed) {
		t.Fatal("closed transport error가 정상 종말로 분류되지 않음")
	}
	// timeout은 종말(close)이 아니라 지연이므로 fatal 유지.
	timeout := &net.DNSError{Err: "synthetic timeout", IsTimeout: true}
	if b.expectedDrainedPeerGone(timeout) {
		t.Fatal("timeout을 정상 종말로 분류함")
	}
	if b.expectedDrainedPeerGone(nil) {
		t.Fatal("nil err을 종말로 분류함")
	}
}

func TestProcessBrokerNaturalControlCloseAfterDrainIsExpected(t *testing.T) {
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	// 자연(비-stop) 종료: 출력을 낸 뒤 컨테이너가 stop 없이 종료한다.
	_, _ = io.WriteString(attach.stdoutW, "orphan-output")
	attach.closeWriters()
	attach.finish(nil)
	waiter.completeWait("0")
	if f := readFrame(t, client.control, client.controlDec, "exit observed"); f.Kind != processwire.KindExitObserved {
		t.Fatalf("exit kind=%d", f.Kind)
	}
	// 순서 고정: 출력을 stream-end까지 완전 drain한 뒤에만 control을 닫는다.
	// stream-end 프레임을 읽었다는 것은 broker가 streamEndReached를 이미
	// 세웠다는 뜻이므로(-count 반복에도 결정적) 아래 close는 항상 drain 이후다.
	var out []byte
	for {
		f := readFrame(t, client.output, client.outputDec, "orphan drain")
		if f.Kind == processwire.KindStreamEnd {
			break
		}
		out = append(out, f.Payload...)
	}
	if string(out) != "orphan-output" {
		t.Fatalf("drained output=%q", out)
	}
	// 어댑터가 stream-end를 소비한 직후 control을 닫는 경합. 자연 종료 경로에서도
	// consumer-gone-after-done 정상 종말이어야 한다(broker fatal 금지). 수정 전에는
	// streamEnded 플래그가 wire보다 늦게 세워져 이 close가 간헐 fatal이었다.
	_ = client.control.Close()
	select {
	case <-b.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("drain 뒤 세션이 완료로 종결되지 않음")
	}
	if err := b.Err(); err != nil {
		t.Fatalf("자연 종료 control close가 fatal로 분류됨: %v", err)
	}
	shutdownBroker(t, b)
}

// TestProcessBrokerControlEOFDuringStreamEndWriteWindow reproduces the exact CI
// flake state deterministically at the broker level: exit observed and sent,
// stop=false, but the broker's own streamEnded bookkeeping not yet flipped (the
// terminal frame is on the wire and the peer has consumed it and closed control,
// racing ahead of the post-write flag). This is the state from run
// a CI run attempt: (exit_sent=true stream_ended=false stop=false). The
// only discriminator is whether the output plane was fully drained beforehand
// (streamEndReached). It is deterministic under -count: the flags are fixed
// before control EOF is delivered, so the classification never depends on
// goroutine scheduling.
func TestProcessBrokerControlEOFDuringStreamEndWriteWindow(t *testing.T) {
	newBroker := func(t *testing.T) (*processBroker, func()) {
		t.Helper()
		root, err := os.MkdirTemp("/tmp", "hxt-")
		if err != nil {
			t.Fatal(err)
		}
		lis, err := net.Listen("unix", filepath.Join(root, "process.sock"))
		if err != nil {
			_ = os.RemoveAll(root)
			t.Fatal(err)
		}
		b := &processBroker{
			ctx: context.Background(), cancel: func() {}, listener: lis,
			runner: &fakeProcessRuntime{}, done: make(chan struct{}),
			activeStages: make(map[streamStage]time.Time), redactionTail: make(map[processwire.Stream][]byte),
		}
		return b, func() { _ = lis.Close(); _ = os.RemoveAll(root) }
	}
	run := func(t *testing.T, drained bool) error {
		b, cleanup := newBroker(t)
		defer cleanup()
		b.mu.Lock()
		b.started, b.waitRequested, b.waitAcked, b.exitSent = true, true, true, true
		// streamEnded is deliberately still false: the terminal frame reached the
		// peer but the broker has not yet run the post-write flag assignment.
		b.streamEnded = false
		b.streamEndReached = drained
		b.waitResult = &processwire.ExitObserved{Code: 0, Reason: "container exited"}
		b.mu.Unlock()
		server, client := net.Pipe()
		dec := processwire.NewDecoder(server)
		enc := processwire.NewEncoder(server)
		returned := make(chan struct{})
		go func() { b.handleControl(server, dec, enc); close(returned) }()
		// The adapter closes control the instant it consumes stream-end.
		_ = client.Close()
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
			t.Fatal("handleControl가 control EOF에서 반환하지 않음")
		}
		_ = server.Close()
		return b.Err()
	}
	// (가) 출력 완전 drain 후 control EOF: consumer-gone-after-done 정상 종말.
	if err := run(t, true); err != nil {
		t.Fatalf("drain 완료 상태의 control EOF가 fatal로 분류됨: %v", err)
	}
	// (나) 출력 미완(streamEndReached=false) 상태의 control EOF: 유실 위험이므로
	// exit가 전송됐더라도 반드시 fatal.
	if err := run(t, false); !errors.Is(err, ErrProcessBrokerFatal) {
		t.Fatalf("출력 미완 상태의 control EOF가 fatal이 아님: %v", err)
	}
}

func TestProcessBrokerNaturalControlCloseBeforeDrainStaysFatal(t *testing.T) {
	waiter, attach := newFakeStartedCommand(t), newFakeStartedCommand(t)
	runtime := &fakeProcessRuntime{waiter: waiter, attach: attach}
	b := mustProcessBroker(t, context.Background(), runtime)
	client := connectProcessClient(t, b)
	defer client.close()
	client.send(t, processwire.KindStart, nil)
	client.ack(t, "start")
	client.send(t, processwire.KindWait, nil)
	client.ack(t, "wait")
	// 컨테이너는 종료해 exit는 전송되지만, attach 출력 파이프는 열린 채로 둬
	// stream-end(출력 완전 drain)가 아직 오지 않은 (나) 상태를 결정적으로 고정한다.
	waiter.completeWait("0")
	if f := readFrame(t, client.control, client.controlDec, "exit observed"); f.Kind != processwire.KindExitObserved {
		t.Fatalf("exit kind=%d", f.Kind)
	}
	// 출력 미완 상태의 control close는 미forward 출력을 유실시킬 수 있으므로
	// post-exit라도 반드시 fatal이어야 한다.
	_ = client.control.Close()
	err := waitBrokerError(t, b)
	if !errors.Is(err, ErrProcessBrokerFatal) {
		t.Fatalf("출력 drain 전 control close가 fatal이 아님: %v", err)
	}
	_ = client.output.Close()
	shutdownBrokerAllowError(t, b)
}

func mustProcessBroker(t *testing.T, parent context.Context, r *fakeProcessRuntime) *processBroker {
	t.Helper()
	b, err := startProcessBroker(parent, strings.Repeat("2", 16), strings.Repeat("1", 64), fakeAgentID, r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func readFrame(t *testing.T, conn net.Conn, dec *processwire.Decoder, what string) processwire.Frame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	f, err := dec.Read()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return f
}
func shutdownBroker(t *testing.T, b *processBroker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
func shutdownBrokerAllowError(t *testing.T, b *processBroker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = b.Shutdown(ctx)
}
func waitBrokerError(t *testing.T, b *processBroker) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := b.Err(); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("broker error timeout")
	return nil
}
