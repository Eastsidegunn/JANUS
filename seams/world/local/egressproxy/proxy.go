// Package egressproxy implements HX's audited HTTP/HTTPS CONNECT proxy.
// It is deliberately independent of Podman so its fail-closed rules can be
// exercised without claiming that a unit test proves network isolation.
package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const connectPort = "443"

type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
)

// Attempt is metadata only. Request bodies, header values, and credentials
// are intentionally absent from the type so they cannot enter the audit wire.
type Attempt struct {
	Domain       string   `json:"domain"`
	Method       string   `json:"method"`
	RequestBytes int64    `json:"request_bytes"`
	AtUnixMs     int64    `json:"at_unix_ms"`
	Decision     Decision `json:"decision"`
	Reason       string   `json:"reason,omitempty"`
}

type AuditSink interface {
	Submit(context.Context, Attempt) error
}

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type DialContext func(context.Context, string, string) (net.Conn, error)

type Config struct {
	Allowlist []string
	Audit     AuditSink
	Resolver  Resolver
	Dial      DialContext
	Now       func() time.Time
}

type Proxy struct {
	allowlist []string
	audit     AuditSink
	resolver  Resolver
	dial      DialContext
	now       func() time.Time
}

func New(config Config) (*Proxy, error) {
	if config.Audit == nil {
		return nil, errors.New("egressproxy: audit sink가 없음")
	}
	allowlist, err := NormalizeAllowlist(config.Allowlist)
	if err != nil {
		return nil, fmt.Errorf("egressproxy: %w", err)
	}
	resolver := config.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dial := config.Dial
	if dial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Proxy{allowlist: allowlist, audit: config.Audit, resolver: resolver, dial: dial, now: now}, nil
}

// NormalizeAllowlist is shared by the host world and the sidecar so invalid
// policy input fails before Podman startup instead of leaving Ready blocked.
func NormalizeAllowlist(entries []string) ([]string, error) {
	allowlist := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		normalized, err := normalizeDomain(entry)
		if err != nil {
			return nil, fmt.Errorf("allowlist %q: %w", entry, err)
		}
		if !seen[normalized] {
			seen[normalized] = true
			allowlist = append(allowlist, normalized)
		}
	}
	sort.Strings(allowlist)
	return allowlist, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodConnect {
		p.serveConnect(w, request)
		return
	}
	p.serveHTTP(w, request)
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL == nil || request.URL.Scheme != "http" || request.URL.Host == "" {
		p.denyWithoutDomain(w, request, "HTTP proxy는 absolute http URL만 허용")
		return
	}
	domain, port, ip, ok := p.authorize(request.Context(), request.URL.Host, request.Method, requestBytes(request))
	if !ok {
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}

	outbound := request.Clone(request.Context())
	outbound.RequestURI = ""
	outbound.URL.Host = net.JoinHostPort(domain, port)
	removeHopHeaders(outbound.Header)
	outbound.Header.Del("Proxy-Authorization")
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return p.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		},
		DisableCompression: true,
	}
	defer transport.CloseIdleConnections()
	response, err := transport.RoundTrip(outbound)
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	removeHopHeaders(response.Header)
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (p *Proxy) serveConnect(w http.ResponseWriter, request *http.Request) {
	_, port, ip, ok := p.authorize(request.Context(), request.Host, request.Method, 0)
	if !ok {
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	if port != connectPort {
		// authorize has already emitted the denial for malformed/forbidden
		// targets. This branch is defensive and must never become a TCP tunnel.
		http.Error(w, "CONNECT port denied", http.StatusForbidden)
		return
	}
	upstream, err := p.dial(request.Context(), "tcp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "CONNECT unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	if err := buffered.Flush(); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	go tunnel(client, upstream)
}

func tunnel(client, upstream net.Conn) {
	defer client.Close()
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
}

func (p *Proxy) authorize(ctx context.Context, target, method string, size int64) (string, string, net.IP, bool) {
	domain, port, err := splitTarget(target, method)
	if err != nil {
		p.auditDeny(ctx, domain, method, size, err.Error())
		return "", "", nil, false
	}
	if !p.allowed(domain) {
		p.auditDeny(ctx, domain, method, size, "domain이 allowlist 밖")
		return "", "", nil, false
	}
	addresses, err := p.resolver.LookupIPAddr(ctx, domain)
	if err != nil || len(addresses) == 0 {
		reason := "DNS 결과 없음"
		if err != nil {
			reason = "DNS 해석 실패"
		}
		p.auditDeny(ctx, domain, method, size, reason)
		return "", "", nil, false
	}
	for _, address := range addresses {
		if !isPublicIP(address.IP) {
			p.auditDeny(ctx, domain, method, size, "private/loopback/link-local/metadata 주소")
			return "", "", nil, false
		}
	}
	if method == http.MethodConnect && port != connectPort {
		p.auditDeny(ctx, domain, method, size, "CONNECT는 port 443만 허용")
		return "", "", nil, false
	}
	attempt := Attempt{
		Domain: domain, Method: method, RequestBytes: size,
		AtUnixMs: p.now().UnixMilli(), Decision: DecisionAllow,
	}
	if err := p.audit.Submit(ctx, attempt); err != nil {
		return "", "", nil, false
	}
	return domain, port, addresses[0].IP, true
}

func (p *Proxy) auditDeny(ctx context.Context, domain, method string, size int64, reason string) {
	_ = p.audit.Submit(ctx, Attempt{
		Domain: domain, Method: method, RequestBytes: size,
		AtUnixMs: p.now().UnixMilli(), Decision: DecisionDeny, Reason: reason,
	})
}

func (p *Proxy) denyWithoutDomain(w http.ResponseWriter, request *http.Request, reason string) {
	p.auditDeny(request.Context(), "", request.Method, requestBytes(request), reason)
	http.Error(w, "egress denied", http.StatusForbidden)
}

func (p *Proxy) allowed(domain string) bool {
	for _, allowed := range p.allowlist {
		if domain == allowed || strings.HasSuffix(domain, "."+allowed) {
			return true
		}
	}
	return false
}

func splitTarget(target, method string) (string, string, error) {
	host := target
	port := "80"
	if method == http.MethodConnect {
		var err error
		host, port, err = net.SplitHostPort(target)
		if err != nil {
			return "", "", errors.New("CONNECT target은 host:port여야 함")
		}
	} else if parsedHost, parsedPort, err := net.SplitHostPort(target); err == nil {
		host, port = parsedHost, parsedPort
	}
	rawHost := strings.ToLower(strings.TrimSuffix(strings.Trim(strings.TrimSpace(host), "[]"), "."))
	if net.ParseIP(rawHost) != nil {
		return rawHost, "", errors.New("IP literal은 허용하지 않음")
	}
	domain, err := normalizeDomain(host)
	if err != nil {
		return "", "", err
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return domain, "", errors.New("유효하지 않은 port")
	}
	return domain, strconv.FormatUint(portNumber, 10), nil
}

func normalizeDomain(domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if domain == "" || len(domain) > 253 || net.ParseIP(strings.Trim(domain, "[]")) != nil {
		return "", errors.New("유효한 DNS domain이 아님")
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("유효한 DNS label이 아님")
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
				return "", errors.New("ASCII DNS domain만 허용")
			}
		}
	}
	return domain, nil
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 { // RFC 6598 shared address space
		return false
	}
	return true
}

func requestBytes(request *http.Request) int64 {
	if request.ContentLength > 0 {
		return request.ContentLength
	}
	return 0
}

func removeHopHeaders(header http.Header) {
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}

func copyHeaders(destination, source http.Header) {
	for name, values := range source {
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}
