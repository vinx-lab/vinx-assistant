package web

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vinx-lab/vinx-assistant/internal/auth"
	"github.com/vinx-lab/vinx-assistant/internal/session"
)

// 网页登录（spec 0003、0004）：「需要登录」开关关闭时一切照旧，只在顶栏下方提示；打开后除 /signin、/static/、/healthz 外都要登录。
// 登录方式有微信验证码（signincode.go）和密码两种。会话令牌放在 cookie 里，数据库只存它的 sha256；
// 密码记录存在 settings 表的 auth 键，开关是 login_required 键。
// 命令行 `vinx-assistant password` 直接改数据库，所以每个请求都现读开关、密码记录和会话，不做进程内缓存。

const (
	sessionCookie = "vinx_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// guardKind 决定未登录时怎么回应。
type guardKind int

const (
	guardPage guardKind = iota // GET 页面：303 到登录页，带 next
	guardForm                  // POST 表单：303 到登录页
	guardAPI                   // 前端脚本 fetch 的接口：401
)

// authState 是当前请求的登录情况，由 guard 放进 context。
type authState struct {
	Required    bool   // 「需要登录」已打开
	HasPassword bool   // 已设密码
	SignedIn    bool   // 带着有效会话
	TokenHash   string // 当前会话（SignedIn 时）
}

type authKey struct{}

func authFrom(ctx context.Context) authState {
	a, _ := ctx.Value(authKey{}).(authState)
	return a
}

// passwordRecord 读密码记录；没设密码时返回 nil。记录变了（网页或命令行改过密码）就清空失败计数，
// 这样被锁定时用命令行重置密码也能立即解锁。
func (s *Server) passwordRecord(ctx context.Context) (*auth.Record, error) {
	raw, err := s.d.Store.PasswordRecord(ctx)
	if err != nil {
		return nil, err
	}
	s.authMu.Lock()
	if raw != s.lastRecord {
		s.lastRecord = raw
		s.limiter.Reset()
	}
	s.authMu.Unlock()
	if raw == "" {
		return nil, nil
	}
	rec, err := auth.Decode(raw)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// authOf 查当前请求的登录情况。
func (s *Server) authOf(r *http.Request) (authState, *auth.Record, error) {
	var a authState
	req, err := s.d.Store.LoginRequired(r.Context())
	if err != nil {
		return a, nil, err
	}
	rec, err := s.passwordRecord(r.Context())
	if err != nil {
		return a, nil, err
	}
	a.Required, a.HasPassword = req, rec != nil
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		h := auth.TokenHash(c.Value)
		ok, err := s.d.Store.SessionValid(r.Context(), h, s.d.Clock.Now())
		if err != nil {
			return a, rec, err
		}
		if ok {
			a.SignedIn, a.TokenHash = true, h
		}
	}
	return a, rec, nil
}

// wechatReady 报告微信是否可用（已登录且没有暂停），可用时登录页才显示验证码。
func (s *Server) wechatReady(ctx context.Context) bool {
	return s.d.Session != nil && s.d.Session.Status(ctx) == session.StatusOK
}

// guard 是登录检查，放在 Host 白名单和跨站防护之后。读不到开关或密码记录时拒绝访问，不放行。
func (s *Server) guard(kind guardKind, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, _, err := s.authOf(r)
		if err != nil {
			s.fail(w, err)
			return
		}
		if a.Required && !a.SignedIn {
			switch kind {
			case guardAPI:
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录或登录已过期，请刷新页面"})
			case guardPage:
				s.redirect(w, r, signinURL(r.URL.RequestURI()))
			default:
				s.redirect(w, r, "/signin")
			}
			return
		}
		if a.Required {
			// 登录后的内容不进浏览器缓存：共用设备上退出后按返回键或打开附件地址看不到（不需要登录时保持原样）
			w.Header().Set("Cache-Control", "no-store")
		}
		h(w, r.WithContext(context.WithValue(r.Context(), authKey{}, a)))
	})
}

// signinURL 是登录页地址；next 是登录后回到的站内地址（不带前缀）。
func signinURL(next string) string {
	if next = safeBack(next); next == "/" {
		return "/signin"
	}
	return "/signin?next=" + url.QueryEscape(next)
}

// isHTTPS：直接 TLS，或者回环地址上的反向代理声明了 X-Forwarded-Proto: https。别的来源的这个头不可信。
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	if !strings.EqualFold(strings.TrimSpace(proto), "https") {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) cookiePath() string {
	if s.d.BasePath == "" {
		return "/"
	}
	return s.d.BasePath
}

// cookieFor 生成 name 的 cookie；token 为空时生成删除用的 cookie。
func (s *Server) cookieFor(r *http.Request, name, token string, expires time.Time) *http.Cookie {
	c := &http.Cookie{Name: name, Value: token, Path: s.cookiePath(), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r)}
	if token == "" {
		c.MaxAge = -1
	} else {
		c.Expires = expires
		c.MaxAge = int(expires.Sub(s.d.Clock.Now()) / time.Second)
	}
	return c
}

// sessionCookieFor 生成会话 cookie；token 为空时生成删除用的 cookie。
func (s *Server) sessionCookieFor(r *http.Request, token string, expires time.Time) *http.Cookie {
	return s.cookieFor(r, sessionCookie, token, expires)
}

// startSession 为当前浏览器新建会话并写 cookie；返回令牌哈希。浏览器原来带着的会话一并吊销（防会话固定）。
func (s *Server) startSession(w http.ResponseWriter, r *http.Request) (string, error) {
	tok, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.d.Store.DeleteSession(r.Context(), auth.TokenHash(c.Value)); err != nil {
			return "", err
		}
	}
	now := s.d.Clock.Now()
	exp := now.Add(sessionTTL)
	h := auth.TokenHash(tok)
	if err := s.d.Store.CreateSession(r.Context(), h, now, exp); err != nil {
		return "", err
	}
	http.SetCookie(w, s.sessionCookieFor(r, tok, exp))
	return h, nil
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.sessionCookieFor(r, "", time.Time{}))
}

// lockedMsg 是锁定期间给用户看的提示。
func lockedMsg(d time.Duration) string {
	return "密码错误次数过多，已暂停密码登录，请 " + strconv.Itoa(auth.Minutes(d)) + " 分钟后再试。忘记密码可在服务器上运行 vinx-assistant password 重置。"
}

// serialKDF 让 PBKDF2（登录校验、改密码时的哈希）同一时刻最多跑一个，防止并发请求把 CPU 占满。
// 排队时请求被取消则返回 false，fn 不执行。
func (s *Server) serialKDF(ctx context.Context, fn func()) bool {
	select {
	case s.kdfSem <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-s.kdfSem }()
	fn()
	return true
}

// checkPassword 在失败计数的保护下校验密码。返回给用户的错误信息和 HTTP 状态；通过时 msg 为空。
// 先向 limiter 预占一次失败（检查锁定和占位是原子的），校验通过再归还，所以并发请求也绕不过锁定。
func (s *Server) checkPassword(ctx context.Context, rec *auth.Record, pw string, what string) (string, int) {
	tk, d := s.limiter.Begin()
	if d > 0 {
		return lockedMsg(d), http.StatusTooManyRequests
	}
	var ok bool
	if !s.serialKDF(ctx, func() { ok = s.verify(rec, pw) }) {
		s.limiter.Done(tk, true) // 没校验就被取消：不算一次尝试
		return "请求已取消，请重试。", http.StatusServiceUnavailable
	}
	if s.limiter.Done(tk, ok) {
		s.d.Log.Warn("密码错误次数过多，暂停密码登录", "fails", auth.MaxFails, "window", auth.FailWindow.String(), "lock", auth.LockFor.String())
		return lockedMsg(s.limiter.Locked()), http.StatusTooManyRequests
	}
	if ok {
		return "", http.StatusOK
	}
	return what + "不对。", http.StatusUnauthorized
}

// signinPage 是登录页。不需要登录或已经登录时直接回到 next。
func (s *Server) signinPage(w http.ResponseWriter, r *http.Request) {
	next := safeBack(r.URL.Query().Get("next"))
	a, _, err := s.authOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !a.Required || a.SignedIn {
		s.redirect(w, r, next)
		return
	}
	d := signinData{Next: next}
	if l := s.limiter.Locked(); l > 0 && a.HasPassword {
		d.PasswordError, d.PasswordOpen = lockedMsg(l), true
	}
	s.renderSignin(w, r, http.StatusOK, a, d)
}

// signinPost 是密码登录。
func (s *Server) signinPost(w http.ResponseWriter, r *http.Request) {
	next := safeBack(r.FormValue("next"))
	a, rec, err := s.authOf(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !a.Required {
		s.redirect(w, r, next)
		return
	}
	if rec == nil {
		s.renderSignin(w, r, http.StatusUnauthorized, a, signinData{Next: next, PasswordError: "还没有设置密码。", PasswordOpen: true})
		return
	}
	if msg, code := s.checkPassword(r.Context(), rec, r.FormValue("password"), "密码"); msg != "" {
		s.renderSignin(w, r, code, a, signinData{Next: next, PasswordError: msg, PasswordOpen: true})
		return
	}
	if _, err := s.startSession(w, r); err != nil {
		s.fail(w, err)
		return
	}
	s.redirect(w, r, next)
}

// signout 只退出当前设备。
func (s *Server) signout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.d.Store.DeleteSession(r.Context(), auth.TokenHash(c.Value)); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.clearSession(w, r)
	s.redirect(w, r, "/signin")
}

// signoutAll 吊销全部会话（含当前设备），然后到登录页。
func (s *Server) signoutAll(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Store.DeleteAllSessions(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	s.clearSession(w, r)
	if !authFrom(r.Context()).Required {
		s.redirect(w, r, "/settings/password")
		return
	}
	s.redirect(w, r, "/signin")
}

// settingsError 在「登录与密码」小节显示错误。
func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderSettings(w, r, code, "password", st, generalFormOf(st), msg)
}

// passwordSave 设置或修改密码。没设密码时只要两次新密码；已设时还要当前密码，改完吊销其他设备、保留当前设备。
func (s *Server) passwordSave(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r.Context())
	bad := func(code int, msg string) { s.settingsError(w, r, code, msg) }
	newPW, confirm := r.FormValue("new"), r.FormValue("confirm")
	if a.HasPassword {
		rec, err := s.passwordRecord(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		if rec == nil { // 刚被命令行清除
			s.redirect(w, r, "/settings/password")
			return
		}
		if msg, code := s.checkPassword(r.Context(), rec, r.FormValue("current"), "当前密码"); msg != "" {
			bad(code, msg)
			return
		}
	}
	if err := auth.CheckNew(newPW); err != nil {
		bad(http.StatusBadRequest, err.Error())
		return
	}
	if newPW != confirm {
		bad(http.StatusBadRequest, "两次输入的新密码不一致")
		return
	}
	var rec auth.Record
	var err error
	if !s.serialKDF(r.Context(), func() { rec, err = auth.Hash(newPW, s.iter) }) {
		http.Error(w, "请求已取消", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	raw, err := rec.Encode()
	if err != nil {
		s.fail(w, err)
		return
	}
	msg := "pwchanged"
	if a.HasPassword {
		err = s.d.Store.SetPassword(r.Context(), raw, a.TokenHash)
	} else {
		// 首次设置用条件写入：处理期间别人（另一个浏览器或命令行）已经设好密码时不覆盖
		var inserted bool
		inserted, err = s.d.Store.SetPasswordIfUnset(r.Context(), raw)
		if err == nil && !inserted {
			s.redirect(w, r, "/settings/password?msg=pwexists") // 之后由 guard 决定还能不能看设置页
			return
		}
		msg = "pwset"
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if !a.SignedIn { // 当前浏览器直接登录（之后打开「需要登录」也不用再登一次）
		if _, err := s.startSession(w, r); err != nil {
			s.fail(w, err)
			return
		}
	}
	if utf8.RuneCountInString(newPW) < shortPassword {
		msg += "_short" // 不阻止，只提醒（spec 0004 安全审查 F4）
	}
	s.redirect(w, r, "/settings/password?msg="+msg)
}

// shortPassword 以下的密码保存时提醒一句：失败锁定是 15 分钟 10 次，短密码几天内就可能被猜中。
const shortPassword = 8

// loginRequiredSave 打开或关闭「需要登录」。打开要求微信已登录或已设密码，至少一种登录方式可用；打开时当前浏览器保持登录。
func (s *Server) loginRequiredSave(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r.Context())
	on := r.FormValue("on") == "1"
	if on {
		wx := s.d.Session != nil && s.d.Session.Status(r.Context()) != session.StatusNoCred
		if !wx && !a.HasPassword {
			s.settingsError(w, r, http.StatusBadRequest, "开启前需要至少一种登录方式：先在「微信登录」里绑定 ClawBot，或者在下面设置密码。")
			return
		}
		if !a.SignedIn {
			if _, err := s.startSession(w, r); err != nil {
				s.fail(w, err)
				return
			}
		}
	}
	if err := s.d.Store.SetLoginRequired(r.Context(), on); err != nil {
		s.fail(w, err)
		return
	}
	if on {
		s.redirect(w, r, "/settings/password?msg=loginon")
		return
	}
	s.redirect(w, r, "/settings/password?msg=loginoff")
}

// loginIndex 是手机设置首页「登录与密码」一项右侧的值。
func loginIndex(a authState) string {
	if a.Required {
		return "需要登录"
	}
	return "未开启"
}
