package approvalrelay

// UnixApprovalRelay is an opt-in, fail-closed ApprovalDecider. The wire
// carries a digest and display summary only; raw tool arguments never leave
// JANUS. This package is the transport seam; durable reconnect state and the
// listener protocol are still required before this is production-ready.
import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Eastsidegunn/JANUS/core/policy"
)

type RelayConfig struct {
	Endpoint   string
	TraceID    string
	PolicyHash string
	Timeout    time.Duration
}
type UnixApprovalRelay struct {
	cfg     RelayConfig
	mu      sync.Mutex
	decided map[string]policy.ApprovalDecision
}

type relayRequest struct {
	Version        int    `json:"version"`
	TraceID        string `json:"trace_id"`
	SpanID         string `json:"span_id"`
	RequestID      string `json:"request_id"`
	RequestDigest  string `json:"request_digest"`
	PolicyHash     string `json:"policy_hash"`
	DisplaySummary string `json:"display_summary"`
	ExpiresAt      int64  `json:"expires_at"`
}
type relayResponse struct{ RequestID, ResponseID, Decision, Reason string }

func NewUnixApprovalRelay(cfg RelayConfig) (*UnixApprovalRelay, error) {
	if cfg.Endpoint == "" || !filepath.IsAbs(cfg.Endpoint) {
		return nil, fmt.Errorf("approval relay endpoint must be absolute")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &UnixApprovalRelay{cfg: cfg, decided: map[string]policy.ApprovalDecision{}}, nil
}

func (r *UnixApprovalRelay) Decide(ctx context.Context, req policy.ApprovalRequest) (policy.ApprovalDecision, error) {
	key := req.RequestID
	r.mu.Lock()
	if d, ok := r.decided[key]; ok {
		r.mu.Unlock()
		return d, nil
	}
	r.mu.Unlock()
	digest := sha256.Sum256(req.Args)
	wire := relayRequest{Version: 1, TraceID: r.cfg.TraceID, SpanID: req.SpanID, RequestID: req.RequestID, RequestDigest: hex.EncodeToString(digest[:]), PolicyHash: r.cfg.PolicyHash, DisplaySummary: "tool=" + req.ToolName, ExpiresAt: time.Now().Add(r.cfg.Timeout).UnixMilli()}
	cctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	var d policy.ApprovalDecision
	conn, err := (&net.Dialer{}).DialContext(cctx, "unix", r.cfg.Endpoint)
	if err == nil {
		if err = json.NewEncoder(conn).Encode(wire); err == nil {
			var resp relayResponse
			err = json.NewDecoder(bufio.NewReader(conn)).Decode(&resp)
			if err == nil {
				if resp.RequestID != req.RequestID || (resp.Decision != "allow" && resp.Decision != "deny") {
					err = fmt.Errorf("approval relay request scope mismatch")
				} else if resp.Decision == "allow" {
					d.Allow = true
				}
				if resp.Reason != "" {
					d.Reason = resp.Reason
				}
			}
		}
		conn.Close()
	}
	if err != nil || (!d.Allow && strings.TrimSpace(d.Reason) == "") {
		d.Allow = false
		if err != nil {
			d.Reason = "relay unavailable: " + err.Error()
		} else {
			d.Reason = "remote denial reason required"
		}
	}
	if !d.Allow && d.Reason == "" {
		d.Reason = "relay denied"
	}
	r.mu.Lock()
	r.decided[key] = d
	r.mu.Unlock()
	return d, nil
}
