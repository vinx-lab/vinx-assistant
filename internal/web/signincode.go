package web

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/auth"
	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// 微信验证码登录（spec 0004）：登录页生成 6 位验证码，并用短期 cookie 把它和这个浏览器绑定；
// 主人在微信里把「登录 123456」发给 Bot，收件（ingest）在数据库里把它标记为已确认；
// 页面每 2 秒查询一次，确认后用 cookie 换会话。网页和收件只通过数据库协作，数据库里只存验证码和浏览器随机值的哈希。
//
// cookie 的值是「浏览器随机值.验证码」：同一浏览器刷新时据此复用原来的验证码（库里只有哈希，无法反查原文），
// 不消耗生成额度，也不会因为别人在取码而丢掉自己的码。

const (
	codeCookie = "vinx_signin"
	codeTTL    = 2 * time.Minute
	// 生成验证码的节流：每个来源每分钟 codeRatePerIP 个，全站每分钟 codeRateTotal 个。
	codeRatePerIP = 5
	codeRateTotal = 30
)

// codeThrottle 是生成验证码的节流（只在网页进程内存里，重启清零）。
type codeThrottle struct {
	clk   clock.Clock
	mu    sync.Mutex
	all   []time.Time
	perIP map[string][]time.Time
}

func recent(ts []time.Time, now time.Time) []time.Time {
	kept := ts[:0]
	for _, x := range ts {
		if now.Sub(x) < time.Minute {
			kept = append(kept, x)
		}
	}
	return kept
}

func (t *codeThrottle) allow(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clk.Now()
	if t.perIP == nil {
		t.perIP = map[string][]time.Time{}
	}
	t.all = recent(t.all, now)
	for k, v := range t.perIP { // 顺带清掉过期的来源，map 不会无限长
		if v = recent(v, now); len(v) == 0 {
			delete(t.perIP, k)
		} else {
			t.perIP[k] = v
		}
	}
	if len(t.all) >= codeRateTotal || len(t.perIP[ip]) >= codeRatePerIP {
		return false
	}
	t.all = append(t.all, now)
	t.perIP[ip] = append(t.perIP[ip], now)
	return true
}

// clientIP 是请求的来源地址。经回环地址上的反向代理时，取 X-Forwarded-For 里最右边的非回环地址
// （最右边是代理自己追加的，左边的可以被客户端伪造）；否则取 RemoteAddr。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return host
	}
	xff := r.Header.Values("X-Forwarded-For")
	parts := strings.Split(strings.Join(xff, ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		if x := net.ParseIP(p); x != nil && !x.IsLoopback() {
			return x.String()
		}
	}
	return ip.String()
}

// uaSummary 把 User-Agent 缩成「浏览器 / 系统」，给 Bot 的确认回复用；不存完整 UA。
func uaSummary(ua string) string {
	pick := func(rules [][2]string) string {
		for _, r := range rules {
			if strings.Contains(ua, r[0]) {
				return r[1]
			}
		}
		return ""
	}
	browser := pick([][2]string{
		{"MicroMessenger", "微信内置浏览器"}, {"Edg", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"FxiOS", "Firefox"},
		{"CriOS", "Chrome"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}, {"curl/", "curl"},
	})
	system := pick([][2]string{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"HarmonyOS", "鸿蒙"}, {"Android", "Android"}, {"Windows", "Windows"},
		{"Mac OS X", "macOS"}, {"Macintosh", "macOS"}, {"CrOS", "ChromeOS"}, {"Linux", "Linux"},
	})
	if browser == "" {
		browser = "未知浏览器"
	}
	if system == "" {
		system = "未知系统"
	}
	return browser + " / " + system
}

// signinData 是登录页的数据。
type signinData struct {
	Title string
	Next  string

	WeChat      bool   // 微信可用：显示验证码区
	Code        string // 本次验证码；空表示没生成（微信不可用、生成太频繁或同时登录的人太多）
	CodeMsg     string // 验证码区的提示或错误
	HasPassword bool

	PasswordOpen  bool // 密码区展开（密码登录出错时）
	PasswordError string
}

// CodeText 是要发给 Bot 的完整文字。
func (d signinData) CodeText() string { return auth.CodeText(d.Code) }

// Help 是一直收不到确认时的提示。
func (d signinData) Help() string { return codeHelp }

// renderSignin 渲染登录页：微信可用时显示这个浏览器的验证码（还有效就复用，否则生成新的）。
func (s *Server) renderSignin(w http.ResponseWriter, r *http.Request, status int, a authState, d signinData) {
	w.Header().Set("Cache-Control", "no-store")
	d.Title, d.HasPassword = "登录", a.HasPassword
	d.WeChat = s.wechatReady(r.Context())
	if d.WeChat {
		code, msg, err := s.codeFor(w, r)
		if err != nil {
			s.fail(w, err)
			return
		}
		d.Code = code
		if msg != "" {
			d.CodeMsg = msg
		}
	}
	if !d.WeChat && !d.HasPassword {
		status = http.StatusServiceUnavailable
	}
	s.render(w, status, "signin", d)
}

// codeCookieValue 拆开验证码 cookie：浏览器随机值和验证码。
func codeCookieValue(r *http.Request) (token, code string) {
	c, err := r.Cookie(codeCookie)
	if err != nil {
		return "", ""
	}
	token, code, _ = strings.Cut(c.Value, ".")
	if !auth.ValidCode(code) {
		code = ""
	}
	return token, code
}

// codeFor 返回这个浏览器的验证码：cookie 对应的码还能用就复用；否则在节流和数量上限内生成新码并写 cookie。
// 生不出来时 code 为空，msg 是给用户的说明。
func (s *Server) codeFor(w http.ResponseWriter, r *http.Request) (code, msg string, err error) {
	ctx := r.Context()
	now := s.d.Clock.Now()
	if tok, old := codeCookieValue(r); tok != "" && old != "" {
		ok, err := s.d.Store.LoginCodeMatches(ctx, auth.TokenHash(tok), auth.CodeHash(old), now)
		if err != nil {
			return "", "", err
		}
		if ok {
			return old, "", nil
		}
	}
	ip := clientIP(r)
	if !s.codes.allow(ip) {
		return "", "取验证码太频繁了，请过一分钟再刷新页面。", nil
	}
	for range 5 { // 避开正在用的验证码，免得一条微信确认了别的浏览器
		c, err := auth.NewCode()
		if err != nil {
			return "", "", err
		}
		busy, err := s.d.Store.LoginCodeActive(ctx, auth.CodeHash(c), now)
		if err != nil {
			return "", "", err
		}
		if code = c; !busy {
			break
		}
	}
	tok, err := auth.NewToken()
	if err != nil {
		return "", "", err
	}
	exp := now.Add(codeTTL)
	client := store.LoginClient{IP: ip, UA: uaSummary(r.UserAgent())}
	err = s.d.Store.CreateLoginCode(ctx, auth.CodeHash(code), auth.TokenHash(tok), client, now, exp)
	if errors.Is(err, store.ErrTooManyLoginCodes) {
		return "", "现在同时在登录的人太多，请稍后刷新页面再试。", nil
	}
	if err != nil {
		return "", "", err
	}
	http.SetCookie(w, s.cookieFor(r, codeCookie, tok+"."+code, exp.Add(store.LoginCodeGrace)))
	return code, "", nil
}

// pollCode 用这个浏览器的 cookie 查询验证码；已确认时建立会话并作废验证码。
func (s *Server) pollCode(w http.ResponseWriter, r *http.Request) (store.LoginCodeState, error) {
	tok, _ := codeCookieValue(r)
	if tok == "" {
		return store.LoginCodeGone, nil
	}
	state, err := s.d.Store.PollLoginCode(r.Context(), auth.TokenHash(tok), s.d.Clock.Now())
	if err != nil || state != store.LoginCodeConfirmed {
		return state, err
	}
	if _, err := s.startSession(w, r); err != nil {
		return state, err
	}
	http.SetCookie(w, s.cookieFor(r, codeCookie, "", time.Time{}))
	return state, nil
}

// codeStatus 是登录页脚本每 2 秒查询的接口：pending / ok（带跳转地址）/ expired。
func (s *Server) codeStatus(w http.ResponseWriter, r *http.Request) {
	next := safeBack(r.URL.Query().Get("next"))
	w.Header().Set("Cache-Control", "no-store")
	a, _, err := s.authOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !a.Required || a.SignedIn {
		writeJSON(w, http.StatusOK, map[string]string{"state": "ok", "next": s.d.BasePath + next})
		return
	}
	state, err := s.pollCode(w, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	switch state {
	case store.LoginCodeConfirmed:
		writeJSON(w, http.StatusOK, map[string]string{"state": "ok", "next": s.d.BasePath + next})
	case store.LoginCodePending:
		writeJSON(w, http.StatusOK, map[string]string{"state": "pending"})
	default:
		writeJSON(w, http.StatusGone, map[string]string{"state": "expired"})
	}
}

// codePost 是不支持脚本时的「我已发送」按钮：已确认就登录跳转；还没确认时原样显示同一个验证码。
func (s *Server) codePost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	next := safeBack(r.FormValue("next"))
	a, _, err := s.authOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !a.Required || a.SignedIn {
		s.redirect(w, r, next)
		return
	}
	state, err := s.pollCode(w, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	d := signinData{Next: next}
	switch state {
	case store.LoginCodeConfirmed:
		s.redirect(w, r, next)
		return
	case store.LoginCodePending:
		d.CodeMsg = "还没收到这个验证码。请在微信里发给 Bot 后再点「我已发送」。" + codeHelp
	default:
		d.CodeMsg = "验证码已失效，下面是新的验证码。"
	}
	s.renderSignin(w, r, http.StatusOK, a, d)
}

// codeHelp 是一直收不到确认时的提示（页面脚本 30 秒后也会显示）。
const codeHelp = "收不到？确认 Bot 能正常收消息；或在服务器上运行 vinx-assistant password --no-login。建议另设一个密码作为备用。"
