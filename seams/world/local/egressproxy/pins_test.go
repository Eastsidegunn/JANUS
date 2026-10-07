package egressproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// SCP-T26-001 / FR-SBX-03: declared gateway pin exception.

const (
	pinnedDomain  = "gw.example"
	pinnedAddress = "100.64.0.10:8080" // RFC 6598 (Tailscale CGNAT)
)

// fakeForbiddenResolver fails the test on any lookup: pinned domains must
// never touch DNS.
type fakeForbiddenResolver struct {
	t     *testing.T
	calls atomic.Int64
}

func (f *fakeForbiddenResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	f.calls.Add(1)
	f.t.Errorf("pinned 경로에서 Resolver 호출됨: %s", host)
	return nil, errors.New("resolver must not be called")
}

// countingResolver records lookups for the non-pinned regression path.
type countingResolver struct {
	calls   atomic.Int64
	answers fakeResolver
}

func (c *countingResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	c.calls.Add(1)
	return c.answers.LookupIPAddr(ctx, host)
}

func pinMap(t *testing.T, entries map[string]string) map[string]netip.AddrPort {
	t.Helper()
	out := make(map[string]netip.AddrPort, len(entries))
	for domain, address := range entries {
		out[domain] = netip.MustParseAddrPort(address)
	}
	return out
}

func pipeHTTPServer() (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		request, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		request.Body.Close()
		_, _ = server.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	}()
	return client, nil
}

// (a) 핀 도메인: Resolver 0회, 핀 주소 dial, allow audit 1건.
func TestPinnedDomainSkipsDNSAndDialsPinnedAddress(t *testing.T) {
	audit := &fakeAudit{}
	resolver := &fakeForbiddenResolver{t: t}
	var dialed []string
	proxy := mustProxy(t, Config{
		Allowlist: []string{pinnedDomain}, Audit: audit, Resolver: resolver,
		Pins: pinMap(t, map[string]string{pinnedDomain: pinnedAddress}),
		Dial: func(_ context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, network+" "+address)
			return pipeHTTPServer()
		},
		Now: func() time.Time { return time.UnixMilli(42) },
	})
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://gw.example:8080/v1/messages", strings.NewReader("body")))
	if response.Code != http.StatusOK || response.Body.String() != "ok" {
		t.Fatalf("pinned forwarding = %d %q", response.Code, response.Body.String())
	}
	if got := resolver.calls.Load(); got != 0 {
		t.Fatalf("Resolver 호출 %d회, want 0", got)
	}
	if len(dialed) != 1 || dialed[0] != "tcp "+pinnedAddress {
		t.Fatalf("pinned dial = %v, want [tcp %s]", dialed, pinnedAddress)
	}
	attempts := audit.snapshot()
	want := Attempt{Domain: pinnedDomain, Method: http.MethodPost, RequestBytes: 4, AtUnixMs: 42, Decision: DecisionAllow}
	if len(attempts) != 1 || attempts[0] != want {
		t.Fatalf("audit = %+v, want exactly [%+v]", attempts, want)
	}
}

// (a) CONNECT도 핀 port가 443이면 DNS 없이 핀 주소로 tunnel한다.
func TestPinnedConnect443DialsPinnedAddressWithoutDNS(t *testing.T) {
	audit := &fakeAudit{}
	resolver := &fakeForbiddenResolver{t: t}
	var dialed atomic.Value
	proxy := mustProxy(t, Config{
		Allowlist: []string{"example"}, Audit: audit, Resolver: resolver,
		Pins: pinMap(t, map[string]string{pinnedDomain: "10.1.2.3:443"}),
		Dial: func(_ context.Context, network, address string) (net.Conn, error) {
			dialed.Store(network + " " + address)
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				buffer := make([]byte, 4)
				if _, err := io.ReadFull(server, buffer); err == nil {
					_, _ = server.Write(buffer)
				}
			}()
			return client, nil
		},
	})
	server := httptest.NewServer(proxy)
	defer server.Close()
	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := connection.Write([]byte("CONNECT gw.example:443 HTTP/1.1\r\nHost: gw.example:443\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status=%d", response.StatusCode)
	}
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(reader, got); err != nil || string(got) != "ping" {
		t.Fatalf("CONNECT echo=%q err=%v", got, err)
	}
	if d, _ := dialed.Load().(string); d != "tcp 10.1.2.3:443" {
		t.Fatalf("CONNECT pinned dial = %q", d)
	}
	if resolver.calls.Load() != 0 {
		t.Fatalf("Resolver 호출 %d회", resolver.calls.Load())
	}
	attempts := audit.snapshot()
	if len(attempts) != 1 || attempts[0].Decision != DecisionAllow || attempts[0].Domain != pinnedDomain ||
		attempts[0].Method != http.MethodConnect {
		t.Fatalf("audit = %+v", attempts)
	}
}

// (b) 핀 port 불일치 deny(+audit reason), CONNECT 443 규칙 유지, 핀은
// allowlist를 넓히지 못함, 비핀 CGNAT 도메인은 기존대로 deny.
func TestPinnedDenialsNeverDialAndAreAudited(t *testing.T) {
	cgnat := []net.IPAddr{{IP: net.ParseIP("100.64.0.10")}}
	cases := []struct {
		name, method, target string
		allow                []string
		pins                 map[string]string
		answers              fakeResolver
		reason               string
		wantLookups          int64
	}{
		{"HTTP pinned port 불일치", http.MethodGet, "http://gw.example/v1", []string{pinnedDomain},
			map[string]string{pinnedDomain: pinnedAddress}, nil, "pinned port 불일치", 0},
		{"HTTP pinned 다른 port", http.MethodGet, "http://gw.example:8318/v1", []string{pinnedDomain},
			map[string]string{pinnedDomain: pinnedAddress}, nil, "pinned port 불일치", 0},
		{"CONNECT pinned port 불일치", http.MethodConnect, "gw.example:443", []string{pinnedDomain},
			map[string]string{pinnedDomain: pinnedAddress}, nil, "pinned port 불일치", 0},
		{"CONNECT 비443 핀", http.MethodConnect, "gw.example:8080", []string{pinnedDomain},
			map[string]string{pinnedDomain: pinnedAddress}, nil, "CONNECT는 port 443만 허용", 0},
		{"핀이 allowlist를 넓히지 못함", http.MethodGet, "http://gw.example:8080/", []string{"other.example"},
			map[string]string{pinnedDomain: pinnedAddress}, nil, "domain이 allowlist 밖", 0},
		{"비핀 CGNAT domain", http.MethodGet, "http://cgnat.example:8080/", []string{pinnedDomain, "cgnat.example"},
			map[string]string{pinnedDomain: pinnedAddress}, fakeResolver{"cgnat.example": cgnat},
			"private/loopback/link-local/metadata 주소", 1},
		{"핀 subdomain은 핀이 아님", http.MethodGet, "http://sub.gw.example:8080/", []string{pinnedDomain},
			map[string]string{pinnedDomain: pinnedAddress}, fakeResolver{"sub.gw.example": cgnat},
			"private/loopback/link-local/metadata 주소", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			audit := &fakeAudit{}
			resolver := &countingResolver{answers: tc.answers}
			dials := 0
			proxy := mustProxy(t, Config{
				Allowlist: tc.allow, Audit: audit, Resolver: resolver, Pins: pinMap(t, tc.pins),
				Dial: func(context.Context, string, string) (net.Conn, error) {
					dials++
					return nil, errors.New("must not dial")
				},
			})
			var request *http.Request
			if tc.method == http.MethodConnect {
				request = httptest.NewRequest(http.MethodConnect, "http://proxy.invalid", nil)
				request.Host = tc.target
			} else {
				request = httptest.NewRequest(tc.method, tc.target, nil)
			}
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || dials != 0 {
				t.Fatalf("response=%d dials=%d", response.Code, dials)
			}
			if got := resolver.calls.Load(); got != tc.wantLookups {
				t.Fatalf("Resolver 호출 %d회, want %d", got, tc.wantLookups)
			}
			attempts := audit.snapshot()
			if len(attempts) != 1 || attempts[0].Decision != DecisionDeny || attempts[0].Reason != tc.reason {
				t.Fatalf("deny audit = %+v, want 1건 reason %q", attempts, tc.reason)
			}
		})
	}
}

// 핀이 있어도 비핀 공개 도메인 경로는 기존과 동일하게 DNS 해석 주소로 dial.
func TestNonPinnedDomainUnchangedWhenPinsPresent(t *testing.T) {
	audit := &fakeAudit{}
	resolver := &countingResolver{answers: fakeResolver{"allowed.example": {{IP: net.ParseIP("93.184.216.34")}}}}
	var dialed []string
	proxy := mustProxy(t, Config{
		Allowlist: []string{"allowed.example", pinnedDomain}, Audit: audit, Resolver: resolver,
		Pins: pinMap(t, map[string]string{pinnedDomain: pinnedAddress}),
		Dial: func(_ context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, network+" "+address)
			return pipeHTTPServer()
		},
	})
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://allowed.example/path", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if resolver.calls.Load() != 1 || len(dialed) != 1 || dialed[0] != "tcp 93.184.216.34:80" {
		t.Fatalf("lookups=%d dialed=%v", resolver.calls.Load(), dialed)
	}
	if attempts := audit.snapshot(); len(attempts) != 1 || attempts[0].Decision != DecisionAllow {
		t.Fatalf("audit = %+v", attempts)
	}
}

// New는 핀을 world-config와 동일 규칙으로 재검증한다(fail-closed).
func TestNewRejectsInvalidPins(t *testing.T) {
	cases := map[string]map[string]netip.AddrPort{
		"loopback v4":       {pinnedDomain: netip.MustParseAddrPort("127.0.0.1:8080")},
		"loopback v6":       {pinnedDomain: netip.MustParseAddrPort("[::1]:8080")},
		"link-local":        {pinnedDomain: netip.MustParseAddrPort("169.254.169.254:80")},
		"link-local v6":     {pinnedDomain: netip.MustParseAddrPort("[fe80::1]:80")},
		"multicast":         {pinnedDomain: netip.MustParseAddrPort("224.0.0.1:80")},
		"unspecified":       {pinnedDomain: netip.MustParseAddrPort("0.0.0.0:80")},
		"unspecified v6":    {pinnedDomain: netip.MustParseAddrPort("[::]:80")},
		"port 0":            {pinnedDomain: netip.MustParseAddrPort("100.64.0.10:0")},
		"4in6 loopback":     {pinnedDomain: netip.MustParseAddrPort("[::ffff:127.0.0.1]:80")},
		"zero AddrPort":     {pinnedDomain: {}},
		"IP literal domain": {"100.64.0.10": netip.MustParseAddrPort(pinnedAddress)},
		"wildcard domain":   {"*.example": netip.MustParseAddrPort(pinnedAddress)},
		"case duplicate": {
			"gw.example": netip.MustParseAddrPort(pinnedAddress),
			"GW.example": netip.MustParseAddrPort(pinnedAddress),
		},
		"상한 초과": {
			"a.example": netip.MustParseAddrPort(pinnedAddress), "b.example": netip.MustParseAddrPort(pinnedAddress),
			"c.example": netip.MustParseAddrPort(pinnedAddress), "d.example": netip.MustParseAddrPort(pinnedAddress),
			"e.example": netip.MustParseAddrPort(pinnedAddress),
		},
	}
	for name, pins := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(Config{Allowlist: []string{"example"}, Audit: &fakeAudit{}, Pins: pins}); err == nil {
				t.Fatalf("불량 pin %v 수용", pins)
			}
		})
	}
	for name, address := range map[string]string{"CGNAT": pinnedAddress, "RFC1918": "10.0.0.5:8080", "ULA": "[fd00::1]:8080", "public": "93.184.216.34:443"} {
		t.Run("accept "+name, func(t *testing.T) {
			if _, err := New(Config{Allowlist: []string{"example"}, Audit: &fakeAudit{}, Pins: pinMap(t, map[string]string{pinnedDomain: address})}); err != nil {
				t.Fatalf("정상 pin 거부: %v", err)
			}
		})
	}
}

func TestParsePinFlag(t *testing.T) {
	pin, err := ParsePinFlag("GW.Example.=" + pinnedAddress)
	if err != nil || pin.Domain != pinnedDomain || pin.Address != netip.MustParseAddrPort(pinnedAddress) {
		t.Fatalf("ParsePinFlag = %+v, %v", pin, err)
	}
	for _, bad := range []string{
		"gw.example", "gw.example=", "=" + pinnedAddress, "gw.example=gateway.local:8080",
		"gw.example=100.64.0.10", "gw.example=100.64.0.10:70000", "gw.example=127.0.0.1:80",
		"10.0.0.1=" + pinnedAddress, "gw.example=[fe80::1%eth0]:80",
	} {
		if _, err := ParsePinFlag(bad); err == nil {
			t.Errorf("불량 --pin %q 수용", bad)
		}
	}
}

func TestAllowlistPermitsMatchesProxyRule(t *testing.T) {
	proxy := mustProxy(t, Config{Allowlist: []string{"example.com"}, Audit: &fakeAudit{}})
	for _, host := range []string{"example.com", "api.example.com", "evil-example.com", "example.com.evil"} {
		if AllowlistPermits(proxy.allowlist, host) != proxy.allowed(host) {
			t.Errorf("AllowlistPermits(%q)가 proxy.allowed와 다름", host)
		}
	}
}
