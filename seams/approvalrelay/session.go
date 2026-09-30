package approvalrelay

// T25 (SCP-T25-001 §3): the relay socket multiplexes two more session ops next
// to approval and T19 stop.
//
//   - send_message{session_id, text}: inject a follow-up user turn into a
//     running multiturn session. Routed only through the session-owned
//     handler (the host subagent seam), which records user/message through
//     the single writer before delivery. This package never writes the log.
//   - events_tail{session_id, from_seq}: a read-only projection of the
//     session event log — envelopes after from_seq plus session status,
//     last activity and usage totals. It is a pure function of the log
//     (ProjectSession, the T18 Rebuild precedent); there is no state store.
//
// JANUS only emits and executes: no budget/idle/max_turns backstop lives here
// (SCP-T25-001 §4). Ending a session is normal done or the T19 stop op.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// Structured rejection reasons for send_message / events_tail.
const (
	ReasonUnknownSession  = "UNKNOWN_SESSION"
	ReasonNotMultiturn    = "NOT_MULTITURN"
	ReasonSessionTerminal = "SESSION_TERMINAL"
	ReasonSessionNotReady = "SESSION_NOT_READY"
	ReasonDeliveryFailed  = "DELIVERY_FAILED"
	ReasonLogUnavailable  = "LOG_UNAVAILABLE"
	ReasonLogInvalid      = "LOG_INVALID"
	ReasonRequestMismatch = "REQUEST_MISMATCH"
)

// maxTailEventsPerResult bounds one events_tail response; the caller pages
// with next_from_seq.
const maxTailEventsPerResult = 1000

// Errors a MessageHandler returns for deterministic rejections. The surface
// maps the host seam's own sentinels onto these (no seam-to-seam import).
var (
	ErrNotMultiturn    = errors.New("approvalrelay: session is not multiturn")
	ErrSessionTerminal = errors.New("approvalrelay: session is terminal")
)

// EventSource reads the session log from fromSeq (inclusive) in seq order. It
// is the log Reader of the session's single store; it must not write.
type EventSource func(ctx context.Context, fromSeq int64) ([]gen.EventRecord, error)

// MessageHandler durably records and delivers one follow-up user turn and
// returns the seq of its user/message record (0 when nothing was recorded).
type MessageHandler func(ctx context.Context, text string) (int64, error)

// SessionControl identifies the session this relay serves. SessionID is the
// session trace_id published in the acceptance session_ref.
type SessionControl struct {
	SessionID string
	Multiturn bool
	Events    EventSource
}

// Envelope is the metadata of one log event exposed by events_tail. The six
// identity fields are always present (never omitted, never zero-filled — the
// projection rejects a log that lacks them); usage and parent span are the
// only legitimately optional fields.
type Envelope struct {
	Seq          int64   `json:"seq"`
	Ts           int64   `json:"ts"`
	Kind         string  `json:"kind"`
	Actor        string  `json:"actor"`
	SpanID       string  `json:"span_id"`
	TraceID      string  `json:"trace_id"`
	ParentSpanID *string `json:"parent_span_id,omitempty"`
	UsageIn      *int64  `json:"usage_in,omitempty"`
	UsageOut     *int64  `json:"usage_out,omitempty"`
}

// SessionStatus is derived from the log alone. State is pending (no
// subagent/spawn yet), running (spawned, no terminal record) or exited
// (subagent/done or session/end recorded). The container exit code is not a
// log field, so it is not emitted; done_status carries the durable outcome.
type SessionStatus struct {
	State          string `json:"state"`
	DoneStatus     string `json:"done_status,omitempty"`
	TerminalSeq    int64  `json:"terminal_seq,omitempty"`
	SessionMode    string `json:"session_mode"`
	LastSeq        int64  `json:"last_seq"`
	LastActivityTs int64  `json:"last_activity_ts"`
	UsageInTotal   int64  `json:"usage_in_total"`
	UsageOutTotal  int64  `json:"usage_out_total"`
}

// TailResult is the events_tail response. Events is always serialized (an
// empty page is [] rather than absent). NextFromSeq is the from_seq for the
// next call; More reports that the page limit truncated the tail.
type TailResult struct {
	Status      string        `json:"status"`
	Reason      string        `json:"reason,omitempty"`
	SessionID   string        `json:"session_id,omitempty"`
	Events      []Envelope    `json:"events"`
	NextFromSeq int64         `json:"next_from_seq"`
	More        bool          `json:"more"`
	Session     SessionStatus `json:"session"`
}

// ProjectSession is the pure events_tail projection. It validates the
// envelope of every event (F2: required fields present, one trace, seq
// contiguous from 1 — no gap is ever synthesized or skipped) and derives the
// session status, then pages the envelopes with seq > fromSeq.
func ProjectSession(events []gen.EventRecord, sessionID string, fromSeq int64, limit int) (TailResult, error) {
	if fromSeq < 0 {
		return TailResult{}, fmt.Errorf("from_seq %d < 0", fromSeq)
	}
	if limit <= 0 {
		limit = maxTailEventsPerResult
	}
	out := TailResult{Status: "ok", SessionID: sessionID, Events: []Envelope{}, NextFromSeq: fromSeq,
		Session: SessionStatus{State: "pending", SessionMode: string(gen.SubagentSpawnPayloadSessionModeOneshot)}}
	for i, e := range events {
		if err := validateEnvelope(e, int64(i+1), sessionID); err != nil {
			return TailResult{}, err
		}
		st := &out.Session
		st.LastSeq, st.LastActivityTs = e.Seq, e.Ts
		if e.UsageIn != nil {
			if err := addChecked(&st.UsageInTotal, *e.UsageIn); err != nil {
				return TailResult{}, fmt.Errorf("seq %d usage_in: %w", e.Seq, err)
			}
		}
		if e.UsageOut != nil {
			if err := addChecked(&st.UsageOutTotal, *e.UsageOut); err != nil {
				return TailResult{}, fmt.Errorf("seq %d usage_out: %w", e.Seq, err)
			}
		}
		switch e.Kind {
		case gen.KindSubagentSpawn:
			if st.State == "pending" {
				st.State = "running"
			}
			var p gen.SubagentSpawnPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return TailResult{}, fmt.Errorf("seq %d spawn payload: %w", e.Seq, err)
			}
			if p.SessionMode != nil {
				st.SessionMode = string(*p.SessionMode)
			}
		case gen.KindSubagentDone:
			var p gen.DonePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return TailResult{}, fmt.Errorf("seq %d done payload: %w", e.Seq, err)
			}
			if st.State != "exited" {
				st.State, st.DoneStatus, st.TerminalSeq = "exited", string(p.Status), e.Seq
			}
		case gen.KindSessionEnd:
			if st.State != "exited" {
				st.State, st.TerminalSeq = "exited", e.Seq
			}
		}
		if e.Seq > fromSeq {
			if len(out.Events) == limit {
				out.More = true
				continue
			}
			out.Events = append(out.Events, Envelope{
				Seq: e.Seq, Ts: e.Ts, Kind: string(e.Kind), Actor: e.Actor, SpanID: e.SpanID, TraceID: e.TraceID,
				ParentSpanID: e.ParentSpanID, UsageIn: e.UsageIn, UsageOut: e.UsageOut,
			})
			out.NextFromSeq = e.Seq
		}
	}
	return out, nil
}

func validateEnvelope(e gen.EventRecord, wantSeq int64, sessionID string) error {
	switch {
	case e.Seq != wantSeq:
		return fmt.Errorf("seq %d at position %d: log is not contiguous from 1", e.Seq, wantSeq)
	case e.Ts <= 0:
		return fmt.Errorf("seq %d: ts missing", e.Seq)
	case e.Kind == "":
		return fmt.Errorf("seq %d: kind missing", e.Seq)
	case e.Actor == "":
		return fmt.Errorf("seq %d: actor missing", e.Seq)
	case e.SpanID == "":
		return fmt.Errorf("seq %d: span_id missing", e.Seq)
	case e.TraceID == "":
		return fmt.Errorf("seq %d: trace_id missing", e.Seq)
	case e.TraceID != sessionID:
		return fmt.Errorf("seq %d: trace_id %s is not session %s", e.Seq, e.TraceID, sessionID)
	}
	return nil
}

func addChecked(total *int64, v int64) error {
	if v < 0 {
		return fmt.Errorf("negative usage %d", v)
	}
	if *total > math.MaxInt64-v {
		return fmt.Errorf("usage overflow")
	}
	*total += v
	return nil
}

// SetSession binds the relay to its session. It is set once the session log
// exists (before launch) so send_message and events_tail reject
// deterministically from the first connection.
func (s *Server) SetSession(c SessionControl) {
	s.mu.Lock()
	s.session = c
	s.mu.Unlock()
}

// SetMessageHandler installs the session-owned delivery path once the
// subagent is running. Before that send_message reports SESSION_NOT_READY.
func (s *Server) SetMessageHandler(h MessageHandler) {
	s.mu.Lock()
	s.messageHandler = h
	s.mu.Unlock()
}

func (s *Server) handleSendMessage(m Message) Result {
	s.mu.Lock()
	ctl, handler, terminal, timeout := s.session, s.messageHandler, s.terminalRef, s.Timeout
	s.mu.Unlock()
	if m.SessionID == "" || strings.TrimSpace(m.Text) == "" {
		return Result{Status: "error", Reason: ReasonRequestMismatch}
	}
	if ctl.SessionID == "" || m.SessionID != ctl.SessionID {
		return Result{Status: "error", Reason: ReasonUnknownSession}
	}
	if !ctl.Multiturn {
		return Result{Status: "error", Reason: ReasonNotMultiturn}
	}
	if terminal != 0 {
		return Result{Status: "error", Reason: ReasonSessionTerminal, TerminalRef: terminal}
	}
	if handler == nil {
		return Result{Status: "error", Reason: ReasonSessionNotReady}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	seq, err := handler(ctx, m.Text)
	switch {
	case err == nil:
		return Result{Status: "message_accepted", MessageSeq: seq}
	case errors.Is(err, ErrNotMultiturn):
		return Result{Status: "error", Reason: ReasonNotMultiturn}
	case errors.Is(err, ErrSessionTerminal):
		return Result{Status: "error", Reason: ReasonSessionTerminal}
	case seq > 0:
		// Recorded (model-visible means logged) but the adapter did not take
		// it; the durable record stays and its seq is reported.
		return Result{Status: "error", Reason: ReasonDeliveryFailed, MessageSeq: seq}
	default:
		return Result{Status: "error", Reason: ReasonLogUnavailable}
	}
}

func (s *Server) handleEventsTail(m Message) TailResult {
	s.mu.Lock()
	ctl, timeout := s.session, s.Timeout
	s.mu.Unlock()
	fail := func(reason string) TailResult {
		return TailResult{Status: "error", Reason: reason, Events: []Envelope{}}
	}
	if m.SessionID == "" || m.FromSeq < 0 {
		return fail(ReasonRequestMismatch)
	}
	if ctl.SessionID == "" || m.SessionID != ctl.SessionID {
		return fail(ReasonUnknownSession)
	}
	if ctl.Events == nil {
		return fail(ReasonLogUnavailable)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// The whole log is read so status/usage are recomputed from seq 1 on every
	// call; nothing is cached between calls.
	events, err := ctl.Events(ctx, 1)
	if err != nil {
		return fail(ReasonLogUnavailable)
	}
	r, err := ProjectSession(events, ctl.SessionID, m.FromSeq, maxTailEventsPerResult)
	if err != nil {
		return fail(ReasonLogInvalid)
	}
	return r
}
