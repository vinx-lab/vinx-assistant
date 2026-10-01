package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/auth"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

var codeRe = regexp.MustCompile(`<p class="code"[^>]*>(\d{6})</p>`)

func cookieNamed(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// bindWeChat 让微信处于已登录状态（登录页才会显示验证码）。
func (e *env) bindWeChat(t *testing.T) {
	t.Helper()
	if err := e.sess.SaveCred(t.Context(), e.ilink.Cred()); err != nil {
		t.Fatal(err)
	}
}

// requireLogin 只打开「需要登录」，不设密码。
func (e *env) requireLogin(t *testing.T) {
	t.Helper()
	if err := e.st.SetLoginRequired(t.Context(), true); err != nil {
		t.Fatal(err)
	}
}

// takeCode 打开登录页取验证码，返回验证码和绑定浏览器的 cookie。
func (e *env) takeCode(t *testing.T, base, next string) (string, *http.Cookie) {
	t.Helper()
	resp, body := e.do(t, "GET", base+"/signin?next="+url.QueryEscape(next), nil, nil)
	m := codeRe.FindStringSubmatch(body)
	c := cookieNamed(resp, codeCookie)
	if resp.StatusCode != 200 || m == nil || c == nil {
		t.Fatalf("取验证码：%d cookie=%v\n%s", resp.StatusCode, c, body)
	}
	return m[1], c
}

type pollResult struct {
	code    int
	state   string
	next    string
	session *http.Cookie
}

func (e *env) poll(t *testing.T, base, next string, c *http.Cookie) pollResult {
	t.Helper()
	resp, body := e.do(t, "GET", base+"/signin/code/status?next="+url.QueryEscape(next), nil, c)
	var j map[string]string
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		t.Fatalf("status 不是 JSON：%s", body)
	}
	return pollResult{resp.StatusCode, j["state"], j["next"], sessionOf(resp)}
}

func TestLoginSwitch(t *testing.T) {
	e := newEnv(t)
	post := func(on string, c *http.Cookie) *http.Response {
		t.Helper()
		resp, _ := e.do(t, "POST", "/settings/login", url.Values{"on": {on}}, c)
		return resp
	}
	required := func() bool {
		on, _ := e.st.LoginRequired(t.Context())
		return on
	}

	// 没有任何登录方式：拒绝开启
	resp, body := e.do(t, "POST", "/settings/login", url.Values{"on": {"1"}}, nil)
	if resp.StatusCode != http.StatusBadRequest || required() {
		t.Fatalf("无登录方式：%d", resp.StatusCode)
	}
	mustContain(t, body, "至少一种登录方式")

	// 只有密码：可以开启，当前浏览器保持登录，其他浏览器要登录
	rec, _ := auth.Hash("p", 1000)
	raw, _ := rec.Encode()
	e.st.SetPassword(t.Context(), raw, "")
	resp = post("1", nil)
	c := sessionOf(resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/password?msg=loginon" || c == nil || !required() {
		t.Fatalf("开启：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if e.code(t, "/", c) != 200 || e.code(t, "/", nil) != http.StatusSeeOther {
		t.Fatal("开启后当前浏览器应保持登录、其他浏览器要登录")
	}
	_, body = e.do(t, "GET", "/settings/password?msg=loginon", nil, c)
	mustContain(t, body, "已开启登录", "关闭登录要求", "退出所有设备")
	// 关闭
	if resp := post("0", c); resp.Header.Get("Location") != "/settings/password?msg=loginoff" || required() {
		t.Fatal("关闭失败")
	}
	if e.code(t, "/", nil) != 200 {
		t.Fatal("关闭后应照常访问")
	}

	// 只有微信（没密码）也可以开启
	e.st.ClearPassword(t.Context())
	e.bindWeChat(t)
	if resp := post("1", nil); resp.StatusCode != http.StatusSeeOther || !required() {
		t.Fatalf("只有微信：%d", resp.StatusCode)
	}
	// 跨站提交被拒
	e.st.SetLoginRequired(t.Context(), false)
	resp, _ = e.postFrom(t, "/settings/login", url.Values{"on": {"1"}}, "cross-site")
	if resp.StatusCode != http.StatusForbidden || required() {
		t.Fatalf("跨站开启：%d", resp.StatusCode)
	}
}

func TestSigninPageModes(t *testing.T) {
	cases := []struct {
		name         string
		wechat       string // ok / paused / ""
		password     bool
		status       int
		code, pwForm bool
		want         string
	}{
		{"微信和密码", "ok", true, 200, true, true, "微信验证码登录"},
		{"只有微信", "ok", false, 200, true, false, "在微信里把这串数字发给 Bot"},
		{"只有密码", "", true, 200, false, true, "暂时只能用密码登录"},
		{"微信暂停中", "paused", true, 200, false, true, "暂时只能用密码登录"},
		{"都没有", "", false, 503, false, false, "vinx-assistant password --no-login"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.requireLogin(t)
			if c.wechat != "" {
				e.bindWeChat(t)
			}
			if c.wechat == "paused" {
				e.sess.Pause(t.Context())
			}
			if c.password {
				rec, _ := auth.Hash("p", 1000)
				raw, _ := rec.Encode()
				e.st.SetPassword(t.Context(), raw, "")
			}
			resp, body := e.do(t, "GET", "/signin", nil, nil)
			if resp.StatusCode != c.status {
				t.Fatalf("status %d", resp.StatusCode)
			}
			mustContain(t, body, c.want)
			if got := codeRe.MatchString(body); got != c.code {
				t.Errorf("验证码显示 %v", got)
			}
			if got := cookieNamed(resp, codeCookie) != nil; got != c.code {
				t.Errorf("验证码 cookie %v", got)
			}
			if got := strings.Contains(body, `action="/signin"`); got != c.pwForm {
				t.Errorf("密码表单 %v", got)
			}
			if c.code && c.pwForm {
				// 有验证码时密码区折叠在下面
				mustContain(t, body, `<details class="card signin-pw">`)
				if strings.Index(body, `class="code"`) > strings.Index(body, "signin-pw") {
					t.Error("密码区应在验证码下方")
				}
			}
		})
	}
}

func TestCodeSignin(t *testing.T) {
	e := newEnv(t)
	e.bindWeChat(t)
	e.requireLogin(t)
	code, mine := e.takeCode(t, "", "/search?q=x")
	if !mine.HttpOnly || mine.Path != "/" || mine.SameSite != http.SameSiteLaxMode {
		t.Fatalf("验证码 cookie %+v", mine)
	}
	_, other := e.takeCode(t, "", "/")

	if r := e.poll(t, "", "/search?q=x", mine); r.code != 200 || r.state != "pending" || r.session != nil {
		t.Fatalf("未确认：%+v", r)
	}
	if r := e.poll(t, "", "/", nil); r.code != http.StatusGone || r.state != "expired" {
		t.Fatalf("没有 cookie：%+v", r)
	}
	// 数据库里只有哈希
	if ok, _ := e.st.LoginCodeActive(t.Context(), code, e.clk.Now()); ok {
		t.Fatal("库里不应存验证码原文")
	}

	// 微信里发了验证码（收件确认）
	if ok, _ := e.st.ConfirmLoginCode(t.Context(), auth.CodeHash(code), e.clk.Now()); !ok {
		t.Fatal("确认失败")
	}
	// 另一个浏览器拿不到这个会话
	if r := e.poll(t, "", "/", other); r.state != "pending" || r.session != nil {
		t.Fatalf("另一个浏览器：%+v", r)
	}
	r := e.poll(t, "", "/search?q=x", mine)
	if r.state != "ok" || r.next != "/search?q=x" || r.session == nil {
		t.Fatalf("换会话：%+v", r)
	}
	if e.code(t, "/", r.session) != 200 {
		t.Fatal("拿到的会话应能用")
	}
	// 换完立即作废
	if r := e.poll(t, "", "/", mine); r.state != "expired" || r.session != nil {
		t.Fatalf("重复换：%+v", r)
	}
	// next 不能跳外站
	code2, c2 := e.takeCode(t, "", "/")
	e.st.ConfirmLoginCode(t.Context(), auth.CodeHash(code2), e.clk.Now())
	if r := e.poll(t, "", "//evil.example", c2); r.next != "/" {
		t.Fatalf("外站 next：%+v", r)
	}
}

func TestCodeSigninWithoutScript(t *testing.T) {
	e := newEnv(t)
	e.bindWeChat(t)
	e.requireLogin(t)
	code, c := e.takeCode(t, "", "/usage")

	// 还没确认：原样显示同一个验证码
	resp, body := e.do(t, "POST", "/signin/code", url.Values{"next": {"/usage"}, "code": {code}}, c)
	if resp.StatusCode != 200 || sessionOf(resp) != nil {
		t.Fatalf("未确认：%d", resp.StatusCode)
	}
	mustContain(t, body, "还没收到这个验证码", ">"+code+"<")
	// 伪造的验证码不回显，换成新的
	_, body = e.do(t, "POST", "/signin/code", url.Values{"code": {"<b>1</b>"}}, c)
	mustNotContain(t, body, "<b>1</b>")
	mustContain(t, body, "已换成新的")
	code, c = e.takeCode(t, "", "/usage")

	e.st.ConfirmLoginCode(t.Context(), auth.CodeHash(code), e.clk.Now())
	resp, _ = e.do(t, "POST", "/signin/code", url.Values{"next": {"/usage"}, "code": {code}}, c)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/usage" || sessionOf(resp) == nil {
		t.Fatalf("确认后：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// 跨站提交被拒
	resp, _ = e.postFrom(t, "/signin/code", url.Values{}, "cross-site")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("跨站：%d", resp.StatusCode)
	}
}

func TestCodeExpiryAndPollLimit(t *testing.T) {
	e := newEnv(t)
	e.bindWeChat(t)
	e.requireLogin(t)
	code, c := e.takeCode(t, "", "/")
	e.clk.Advance(codeTTL)
	if ok, _ := e.st.ConfirmLoginCode(t.Context(), auth.CodeHash(code), e.clk.Now()); ok {
		t.Fatal("过期后不应确认")
	}
	if r := e.poll(t, "", "/", c); r.state != "expired" {
		t.Fatalf("过期：%+v", r)
	}

	_, c = e.takeCode(t, "", "/")
	for i := 0; i < store.MaxLoginCodePolls; i++ {
		if r := e.poll(t, "", "/", c); r.state != "pending" {
			t.Fatalf("第 %d 次：%+v", i+1, r)
		}
	}
	if r := e.poll(t, "", "/", c); r.state != "expired" {
		t.Fatalf("超过查询上限：%+v", r)
	}
}

func TestCodeThrottle(t *testing.T) {
	e := newEnv(t)
	e.bindWeChat(t)
	e.requireLogin(t)
	for i := 0; i < codeRate; i++ {
		e.takeCode(t, "", "/")
	}
	if n, _ := e.st.CountLoginCodes(t.Context()); n > store.MaxActiveLoginCodes {
		t.Fatalf("表里 %d 个验证码", n)
	}
	resp, body := e.do(t, "GET", "/signin", nil, nil)
	if codeRe.MatchString(body) || cookieNamed(resp, codeCookie) != nil {
		t.Fatal("超过节流还在生成")
	}
	mustContain(t, body, "太频繁")
	e.clk.Advance(time.Minute)
	e.takeCode(t, "", "/")
}

func TestCodeSigninBasePath(t *testing.T) {
	e := newEnvBase(t, "/todo")
	e.bindWeChat(t)
	e.requireLogin(t)
	resp, body := e.do(t, "GET", "/todo/signin?next=%2Fsearch", nil, nil)
	checkPrefixed(t, "/signin", body)
	mustContain(t, body, `data-next="/search"`)
	c := cookieNamed(resp, codeCookie)
	if c == nil || c.Path != "/todo" {
		t.Fatalf("cookie %+v", c)
	}
	code := codeRe.FindStringSubmatch(body)[1]
	e.st.ConfirmLoginCode(t.Context(), auth.CodeHash(code), e.clk.Now())
	r := e.poll(t, "/todo", "/search", c)
	if r.state != "ok" || r.next != "/todo/search" || r.session == nil || r.session.Path != "/todo" {
		t.Fatalf("%+v", r)
	}
}
