package egressproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAudit struct {
	mu       sync.Mutex
	attempts []Attempt
	err      error
	order    *[]string
}

func (f *fakeAudit) Submit(_ context.Context, attempt Attempt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, attempt)
	if f.order != nil {
		*f.order = append(*f.order, "audit")
	}
	return f.err
}

func (f *fakeAudit) snapshot() []Attempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Attempt(nil), f.attempts...)
}

type fakeResolver map[string][]net.IPAddr

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addresses, ok := f[host]
	if !ok {
		return nil, errors.New("not found")
	}
	return addresses, nil
}

func TestAllowlistExactAndLabelBoundary(t *testing.T) {
	proxy := mustProxy(t, Config{Allowlist: []string{"Example.COM."}, Audit: &fakeAudit{}})
	for _, host := range []string{"example.com", "api.example.com", "deep.api.example.com"} {
		if !proxy.allowed(host) {
			t.Errorf("허용 domain 거부: %s", host)
		}
	}
	for _, host := range []string{"evil-example.com", "example.com.evil", "notexample.com"} {
		if proxy.allowed(host) {
			t.Errorf("suffix 우회 허용: %s", host)
		}
	}
}

func TestDeniedTargetsNeverDialAndAreAudited(t *testing.T) {
	private := []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}
	metadata := []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}
	loopback := []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}
	publicAndPrivate := []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("192.168.1.1")}}
	cases := []struct {
		name, url string
		allow     []string
		resolve   fakeResolver
	}{
		{"allowlist 밖", "http://denied.example/path", []string{"allowed.example"}, fakeResolver{}},
		{"suffix 우회", "http://evil-example.com/", []string{"example.com"}, fakeResolver{}},
		{"IP literal", "http://93.184.216.34/", []string{"allowed.example"}, fakeResolver{}},
		{"private", "http://private.example/", []string{"private.example"}, fakeResolver{"private.example": private}},
		{"metadata", "http://metadata.example/", []string{"metadata.example"}, fakeResolver{"metadata.example": metadata}},
		{"loopback", "http://loop.example/", []string{"loop.example"}, fakeResolver{"loop.example": loopback}},
		{"mixed rebinding", "http://mixed.example/", []string{"mixed.example"}, fakeResolver{"mixed.example": publicAndPrivate}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			audit := &fakeAudit{}
			dials := 0
			proxy := mustProxy(t, Config{
				Allowlist: tc.allow, Audit: audit, Resolver: tc.resolve,
				Dial: func(context.Context, string, string) (net.Conn, error) {
					dials++
					return nil, errors.New("must not dial")
				},
			})
			request := httptest.NewRequest(http.MethodGet, tc.url, nil)
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || dials != 0 {
				t.Fatalf("response=%d dials=%d", response.Code, dials)
			}
			attempts := audit.snapshot()
			if len(attempts) != 1 || attempts[0].Decision != DecisionDeny {
				t.Fatalf("deny audit 누락: %+v", attempts)
			}
		})
	}
}

func TestAuditFailurePreventsDial(t *testing.T) {
	audit := &fakeAudit{err: errors.New("queue full")}
	dials := 0
	proxy := mustProxy(t, Config{
		Allowlist: []string{"allowed.example"}, Audit: audit,
		Resolver: fakeResolver{"allowed.example": {{IP: net.ParseIP("93.184.216.34")}}},
		Dial: func(context.Context, string, string) (net.Conn, error) {
			dials++
			return nil, errors.New("must not dial")
		},
	})
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://allowed.example/", nil))
	if dials != 0 {
		t.Fatalf("audit enqueue 실패 뒤 dial=%d", dials)
	}
	if len(audit.snapshot()) != 1 {
		t.Fatalf("audit 시도 자체가 없음: %+v", audit.snapshot())
	}
}

func TestAuditHappensBeforeDialAndCarriesMetadataOnly(t *testing.T) {
	var order []string
	audit := &fakeAudit{order: &order}
	proxy := mustProxy(t, Config{
		Allowlist: []string{"allowed.example"}, Audit: audit,
		Resolver: fakeResolver{"allowed.example": {{IP: net.ParseIP("93.184.216.34")}}},
		Dial: func(context.Context, string, string) (net.Conn, error) {
			order = append(order, "dial")
			return nil, errors.New("test dial")
		},
		Now: func() time.Time { return time.UnixMilli(1234) },
	})
	request := httptest.NewRequest(http.MethodPost, "http://allowed.example/path", strings.NewReader("secret-body"))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if !reflect.DeepEqual(order, []string{"audit", "dial"}) {
		t.Fatalf("audit-before-dial 순서 위반: %v", order)
	}
	want := Attempt{Domain: "allowed.example", Method: http.MethodPost, RequestBytes: 11, AtUnixMs: 1234, Decision: DecisionAllow}
	if got := audit.snapshot(); len(got) != 1 || got[0] != want {
		t.Fatalf("audit metadata=%+v want=%+v", got, want)
	}
}

func TestConnectOnlyAllowsDomainPort443(t *testing.T) {
	audit := &fakeAudit{}
	dials := 0
	proxy := mustProxy(t, Config{
		Allowlist: []string{"allowed.example"}, Audit: audit,
		Resolver: fakeResolver{"allowed.example": {{IP: net.ParseIP("93.184.216.34")}}},
		Dial: func(context.Context, string, string) (net.Conn, error) {
			dials++
			return nil, errors.New("test dial")
		},
	})
	for _, target := range []string{"allowed.example:22", "93.184.216.34:443", "allowed.example"} {
		request := httptest.NewRequest(http.MethodConnect, "http://proxy.invalid", nil)
		request.Host = target
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("CONNECT %q status=%d", target, response.Code)
		}
	}
	if dials != 0 {
		t.Fatalf("금지 CONNECT가 dial함: %d", dials)
	}
}

func TestHTTPForwardingUsesPinnedResolvedAddress(t *testing.T) {
	audit := &fakeAudit{}
	proxy := mustProxy(t, Config{
		Allowlist: []string{"allowed.example"}, Audit: audit,
		Resolver: fakeResolver{"allowed.example": {{IP: net.ParseIP("93.184.216.34")}}},
		Dial: func(_ context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != "93.184.216.34:80" {
				t.Fatalf("pinned dial = %s %s", network, address)
			}
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
		},
	})
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://allowed.example/path", nil))
	if response.Code != http.StatusOK || response.Body.String() != "ok" {
		t.Fatalf("HTTP forwarding = %d %q", response.Code, response.Body.String())
	}
}

func TestHTTPSConnectTunnel(t *testing.T) {
	audit := &fakeAudit{}
	proxy := mustProxy(t, Config{
		Allowlist: []string{"allowed.example"}, Audit: audit,
		Resolver: fakeResolver{"allowed.example": {{IP: net.ParseIP("93.184.216.34")}}},
		Dial: func(_ context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != "93.184.216.34:443" {
				t.Fatalf("CONNECT pinned dial = %s %s", network, address)
			}
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
	if _, err := connection.Write([]byte("CONNECT allowed.example:443 HTTP/1.1\r\nHost: allowed.example:443\r\n\r\n")); err != nil {
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
}

func mustProxy(t *testing.T, config Config) *Proxy {
	t.Helper()
	proxy, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return proxy
}
