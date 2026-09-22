package egressproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

const credentialDeadline = 30 * time.Second

// credentialRequest asks the host credential broker for one or more credential
// values by NAME. Only names travel to the host; the reply carries the values,
// which the proxy holds in memory and never writes to disk.
type credentialRequest struct {
	Type  string   `json:"type"`
	Names []string `json:"names"`
}

type credentialReply struct {
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"`
	Credentials map[string]string `json:"credentials,omitempty"`
}

// UnixCredentialSource is the proxy-side client of the host credential broker.
// The credential value flows across this Unix socket into proxy memory only;
// it is never persisted, echoed to argv, or emitted to the audit wire. Only the
// sidecar receives the mounted socket path — the agent container never does.
type UnixCredentialSource struct {
	Path string
}

// Fetch retrieves the values for the requested credential names once at proxy
// startup. It fails closed: a missing name or a broker error returns an error,
// so the proxy never begins listening without the credentials its injection
// rules require.
func (s UnixCredentialSource) Fetch(ctx context.Context, names []string) (map[string]string, error) {
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", s.Path)
	if err != nil {
		return nil, fmt.Errorf("credential socket 연결: %w", err)
	}
	defer connection.Close()
	deadline := time.Now().Add(credentialDeadline)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("credential socket deadline: %w", err)
	}
	if err := json.NewEncoder(connection).Encode(credentialRequest{Type: "fetch", Names: names}); err != nil {
		return nil, fmt.Errorf("credential 요청 전송: %w", err)
	}
	var reply credentialReply
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&reply); err != nil {
		return nil, fmt.Errorf("credential 응답: %w", err)
	}
	if !reply.OK {
		if reply.Error == "" {
			reply.Error = "host credential broker가 거부함"
		}
		return nil, errors.New(reply.Error)
	}
	for _, name := range names {
		if reply.Credentials[name] == "" {
			return nil, fmt.Errorf("credential %q 값이 응답에 없음", name)
		}
	}
	return reply.Credentials, nil
}
