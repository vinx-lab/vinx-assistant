package web

import (
	"net"
	"net/http"
	"strings"
	"sync"
)

// hostGuard 检查请求的 Host 头，防 DNS rebinding：攻击者的域名临时解析到本机后，浏览器会把它当同源，
// 绕过 CrossOriginProtection。允许 localhost、回环地址、本机网卡 IP，以及配置的额外主机名。
type hostGuard struct {
	extra map[string]bool // localhost、回环地址和 VINX_ALLOWED_HOSTS

	mu  sync.Mutex
	ips map[string]bool // 本机网卡 IP；遇到不认识的 IP 时重新枚举一次（网卡可能在启动后才起来，如 tailscale0）
}

func newHostGuard(extra []string) *hostGuard {
	g := &hostGuard{extra: map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}}
	for _, h := range extra {
		if h = normHost(h); h != "" {
			g.extra[h] = true
		}
	}
	g.ips = interfaceIPs()
	return g
}

// ParseHosts 解析逗号分隔的主机名列表（VINX_ALLOWED_HOSTS），忽略空项。
func ParseHosts(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func interfaceIPs() map[string]bool {
	out := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out[ipn.IP.String()] = true
		}
	}
	return out
}

// normHost 去掉端口、方括号和末尾的点，转小写；IP 统一成标准写法。
func normHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"), ".")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

func (g *hostGuard) allowed(host string) bool {
	h := normHost(host)
	if h == "" {
		return false
	}
	if g.extra[h] {
		return true
	}
	if net.ParseIP(h) == nil {
		return false // 主机名只认配置里的
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ips[h] {
		return true
	}
	g.ips = interfaceIPs()
	return g.ips[h]
}

func (g *hostGuard) handler(h http.Handler, s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.allowed(r.Host) {
			s.d.Log.Warn("拒绝了不认识的 Host", "host", r.Host, "path", r.URL.Path)
			http.Error(w, "Host 不在允许列表里；用域名访问时请把它加进环境变量 VINX_ALLOWED_HOSTS（逗号分隔）", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}
