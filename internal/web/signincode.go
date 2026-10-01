package web

import (
	"net/http"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/auth"
	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// 微信验证码登录（spec 0004）：登录页生成 6 位验证码，并用短期 cookie 把它和这个浏览器绑定；
// 主人在微信里把验证码发给 Bot，收件（ingest）在数据库里把它标记为已确认；
// 页面每 2 秒查询一次，确认后用 cookie 换会话。网页和收件只通过数据库协作，数据库里只存验证码和浏览器随机值的哈希。

const (
	codeCookie = "vinx_signin"
	codeTTL    = 2 * time.Minute
	// codeRate 是每分钟最多生成的验证码数（全站），防止刷表。
	codeRate = 10
)

// codeThrottle 是生成验证码的节流：任意一分钟内最多 codeRate 个。
type codeThrottle struct {
	clk clock.Clock
	mu  sync.Mutex
	ts  []time.Time
}

func (t *codeThrottle) allow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clk.Now()
	kept := t.ts[:0]
	for _, x := range t.ts {
		if now.Sub(x) < time.Minute {
			kept = append(kept, x)
		}
	}
	t.ts = kept
	if len(t.ts) >= codeRate {
		return false
	}
	t.ts = append(t.ts, now)
	return true
}

// signinData 是登录页的数据。
type signinData struct {
	Title string
	Next  string

	WeChat      bool   // 微信可用：显示验证码区
	Code        string // 本次验证码；空表示没生成（微信不可用或生成太频繁）
	CodeMsg     string // 验证码区的提示或错误
	HasPassword bool

	PasswordOpen  bool // 密码区展开（密码登录出错时）
	PasswordError string
}

// renderSignin 渲染登录页；d.Code 为空且微信可用时生成新的验证码。
func (s *Server) renderSignin(w http.ResponseWriter, r *http.Request, status int, a authState, d signinData) {
	d.Title, d.HasPassword = "登录", a.HasPassword
	d.WeChat = s.wechatReady(r.Context())
	if d.WeChat && d.Code == "" {
		code, err := s.newCode(w, r)
		if err != nil {
			s.fail(w, err)
			return
		}
		if code == "" {
			d.CodeMsg = "取验证码太频繁了，请过一分钟再刷新页面。"
		}
		d.Code = code
	}
	if !d.WeChat && !d.HasPassword {
		status = http.StatusServiceUnavailable
	}
	s.render(w, status, "signin", d)
}

// newCode 生成验证码并写 cookie；节流时返回空串。这个浏览器原来的验证码作废。
func (s *Server) newCode(w http.ResponseWriter, r *http.Request) (string, error) {
	if !s.codes.allow() {
		return "", nil
	}
	ctx := r.Context()
	now := s.d.Clock.Now()
	if c, err := r.Cookie(codeCookie); err == nil && c.Value != "" {
		if err := s.d.Store.DeleteLoginCode(ctx, auth.TokenHash(c.Value)); err != nil {
			return "", err
		}
	}
	var code string
	for range 5 { // 避开正在用的验证码，免得一条微信确认了别的浏览器
		c, err := auth.NewCode()
		if err != nil {
			return "", err
		}
		busy, err := s.d.Store.LoginCodeActive(ctx, auth.CodeHash(c), now)
		if err != nil {
			return "", err
		}
		if code = c; !busy {
			break
		}
	}
	tok, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	exp := now.Add(codeTTL)
	if err := s.d.Store.CreateLoginCode(ctx, auth.CodeHash(code), auth.TokenHash(tok), now, exp); err != nil {
		return "", err
	}
	http.SetCookie(w, s.cookieFor(r, codeCookie, tok, exp.Add(store.LoginCodeGrace)))
	return code, nil
}

// pollCode 用这个浏览器的 cookie 查询验证码；已确认时建立会话并作废验证码。
func (s *Server) pollCode(w http.ResponseWriter, r *http.Request) (store.LoginCodeState, error) {
	c, err := r.Cookie(codeCookie)
	if err != nil || c.Value == "" {
		return store.LoginCodeGone, nil
	}
	state, err := s.d.Store.PollLoginCode(r.Context(), auth.TokenHash(c.Value), s.d.Clock.Now())
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
		// 表单带回页面上的验证码；核对它确实属于这个浏览器再显示，不能借此往页面里塞任意内容
		code, ok := auth.ParseCode(r.FormValue("code"))
		if c, err := r.Cookie(codeCookie); ok && err == nil {
			if match, err := s.d.Store.LoginCodeMatches(r.Context(), auth.TokenHash(c.Value), auth.CodeHash(code), s.d.Clock.Now()); err == nil && match {
				d.Code = code
			}
		}
		d.CodeMsg = "还没收到这个验证码。请在微信里发给 Bot 后再点「我已发送」。"
		if d.Code == "" {
			d.CodeMsg = "验证码已失效，已换成新的。"
		}
	default:
		d.CodeMsg = "验证码已失效，已换成新的。"
	}
	s.renderSignin(w, r, http.StatusOK, a, d)
}
