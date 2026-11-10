package network

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSharedProxyOverrideBypassAndDisabled(t *testing.T) {
	var global, override atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { global.Add(1); fmt.Fprint(w, "global") }))
	defer proxy.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { override.Add(1); fmt.Fprint(w, "override") }))
	defer other.Close()
	policy := harness.NetworkConfig{Proxy: harness.ProxyConfig{Enabled: true, URL: proxy.URL, NoProxy: []string{".internal.test", "10.0.0.0/8"}}}
	for _, tc := range []struct{ override, want string }{{"", "global"}, {other.URL, "override"}} {
		client, err := NewClient(policy, tc.override, false)
		if err != nil {
			t.Fatal(err)
		}
		defer client.CloseIdleConnections()
		response, err := client.Get("http://external.invalid/page")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if string(body) != tc.want {
			t.Fatalf("response=%q want=%q", body, tc.want)
		}
		for _, target := range []string{"http://service.internal.test", "http://10.2.3.4", "http://127.0.0.1"} {
			req, _ := http.NewRequest(http.MethodGet, target, nil)
			u, err := client.Transport.(*http.Transport).Proxy(req)
			if err != nil || u != nil {
				t.Fatalf("bypass failed for %s: %v", target, err)
			}
		}
		if client.Timeout != 0 {
			t.Fatal("network factory imposed a total stream timeout")
		}
	}
	if global.Load() != 1 || override.Load() != 1 {
		t.Fatal("wrong proxy routing")
	}
	policy.Proxy.Enabled = false
	client, err := NewClient(policy, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("disabled tools inherited environment or global proxy")
	}
	if Fingerprint(policy) != "" {
		t.Fatal("disabled policy changes legacy execution identity")
	}
}

func TestSharedProxyHTTPSConnectAuthentication(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credential leaked to origin")
		}
		fmt.Fprint(w, "encrypted response")
	}))
	defer origin.Close()
	var tunnels atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "target.invalid:443" || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Error("invalid CONNECT or proxy authentication")
			w.WriteHeader(407)
			return
		}
		upstream, err := net.Dial("tcp", origin.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			t.Error(err)
			return
		}
		tunnels.Add(1)
		defer conn.Close()
		defer upstream.Close()
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { io.Copy(upstream, conn); upstream.Close(); close(done) }()
		io.Copy(conn, upstream)
		conn.Close()
		<-done
	}))
	defer proxy.Close()
	policy := harness.NetworkConfig{Proxy: harness.ProxyConfig{Enabled: true, URL: strings.Replace(proxy.URL, "http://", "http://user:pass@", 1)}}
	client, err := NewClient(policy, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: "example.com", MinVersion: tls.VersionTLS12}
	response, err := client.Get("https://target.invalid/page")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "encrypted response" || tunnels.Load() != 1 {
		t.Fatal("HTTPS did not use proxy tunnel")
	}
}

func TestProxyFailureDoesNotFallbackAndProbeRedactsCredentials(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	policy := harness.NetworkConfig{Proxy: harness.ProxyConfig{Enabled: true, URL: "http://private-user:private-password@" + address}}
	err = Probe(context.Background(), policy)
	if err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatalf("unexpected probe error: %v", err)
	}
	for _, raw := range []string{"", "file:///tmp/proxy", "http://", "http://proxy.test/path", "http://proxy.test?secret=1"} {
		policy.Proxy.URL = raw
		if Validate(policy) == nil {
			t.Errorf("accepted invalid proxy %q", raw)
		}
	}
}

func TestProxyBypassValidation(t *testing.T) {
	for _, entry := range []string{"localhost", "127.0.0.1", "::1", "[::1]:8080", ".internal.test", "*.internal.test", "service:8080", "10.0.0.0/8", "2001:db8::/32", "*"} {
		if !validBypass(entry) {
			t.Errorf("rejected valid bypass %q", entry)
		}
	}
	for _, entry := range []string{"http://internal.test", "10.0.0.0/33", "service:invalid", "service:65536", "service:0", "host/path", "host,,other", "a..b", "in*ternal.test", "[::invalid]:8080"} {
		if validBypass(entry) {
			t.Errorf("accepted invalid bypass %q", entry)
		}
	}
}
