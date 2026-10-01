package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostGuard(t *testing.T) {
	g := newHostGuard(ParseHosts(" vinx.tail1234.ts.net , ,Box.LAN "))
	// 找一个本机网卡 IP（回环之外的），没有就只测回环。
	var nicIP string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
				nicIP = ipn.IP.String()
				break
			}
		}
	}
	allow := []string{"localhost", "localhost:3100", "LOCALHOST.", "127.0.0.1:3100", "[::1]:3100", "::1",
		"vinx.tail1234.ts.net:3100", "box.lan"}
	if nicIP != "" {
		allow = append(allow, net.JoinHostPort(nicIP, "3100"))
	}
	for _, h := range allow {
		if !g.allowed(h) {
			t.Errorf("%q should be allowed", h)
		}
	}
	for _, h := range []string{"", "evil.example", "evil.example:3100", "127.0.0.1.evil.example", "localhost.evil.example", "203.0.113.9:3100", "[2001:db8::1]:3100"} {
		if g.allowed(h) {
			t.Errorf("%q should be rejected", h)
		}
	}
}

// DNS rebinding：攻击者域名解析到本机后，浏览器发来的 Host 是攻击者域名，必须 403。
func TestForeignHostRejected(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/settings", "/static/app.css", "/login"} {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
		req.Host = "rebind.evil.example:3100"
		resp, err := e.client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: code %d", path, resp.StatusCode)
		}
	}
	if code, _ := e.get(t, "/settings"); code != http.StatusOK {
		t.Fatalf("127.0.0.1 rejected: %d", code)
	}
	// 配置的额外主机名放行。
	s := New(Deps{Store: e.st, Session: e.sess, Clock: e.clk, AllowedHosts: []string{"vinx.tail1234.ts.net"}})
	mux := http.NewServeMux()
	s.Routes(mux)
	for host, want := range map[string]int{"vinx.tail1234.ts.net:3100": http.StatusOK, "other.example": http.StatusForbidden} {
		req := httptest.NewRequest(http.MethodGet, "/settings", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %s: code %d, want %d", host, rec.Code, want)
		}
	}
}
