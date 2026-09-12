package approvalrelay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Server is the JANUS-owned Unix relay listener. State is an index of the
// caller-owned session log; it is intentionally not an independent durable
// store.
type Server struct {
	Endpoint  string
	Timeout   time.Duration
	mu        sync.Mutex
	pending   map[requestKey]chan Result
	decided   map[requestKey]Result
	byID      map[string]requestKey
	meta      map[requestKey]PendingMeta
	ln        net.Listener
	OwnerUID  int
	peerCheck func(net.Conn) (int, error)
}
type PendingMeta struct {
	RequestDigest  string `json:"request_digest"`
	PolicyHash     string `json:"policy_hash"`
	DisplaySummary string `json:"display_summary"`
	ExpiresAt      int64  `json:"expires_at"`
}
type requestKey struct{ TraceID, SpanID, RequestID string }
type Result struct {
	Status         string `json:"status"`
	Decision       string `json:"decision,omitempty"`
	Reason         string `json:"reason,omitempty"`
	ResponseSeq    int64  `json:"response_seq,omitempty"`
	ResponseID     string `json:"response_id,omitempty"`
	RequestDigest  string `json:"request_digest,omitempty"`
	PolicyHash     string `json:"policy_hash,omitempty"`
	DisplaySummary string `json:"display_summary,omitempty"`
	ExpiresAt      int64  `json:"expires_at,omitempty"`
}
type Message struct {
	Op         string `json:"op"`
	TraceID    string `json:"trace_id"`
	SpanID     string `json:"span_id"`
	RequestID  string `json:"request_id"`
	ResponseID string `json:"response_id,omitempty"`
	Decision   string `json:"decision,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func NewServer(endpoint string, timeout time.Duration) (*Server, error) {
	if !filepath.IsAbs(endpoint) {
		return nil, fmt.Errorf("approval relay endpoint must be absolute")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Server{Endpoint: endpoint, Timeout: timeout, OwnerUID: os.Getuid(), peerCheck: peerUID, pending: map[requestKey]chan Result{}, decided: map[requestKey]Result{}, byID: map[string]requestKey{}, meta: map[requestKey]PendingMeta{}}, nil
}

func (s *Server) Listen() error {
	if err := os.MkdirAll(filepath.Dir(s.Endpoint), 0700); err != nil {
		return err
	}
	if info, err := os.Stat(filepath.Dir(s.Endpoint)); err != nil || info.Mode().Perm() != 0700 {
		return fmt.Errorf("approval relay socket directory must be mode 0700")
	}
	if _, err := os.Stat(s.Endpoint); err == nil {
		return fmt.Errorf("approval relay socket already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", s.Endpoint)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.Endpoint, 0600); err != nil {
		ln.Close()
		return err
	}
	s.ln = ln
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.serve(c)
	}
}
func (s *Server) Close() error {
	if s.ln != nil {
		err := s.ln.Close()
		_ = os.Remove(s.Endpoint)
		return err
	}
	return nil
}

func (s *Server) serve(c net.Conn) {
	defer c.Close()
	uid, err := s.peerCheck(c)
	if err != nil || uid != s.OwnerUID {
		_ = json.NewEncoder(c).Encode(Result{Status: "error", Reason: "UNAUTHENTICATED"})
		return
	}
	dec := json.NewDecoder(bufio.NewReader(c))
	enc := json.NewEncoder(c)
	var m Message
	if dec.Decode(&m) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := requestKey{m.TraceID, m.SpanID, m.RequestID}
	if m.RequestID == "" || m.TraceID == "" || m.SpanID == "" {
		_ = enc.Encode(Result{Status: "error", Reason: "UNAUTHENTICATED"})
		return
	}
	if m.Op == "query" {
		if r, ok := s.decided[k]; ok {
			_ = enc.Encode(r)
		} else if _, ok := s.pending[k]; ok {
			m := s.meta[k]
			_ = enc.Encode(Result{Status: "pending", RequestDigest: m.RequestDigest, PolicyHash: m.PolicyHash, DisplaySummary: m.DisplaySummary, ExpiresAt: m.ExpiresAt})
		} else {
			_ = enc.Encode(Result{Status: "unknown"})
		}
		return
	}
	if m.Op != "submit" || (m.Decision != "allow" && m.Decision != "deny") {
		_ = enc.Encode(Result{Status: "error", Reason: "REQUEST_MISMATCH"})
		return
	}
	if old, ok := s.decided[k]; ok {
		if old.Status == "expired" {
			_ = enc.Encode(old)
			return
		}
		if old.ResponseID != m.ResponseID || old.Decision != m.Decision || old.Reason != m.Reason {
			_ = enc.Encode(Result{Status: "error", Reason: "RESPONSE_CONFLICT"})
		} else {
			_ = enc.Encode(old)
		}
		return
	}
	if m.Decision == "deny" && strings.TrimSpace(m.Reason) == "" {
		_ = enc.Encode(Result{Status: "error", Reason: "REQUEST_MISMATCH"})
		return
	}
	r := Result{Status: "decided", Decision: m.Decision, Reason: m.Reason, ResponseID: m.ResponseID}
	s.decided[k] = r
	if ch, ok := s.pending[k]; ok {
		ch <- r
		delete(s.pending, k)
	}
	_ = enc.Encode(r)
}

func (s *Server) Wait(ctx context.Context, traceID, spanID, requestID string) Result {
	return s.WaitWithMeta(ctx, traceID, spanID, requestID, PendingMeta{})
}
func (s *Server) WaitWithMeta(ctx context.Context, traceID, spanID, requestID string, meta PendingMeta) Result {
	s.mu.Lock()
	k := requestKey{traceID, spanID, requestID}
	if r, ok := s.decided[k]; ok {
		s.mu.Unlock()
		return r
	}
	ch := make(chan Result, 1)
	s.pending[k] = ch
	s.byID[requestID] = k
	s.meta[k] = meta
	s.mu.Unlock()
	t := time.NewTimer(s.Timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r
	case <-ctx.Done():
		r := Result{Status: "expired", Decision: "deny", Reason: "LEASE_ENDED"}
		s.mu.Lock()
		delete(s.pending, k)
		s.decided[k] = r
		s.mu.Unlock()
		return r
	case <-t.C:
		r := Result{Status: "expired", Decision: "deny", Reason: "EXPIRED"}
		s.mu.Lock()
		delete(s.pending, k)
		s.decided[k] = r
		s.mu.Unlock()
		return r
	}
}

// RecordApprovalResult attaches the durable session event sequence to the
// derived decision after the coordinator commits it.
func (s *Server) RecordApprovalResult(req policy.ApprovalRequest, d policy.ApprovalDecision, seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[req.RequestID]
	if !ok {
		return
	}
	r := s.decided[k]
	if r.Status != "expired" {
		r.Status = "decided"
		if !d.Allow {
			r.Decision = "deny"
		} else {
			r.Decision = "allow"
		}
		r.Reason = d.Reason
	}
	r.ResponseSeq = seq
	s.decided[k] = r
}
