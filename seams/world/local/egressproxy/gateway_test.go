package egressproxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A real HTTP receiver verifies transparent standard API forwarding. DNS/dial
// are controlled test dependencies; this is not a container isolation smoke.
func TestGatewayForwardPreservesAuthAndDeniesOtherDestinations(t *testing.T) {
	const key = "gateway-access-sentinel-23"
	received := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"messages":[]}` {
			t.Error("API body changed")
		}
		received <- r.Clone(r.Context())
		w.Header().Set("Proxy-Authenticate", "discard")
		_, _ = io.WriteString(w, `{"content":[]}`)
	}))
	defer upstream.Close()
	audit := &fakeAudit{}
	dials := 0
	proxy := mustProxy(t, Config{
		Allowlist: []string{"gateway.example"}, Audit: audit,
		Resolver: fakeResolver{"gateway.example": {{IP: net.ParseIP("93.184.216.34")}}},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials++
			if address != "93.184.216.34:8317" {
				t.Error("gateway target changed")
			}
			return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
		},
	})
	request := httptest.NewRequest(http.MethodPost, "http://gateway.example:8317/v1/messages?beta=true", strings.NewReader(`{"messages":[]}`))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("Proxy-Authorization", "discard")
	request.Header.Set("Connection", "keep-alive")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{"content":[]}` {
		t.Fatal("forward failed")
	}
	got := <-received
	if len(got.Header.Values("Authorization")) != 1 || got.Header.Get("Authorization") != "Bearer "+key || got.Header.Get("anthropic-version") != "2023-06-01" || got.URL.RequestURI() != "/v1/messages?beta=true" {
		t.Fatal("proxy modified standard API request")
	}
	if got.Header.Get("Proxy-Authorization") != "" || got.Header.Get("Connection") != "" || response.Header().Get("Proxy-Authenticate") != "" {
		t.Fatal("hop header survived")
	}
	for _, host := range []string{"api.anthropic.com", "api.openai.com", "gateway.example.attacker.test", "notgateway.example"} {
		for _, method := range []string{http.MethodGet, http.MethodConnect} {
			target := "http://" + host + "/v1/messages"
			req := httptest.NewRequest(method, target, nil)
			if method == http.MethodConnect {
				req.Host = host + ":443"
			}
			req.Header.Set("Authorization", "Bearer "+key)
			res := httptest.NewRecorder()
			proxy.ServeHTTP(res, req)
			if res.Code != http.StatusForbidden {
				t.Fatal("outside gateway allowlist accepted")
			}
		}
	}
	if dials != 1 {
		t.Fatal("denied request dialed")
	}
	attempts := audit.snapshot()
	if len(attempts) != 9 || attempts[0].Decision != DecisionAllow {
		t.Fatal("missing audit")
	}
	for _, attempt := range attempts[1:] {
		if attempt.Decision != DecisionDeny {
			t.Fatal("denial not audited")
		}
	}
	data, err := json.Marshal(attempts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), key) || strings.Contains(string(data), "messages") || strings.Contains(string(data), "Authorization") {
		t.Fatal("audit contains credential or request content")
	}
}
