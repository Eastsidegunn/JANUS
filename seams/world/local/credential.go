package local

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	credentialSocketName = "cred.sock"
	credentialMount      = "/run/hx-cred"
	credentialSocketPath = credentialMount + "/" + credentialSocketName
	credentialMaxLine    = 64 * 1024
	credentialIOTimeout  = 30 * time.Second
)

// credentialBroker is the host-only channel that delivers proxy-held credential
// VALUES to the egress proxy sidecar (T20 하드 결정 1). It mirrors the audit
// broker's shape: a Unix socket in a short 0700 root, mounted read-only into
// the proxy container. The credential value is held only in this host process's
// memory and streamed to the proxy over the socket — it is never written to a
// file, so although the socket PATH lives on disk, the secret BYTES only ever
// flow through the socket. Podman --secret was rejected precisely because it is
// a disk-backed store. The agent container never receives this mount.
type credentialBroker struct {
	dir      string
	listener net.Listener
	values   map[string]string

	mu       sync.Mutex
	closing  bool
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closeErr error
	once     sync.Once
}

// startCredentialBroker binds the host socket and begins serving fetches. The
// values map is copied so later caller mutation cannot change what is served.
func startCredentialBroker(values map[string]string) (*credentialBroker, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("world/local: credential broker에 값이 없음")
	}
	copied := make(map[string]string, len(values))
	for name, value := range values {
		if value == "" {
			return nil, fmt.Errorf("world/local: credential %q 값이 비어 있음", name)
		}
		copied[name] = value
	}
	// Short 0700 root, as the audit/approval brokers use, keeps the Unix socket
	// path well under the 108-byte limit.
	dir, err := os.MkdirTemp("/tmp", "hxc-")
	if err != nil {
		return nil, fmt.Errorf("world/local: credential socket root: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("world/local: credential socket root mode: %w", err)
	}
	path := filepath.Join(dir, credentialSocketName)
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("world/local: credential socket listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("world/local: credential socket mode: %w", err)
	}
	broker := &credentialBroker{dir: dir, listener: listener, values: copied, conns: map[net.Conn]struct{}{}}
	broker.wg.Add(1)
	go broker.accept()
	return broker, nil
}

func (b *credentialBroker) SocketDir() string { return b.dir }

func (b *credentialBroker) accept() {
	defer b.wg.Done()
	for {
		connection, err := b.listener.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		if b.closing {
			b.mu.Unlock()
			connection.Close()
			return
		}
		b.conns[connection] = struct{}{}
		b.wg.Add(1)
		b.mu.Unlock()
		go b.handle(connection)
	}
}

func (b *credentialBroker) handle(connection net.Conn) {
	defer func() {
		connection.Close()
		b.mu.Lock()
		delete(b.conns, connection)
		b.mu.Unlock()
		b.wg.Done()
	}()
	_ = connection.SetDeadline(time.Now().Add(credentialIOTimeout))
	reader := bufio.NewReader(io.LimitReader(connection, credentialMaxLine+1))
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	if len(line) > credentialMaxLine {
		b.reply(connection, credentialReply{Error: "credential 요청이 64KiB 상한 초과"})
		return
	}
	var request credentialRequestWire
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Type != "fetch" {
		b.reply(connection, credentialReply{Error: "credential 요청 형식 오류"})
		return
	}
	out := make(map[string]string, len(request.Names))
	for _, name := range request.Names {
		value, ok := b.values[name]
		if !ok {
			// Do not echo the requested name back in a way that could confuse a
			// value with an error; a missing name is simply refused.
			b.reply(connection, credentialReply{Error: "요청한 credential 이름이 없음"})
			return
		}
		out[name] = value
	}
	b.reply(connection, credentialReply{OK: true, Credentials: out})
}

func (b *credentialBroker) reply(connection net.Conn, reply credentialReply) {
	_ = json.NewEncoder(connection).Encode(reply)
}

// Cleanup stops the broker and removes its socket directory. Closing the
// listener and dir also removes the only on-disk artifact of the channel.
func (b *credentialBroker) Cleanup() error {
	b.once.Do(func() {
		b.mu.Lock()
		b.closing = true
		conns := make([]net.Conn, 0, len(b.conns))
		for connection := range b.conns {
			conns = append(conns, connection)
		}
		b.mu.Unlock()
		if err := b.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			b.closeErr = errors.Join(b.closeErr, err)
		}
		for _, connection := range conns {
			connection.Close()
		}
		b.wg.Wait()
		if err := os.RemoveAll(b.dir); err != nil {
			b.closeErr = errors.Join(b.closeErr, fmt.Errorf("world/local: credential socket cleanup: %w", err))
		}
	})
	return b.closeErr
}

// credentialRequestWire and credentialReply mirror the proxy-side wire in
// egressproxy (which keeps its own copies unexported). The JSON tags are the
// contract between the two ends.
type credentialRequestWire struct {
	Type  string   `json:"type"`
	Names []string `json:"names"`
}

type credentialReply struct {
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"`
	Credentials map[string]string `json:"credentials,omitempty"`
}
