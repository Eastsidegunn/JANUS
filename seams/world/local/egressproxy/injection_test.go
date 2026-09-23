package egressproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const injectionSentinel = "sk-secret-oauth-value-DO-NOT-LEAK-42"

// TestForwardInjectionAttachesHeaderAndReSendsOverTLS is 완료 기준 ②: a request
// through the forward path to a declared injection domain gets the credential
// header attached and is re-sent to a real (fake) TLS upstream. The agent sent
// no credential; the proxy supplied it. The audit record carries metadata only.
func TestForwardInjectionAttachesHeaderAndReSendsOverTLS(t *testing.T) {
	var gotAuth atomic.Value
	gotAuth.Store("")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	upstreamAddr := strings.TrimPrefix(upstream.URL, "https://")

	audit := &fakeAudit{}
	proxy := mustProxy(t, Config{
		Allowlist: []string{"api.anthropic.com"},
		Audit:     audit,
		Resolver:  fakeResolver{"api.anthropic.com": {{IP: net.ParseIP("93.184.216.34")}}},
		Inject: []InjectRule{{
			Domain: "api.anthropic.com", Header: "Authorization", ValuePrefix: "Bearer ",
			CredentialName: "CLAUDE_CODE_OAUTH_TOKEN",
		}},
		Credentials: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": injectionSentinel},
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			// The proxy dials the resolved public IP on 443; the test reroutes that
			// to the fake TLS upstream. TLS is performed by the proxy transport.
			if !strings.HasSuffix(address, ":443") {
				t.Fatalf("injection dial이 443이 아님: %s", address)
			}
			return net.Dial("tcp", upstreamAddr)
		},
	})

	// The agent request is plaintext http and carries no credential of its own.
	request := httptest.NewRequest(http.MethodPost, "http://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "ok" {
		t.Fatalf("injection forward = %d %q", response.Code, response.Body.String())
	}
	if got := gotAuth.Load().(string); got != "Bearer "+injectionSentinel {
		t.Fatalf("upstream이 받은 Authorization = %q, want Bearer <secret>", got)
	}
	// 완료 기준(감사에 헤더 값 금지): the audit attempt is metadata only.
	attempts := audit.snapshot()
	if len(attempts) != 1 || attempts[0].Decision != DecisionAllow || attempts[0].Domain != "api.anthropic.com" {
		t.Fatalf("audit attempt 위반: %+v", attempts)
	}
	assertNoSecret(t, attempts)
}

// TestUndeclaredDomainNotInjected is 완료 기준 ④: an allowed but undeclared
// domain is forwarded as plaintext http on port 80 with no injected header.
func TestUndeclaredDomainNotInjected(t *testing.T) {
	forwarded := make(chan *http.Request, 1)
	audit := &fakeAudit{}
	proxy := mustProxy(t, Config{
		Allowlist: []string{"api.anthropic.com", "other.example"},
		Audit:     audit,
		Resolver:  fakeResolver{"other.example": {{IP: net.ParseIP("93.184.216.34")}}},
		Inject: []InjectRule{{
			Domain: "api.anthropic.com", Header: "Authorization", ValuePrefix: "Bearer ",
			CredentialName: "CLAUDE_CODE_OAUTH_TOKEN",
		}},
		Credentials: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": injectionSentinel},
		Dial: func(_ context.Context, network, address string) (net.Conn, error) {
			if address != "93.184.216.34:80" {
				t.Fatalf("미선언 도메인이 평문 80으로 가지 않음: %s", address)
			}
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				req, err := http.ReadRequest(bufio.NewReader(server))
				if err != nil {
					return
				}
				req.Body.Close()
				forwarded <- req
				_, _ = server.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
			}()
			return client, nil
		},
	})
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://other.example/x", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("미선언 도메인 forward = %d", response.Code)
	}
	req := <-forwarded
	if req.Header.Get("Authorization") != "" {
		t.Fatalf("미선언 도메인에 credential 헤더가 주입됨: %q", req.Header.Get("Authorization"))
	}
	assertNoSecret(t, audit.snapshot())
}

// TestConnectToInjectionDomainDenied is the bypass closure: CONNECT to an
// injection domain is denied (no dial) so the agent cannot open an opaque TLS
// tunnel that skips header injection. A non-injection allowed domain still
// tunnels normally.
func TestConnectToInjectionDomainDenied(t *testing.T) {
	dials := 0
	audit := &fakeAudit{}
	proxy := mustProxy(t, Config{
		Allowlist: []string{"api.anthropic.com", "plain.example"},
		Audit:     audit,
		Resolver: fakeResolver{
			"api.anthropic.com": {{IP: net.ParseIP("93.184.216.34")}},
			"plain.example":     {{IP: net.ParseIP("93.184.216.34")}},
		},
		Inject: []InjectRule{{
			Domain: "api.anthropic.com", Header: "Authorization", ValuePrefix: "Bearer ",
			CredentialName: "CLAUDE_CODE_OAUTH_TOKEN",
		}},
		Credentials: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": injectionSentinel},
		Dial: func(context.Context, string, string) (net.Conn, error) {
			dials++
			return nil, context.Canceled
		},
	})
	request := httptest.NewRequest(http.MethodConnect, "http://proxy.invalid", nil)
	request.Host = "api.anthropic.com:443"
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("주입 대상 도메인 CONNECT status = %d, want 403", response.Code)
	}
	if dials != 0 {
		t.Fatalf("거부된 CONNECT가 dial함: %d", dials)
	}
	attempts := audit.snapshot()
	if len(attempts) != 1 || attempts[0].Decision != DecisionDeny || attempts[0].Reason == "" {
		t.Fatalf("주입 도메인 CONNECT deny audit 위반: %+v", attempts)
	}
	// A non-injection allowed domain is not affected by the CONNECT closure: it
	// passes authorization (dial attempted, then fails on our stub).
	nonInj := httptest.NewRequest(http.MethodConnect, "http://proxy.invalid", nil)
	nonInj.Host = "plain.example:443"
	proxy.ServeHTTP(httptest.NewRecorder(), nonInj)
	if dials != 1 {
		t.Fatalf("비주입 허용 도메인 CONNECT가 dial되지 않음: dials=%d", dials)
	}
}

// TestInjectionFailsClosedWithoutCredential proves an injection rule whose
// credential value is missing refuses construction rather than silently
// forwarding without the header.
func TestInjectionFailsClosedWithoutCredential(t *testing.T) {
	_, err := New(Config{
		Allowlist: []string{"api.anthropic.com"},
		Audit:     &fakeAudit{},
		Inject:    []InjectRule{{Domain: "api.anthropic.com", Header: "Authorization", CredentialName: "MISSING"}},
	})
	if err == nil {
		t.Fatal("credential 없는 inject 규칙이 수락됨")
	}
}

func assertNoSecret(t *testing.T, attempts []Attempt) {
	t.Helper()
	encoded, err := json.Marshal(attempts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), injectionSentinel) {
		t.Fatalf("credential 값이 audit 레코드에 유출됨: %s", encoded)
	}
}
