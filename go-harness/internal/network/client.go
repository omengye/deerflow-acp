// Package network constructs host-owned HTTP transports without mutating global
// transports or process environment. Disabling the shared proxy retains each
// caller's legacy environment/direct behavior.
package network

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"golang.org/x/net/http/httpproxy"
)

func Clone(config harness.NetworkConfig) harness.NetworkConfig {
	config.Proxy.NoProxy = append([]string(nil), config.Proxy.NoProxy...)
	return config
}

func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || strings.ContainsAny(raw, "\r\n\x00") {
		return errors.New("proxy URL must be an absolute HTTP, HTTPS or SOCKS5 address")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("proxy port must be between 1 and 65535")
		}
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return errors.New("proxy URL must use http, https, socks5 or socks5h")
	}
}

func Validate(config harness.NetworkConfig) error {
	p := config.Proxy
	if len(p.URL) > 8192 || len(p.NoProxy) > 128 {
		return errors.New("proxy configuration exceeds size limits")
	}
	if p.URL != "" || p.Enabled {
		if err := ValidateURL(p.URL); err != nil {
			return err
		}
	}
	for _, entry := range p.NoProxy {
		if !validBypass(entry) {
			return errors.New("proxy bypass entries must be host names, IP addresses or CIDRs")
		}
	}
	return nil
}

func validBypass(entry string) bool {
	if entry == "" || len(entry) > 512 || strings.ContainsAny(entry, "\r\n\x00, \t@?#") || strings.Contains(entry, "://") {
		return false
	}
	if entry == "*" {
		return true
	}
	if strings.Contains(entry, "/") {
		_, err := netip.ParsePrefix(entry)
		return err == nil
	}
	if _, err := netip.ParseAddr(entry); err == nil {
		return true
	}
	host := entry
	if strings.Contains(entry, ":") {
		var port string
		var err error
		host, port, err = net.SplitHostPort(entry)
		if err != nil {
			return false
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
		if _, err := netip.ParseAddr(host); err == nil {
			return true
		}
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "*."), ".")
	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
				return false
			}
		}
	}
	return true
}

// Fingerprint includes the effective shared policy without exposing credentials.
// The empty disabled fingerprint preserves legacy checkpoint identities.
func Fingerprint(config harness.NetworkConfig) string {
	if !config.Proxy.Enabled {
		return ""
	}
	body, _ := json.Marshal(config)
	return fmt.Sprintf("%x", sha256.Sum256(body))
}

// NewClient leaves request deadlines to its caller. Model streams must not
// inherit the short total timeout used by web fetch/search clients.
func NewClient(config harness.NetworkConfig, override string, inheritEnvironment bool) (*http.Client, error) {
	if err := Validate(config); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	proxyURL := override
	if proxyURL == "" && config.Proxy.Enabled {
		proxyURL = config.Proxy.URL
	}
	if proxyURL != "" {
		if err := ValidateURL(proxyURL); err != nil {
			return nil, err
		}
		parsed, _ := url.Parse(proxyURL)
		transport.Proxy = http.ProxyURL(parsed)
		if config.Proxy.Enabled {
			policy := (&httpproxy.Config{HTTPProxy: proxyURL, HTTPSProxy: proxyURL, NoProxy: strings.Join(config.Proxy.NoProxy, ",")}).ProxyFunc()
			transport.Proxy = func(req *http.Request) (*url.URL, error) { return policy(req.URL) }
		}
	} else if inheritEnvironment {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{Transport: transport}, nil
}

// Probe verifies HTTPS CONNECT and TLS through the configured proxy. It never
// invokes a model API or uses provider credentials. Bypass entries are ignored
// for this explicit proxy test, and no direct fallback is attempted.
func Probe(ctx context.Context, config harness.NetworkConfig) error {
	if !config.Proxy.Enabled {
		return errors.New("enable the proxy before testing")
	}
	config = Clone(config)
	config.Proxy.NoProxy = nil
	client, err := NewClient(config, "", false)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/", nil)
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("proxy connection failed; check address, credentials and connectivity")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode >= 400 {
		return fmt.Errorf("proxy test returned HTTP %d", response.StatusCode)
	}
	return nil
}
