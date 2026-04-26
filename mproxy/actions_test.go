package mproxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestProxy(cfg ServerConfig) *CoreHttpServer {
	proxy := NewCoreHttpSever()
	proxy.Logger = log.New(io.Discard, "", 0)
	proxy.Config = &ConfigManager{Current: &cfg}
	return proxy
}

func TestNormalizeIP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "IPv4", raw: " 127.0.0.1 ", want: "127.0.0.1"},
		{name: "IPv6", raw: "2001:db8::1", want: "2001:db8::1"},
		{name: "Invalid", raw: "not-an-ip", want: ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeIP(tc.raw); got != tc.want {
				t.Fatalf("NormalizeIP(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestGetClientIPPriority(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("X-Forwarded-For", " 203.0.113.1 , 203.0.113.2")
	req.Header.Set("X-Real-IP", "198.51.100.20")

	if got := getClientIP(req); got != "203.0.113.1" {
		t.Fatalf("getClientIP() = %q, want %q", got, "203.0.113.1")
	}
}

func TestGetClientIPFallback(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("X-Forwarded-For", "invalid-ip")
	req.Header.Set("X-Real-IP", "198.51.100.20")

	if got := getClientIP(req); got != "198.51.100.20" {
		t.Fatalf("getClientIP() = %q, want %q", got, "198.51.100.20")
	}
}

func TestAccessControllerBlocksHTTPByUserIP(t *testing.T) {
	t.Parallel()

	cfg := ServerConfig{
		AccessEnable: true,
		UserBlockRules: []UserBlockRule{
			{Id: 1, Value: "203.0.113.10", Enable: true},
		},
	}
	proxy := newTestProxy(cfg)
	AddAccessControl(proxy)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/path", nil)
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.10")

	ctx := &Pcontext{
		core_proxy: proxy,
		Req:        req,
		ClientIP:   getClientIP(req),
	}

	_, resp := proxy.filterRequest(req, ctx)
	if resp == nil {
		t.Fatal("expected blocked response, got nil")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("resp.StatusCode = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestAccessControllerBlocksConnectByUserIP(t *testing.T) {
	t.Parallel()

	cfg := ServerConfig{
		AccessEnable: true,
		UserBlockRules: []UserBlockRule{
			{Id: 1, Value: "203.0.113.10", Enable: true},
		},
	}
	proxy := newTestProxy(cfg)
	AddAccessControl(proxy)

	req := httptest.NewRequest(http.MethodConnect, "http://example.com:443", nil)
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.10")

	ctx := &Pcontext{
		core_proxy: proxy,
		Req:        req,
		ClientIP:   getClientIP(req),
	}

	action, host := proxy.httpsHandlers[0].HandleConnect("example.com:443", ctx)
	if action != RejectConnect {
		t.Fatalf("action = %#v, want RejectConnect", action)
	}
	if host != "example.com:443" {
		t.Fatalf("host = %q, want %q", host, "example.com:443")
	}
	if ctx.Resp == nil || ctx.Resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ctx.Resp = %#v, want forbidden response", ctx.Resp)
	}
}

func TestCloseBlockedConnections(t *testing.T) {
	t.Parallel()

	cfg := ServerConfig{
		AccessEnable: true,
		UserBlockRules: []UserBlockRule{
			{Id: 1, Value: "203.0.113.10", Enable: true},
		},
	}
	proxy := newTestProxy(cfg)
	ac := AddAccessControl(proxy)

	blockedClosed := 0
	allowedClosed := 0
	proxy.Connections.Store(int64(1), &ConnectionInfo{
		Session:    1,
		Status:     "Active",
		ClientIP:   "203.0.113.10",
		RemoteAddr: "192.0.2.10:12345",
		OnClose: func() {
			blockedClosed++
		},
	})
	proxy.Connections.Store(int64(2), &ConnectionInfo{
		Session:    2,
		Status:     "Active",
		ClientIP:   "198.51.100.20",
		RemoteAddr: "192.0.2.11:12345",
		OnClose: func() {
			allowedClosed++
		},
	})

	ac.CloseBlockedConnections()

	if blockedClosed != 1 {
		t.Fatalf("blockedClosed = %d, want 1", blockedClosed)
	}
	if allowedClosed != 0 {
		t.Fatalf("allowedClosed = %d, want 0", allowedClosed)
	}
	if _, ok := proxy.Connections.Load(int64(1)); ok {
		t.Fatal("blocked connection should be removed")
	}
	if _, ok := proxy.Connections.Load(int64(2)); !ok {
		t.Fatal("allowed connection should remain")
	}
}
