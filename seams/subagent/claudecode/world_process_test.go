package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/core/world/processwire"
)

// A test-only process broker launches fakeclaude with the container PID1 argv.
// It accepts no StdinData frame, and fakeclaude independently checks zero-byte
// EOF and the exact argv before replaying the recorded native stream.
func TestWorldProcessFakeClaudeUsesArgvAndEmptyStdin(t *testing.T) {
	bins := buildAdapterBinaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "hx-t24-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "process.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	finished := make(chan error, 1)
	release := make(chan struct{})
	defer close(release)
	fixture, err := filepath.Abs(filepath.Join(fixtureDir, "01-simple-text.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	argv := ContainerArgv(bins.fake, "fixture replay")
	expected, _ := json.Marshal(argv[1:])
	go func() {
		finished <- func() error {
			output, err := listener.Accept()
			if err != nil {
				return err
			}
			defer output.Close()
			_ = output.SetDeadline(time.Now().Add(15 * time.Second))
			out := processwire.NewEncoder(output)
			if _, err := processwire.NewDecoder(output).Read(); err != nil {
				return err
			}
			if _, err := out.Write(processwire.KindHelloAck, processwire.StreamControl, 0, nil); err != nil {
				return err
			}
			control, err := listener.Accept()
			if err != nil {
				return err
			}
			defer control.Close()
			_ = control.SetDeadline(time.Now().Add(15 * time.Second))
			dec, enc := processwire.NewDecoder(control), processwire.NewEncoder(control)
			if _, err := dec.Read(); err != nil {
				return err
			}
			if _, err := enc.Write(processwire.KindHelloAck, processwire.StreamControl, 0, nil); err != nil {
				return err
			}
			var native bytes.Buffer
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Env = append(os.Environ(), "HX_CLAUDE_FIXTURE="+fixture, "HX_CLAUDE_EXPECT_ARGS="+string(expected))
			cmd.Stdout = &native
			var diagnostic bytes.Buffer
			cmd.Stderr = &diagnostic
			stdin, err := cmd.StdinPipe()
			if err != nil {
				return err
			}
			defer stdin.Close()
			for _, want := range []processwire.Kind{processwire.KindStart, processwire.KindStdinClose, processwire.KindWait} {
				frame, err := dec.Read()
				if err != nil {
					return err
				}
				if frame.Kind != want || len(frame.Payload) != 0 {
					return fmt.Errorf("frame=%d payload=%q want=%d with zero bytes", frame.Kind, frame.Payload, want)
				}
				switch want {
				case processwire.KindStart:
					if err := cmd.Start(); err != nil {
						return err
					}
					defer cmd.Process.Kill()
				case processwire.KindStdinClose:
					if err := stdin.Close(); err != nil {
						return err
					}
				case processwire.KindWait:
					if err := cmd.Wait(); err != nil {
						return fmt.Errorf("fakeclaude: %w: %s", err, diagnostic.String())
					}
				}
				ack, _ := processwire.Marshal(processwire.Ack{RequestSeq: frame.Seq})
				if _, err := enc.Write(processwire.KindAck, processwire.StreamControl, 0, ack); err != nil {
					return err
				}
			}
			for native.Len() > 0 {
				if _, err := out.Write(processwire.KindStdoutData, processwire.StreamStdout, 0, native.Next(processwire.MaxPayload)); err != nil {
					return err
				}
			}
			if _, err := out.Write(processwire.KindStreamEnd, processwire.StreamControl, 0, nil); err != nil {
				return err
			}
			exit, _ := processwire.Marshal(processwire.ExitObserved{Code: 0})
			if _, err := enc.Write(processwire.KindExitObserved, processwire.StreamControl, 0, exit); err != nil {
				return err
			}
			<-release
			return nil
		}()
	}()
	input, writer := io.Pipe()
	defer writer.Close()
	taskLine := taskCommandLine(t, t.TempDir())
	go func() { _, _ = writer.Write(taskLine) }()
	var output, stderr bytes.Buffer
	err = Run(ctx, input, &output, &stderr, Config{
		ClaudeBin: "not-a-host-executable", ProcessEndpoint: world.NewProcessEndpoint("unix", listener.Addr().String(), "lease", "control", "output"), WorldSpanID: "2222222222222222",
	})
	if err != nil {
		select {
		case brokerErr := <-finished:
			t.Fatalf("Run: %v; broker: %v", err, brokerErr)
		default:
			t.Fatalf("Run: %v; stderr: %s", err, stderr.String())
		}
	}
	var last gen.Event
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		if err := json.Unmarshal(line, &last); err != nil {
			t.Fatal(err)
		}
	}
	if last.Kind != gen.EventKindSubagentDone {
		t.Fatalf("last event=%s", last.Kind)
	}
	var done gen.DonePayload
	if err := json.Unmarshal(last.Payload, &done); err != nil {
		t.Fatal(err)
	}
	if done.Status != gen.DonePayloadStatusOk {
		t.Fatalf("done=%+v", done)
	}
}
