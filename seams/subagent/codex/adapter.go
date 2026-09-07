package codex

// Run is the small process boundary for the Codex JSON adapter. The native
// stream is read from stdin after the HX task command; this keeps the parser
// deterministic and leaves process/world wiring to the surrounding seam.
import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

func Run(ctx context.Context, in io.Reader, out, stderr io.Writer, mode policy.ApprovalMode) error {
	_ = ctx
	vals, err := validate.New()
	if err != nil {
		return err
	}
	s := bufio.NewScanner(in)
	if !s.Scan() {
		return fmt.Errorf("codex: task missing")
	}
	var cmd gen.Command
	if err := json.Unmarshal(s.Bytes(), &cmd); err != nil || cmd.Cmd != gen.CommandCmdTask {
		return fmt.Errorf("codex: first command must be task")
	}
	p := NewParser(mode)
	for s.Scan() {
		events, err := p.ParseLine(s.Bytes())
		if err != nil {
			fmt.Fprintln(stderr, err)
			if p.Ready() {
				emitDone(out, vals, gen.DonePayloadStatusError, err.Error())
			}
			return err
		}
		for _, e := range events {
			if err := emitValidated(out, vals, e); err != nil {
				return err
			}
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	events, err := p.Finish()
	if err != nil {
		return err
	}
	for _, e := range events {
		if err := emitValidated(out, vals, e); err != nil {
			return err
		}
	}
	return nil
}

func emitValidated(out io.Writer, vals *validate.Validators, e Event) error {
	record := gen.Event{V: 1, Kind: e.Kind, Payload: e.Payload, Raw: base64.StdEncoding.EncodeToString(e.Raw)}
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := vals.ValidateEvent(line); err != nil {
		return fmt.Errorf("codex: 발신 이벤트 계약 위반: %w", err)
	}
	_, err = fmt.Fprintf(out, "%s\n", line)
	return err
}
func emitDone(out io.Writer, vals *validate.Validators, status gen.DonePayloadStatus, result string) {
	b, _ := json.Marshal(gen.DonePayload{Status: status, Result: result})
	_ = emitValidated(out, vals, Event{Kind: gen.EventKindSubagentDone, Payload: b})
}
