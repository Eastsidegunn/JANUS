package approvalrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"path/filepath"
	"time"
)

type RelayConfig struct {
	Endpoint, TraceID, PolicyHash string
	Timeout                       time.Duration
}
type UnixApprovalRelay struct {
	cfg    RelayConfig
	server *Server
}

// RecordApprovalResult forwards the coordinator's durable event sequence to
// the server-owned derived index.
func (r *UnixApprovalRelay) RecordApprovalResult(req policy.ApprovalRequest, d policy.ApprovalDecision, seq int64) {
	if r.server != nil {
		r.server.RecordApprovalResult(req, d, seq)
	}
}

func NewServerApprovalRelay(server *Server, cfg RelayConfig) (*UnixApprovalRelay, error) {
	if server == nil {
		return nil, fmt.Errorf("approval relay server is nil")
	}
	if cfg.Endpoint == "" || !filepath.IsAbs(cfg.Endpoint) {
		return nil, fmt.Errorf("approval relay endpoint must be absolute")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &UnixApprovalRelay{cfg: cfg, server: server}, nil
}
func (r *UnixApprovalRelay) Decide(ctx context.Context, req policy.ApprovalRequest) (policy.ApprovalDecision, error) {
	canonical, err := policy.CanonicalArgs(req.Args)
	if err != nil {
		return policy.ApprovalDecision{Reason: "forced: invalid approval args", DecisionSource: "forced"}, nil
	}
	sum := sha256.Sum256(canonical)
	res := r.server.WaitWithMeta(ctx, r.cfg.TraceID, req.SpanID, req.RequestID, PendingMeta{RequestDigest: "hx-args-digest-v1:" + hex.EncodeToString(sum[:]), PolicyHash: r.cfg.PolicyHash, DisplaySummary: "tool=" + req.ToolName, ExpiresAt: time.Now().Add(r.cfg.Timeout).UnixMilli()})
	d := policy.ApprovalDecision{Allow: res.Decision == "allow", Reason: res.Reason, ResponseID: res.ResponseID}
	if res.Status == "expired" {
		d.DecisionSource = "forced"
	} else {
		d.DecisionSource = "relay"
		d.ActorRef = "unverified-local-operator"
	}
	return d, nil
}
