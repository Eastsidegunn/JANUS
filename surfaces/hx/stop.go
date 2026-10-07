package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/Eastsidegunn/JANUS/seams/approvalrelay"
)

type stopRequest struct {
	OperationID   string `json:"operation_id"`
	StopID        string `json:"stop_id"`
	TraceID       string `json:"trace_id"`
	TargetSpanID  string `json:"target_span_id,omitempty"`
	Reason        string `json:"reason"`
	EvidenceSeq   int64  `json:"evidence_seq,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

func stopCmd(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "", "제어 relay socket")
	path := fs.String("request", "", "stop request JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *endpoint == "" || *path == "" || fs.NArg() != 0 {
		return fmt.Errorf("사용법: hx stop --endpoint <sock> --request <stop-request.json>")
	}
	b, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	var q stopRequest
	if err = json.Unmarshal(b, &q); err != nil {
		return err
	}
	if q.StopID == "" || q.TraceID == "" || q.Reason == "" {
		return fmt.Errorf("stop request: stop_id, trace_id, reason 필수")
	}
	c, err := net.Dial("unix", *endpoint)
	if err != nil {
		return err
	}
	defer c.Close()
	m := approvalrelay.Message{Op: "stop", TraceID: q.TraceID, StopID: q.StopID, TargetSpanID: q.TargetSpanID, Reason: q.Reason, EvidenceSeq: q.EvidenceSeq}
	if err = json.NewEncoder(c).Encode(m); err != nil {
		return err
	}
	var r approvalrelay.Result
	if err = json.NewDecoder(c).Decode(&r); err != nil {
		return err
	}
	if err = json.NewEncoder(os.Stdout).Encode(r); err != nil {
		return err
	}
	if r.Status == "stop_accepted" || r.Status == "already_terminal" {
		return nil
	}
	return fmt.Errorf("stop rejected: %s", r.Reason)
}
