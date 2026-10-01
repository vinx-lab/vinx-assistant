package web

import (
	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/auth"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

const testPW = "open sesame 123"

// setPassword 模拟命令行设置密码并打开「需要登录」（相当于 0003 升级上来的部署）：直接写库，吊销全部会话。返回记录里的哈希（base64），用来检查它没出现在页面和日志里。
func (e *env) setPassword(t *testing.T, pw string) string {
	t.Helper()
	rec, err := auth.Hash(pw, 1000)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := rec.Encode()
	if err := e.st.SetPassword(t.Context(), raw, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetLoginRequired(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(rec.Hash)
}

// do 发一个请求；form 非 nil 时是同源表单 POST。cookie 可为 nil。
func (e *env) do(t *testing.T, method, path string, form url.Values, c *http.Cookie) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := e.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func sessionOf(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

// signin 用 pw 登录（base 是前缀），返回会话 cookie。
func (e *env) signin(t *testing.T, base, pw string) *http.Cookie {
	t.Helper()
	resp, body := e.do(t, "POST", base+"/signin", url.Values{"password": {pw}}, nil)
	c := sessionOf(resp)
	if resp.StatusCode != http.StatusSeeOther || c == nil || c.Value == "" {
		t.Fatalf("登录失败：%d %s", resp.StatusCode, body)
	}
	return c
}

func (e *env) code(t *testing.T, path string, c *http.Cookie) int {
	t.Helper()
	resp, _ := e.do(t, "GET", path, nil, c)
	return resp.StatusCode
}

func TestNoPasswordHint(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/settings/rules", "/search"} {
		code, body := e.get(t, p)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", p, code)
		}
		mustContain(t, body, "还没开启登录", `href="/settings/password"`)
		mustNotContain(t, body, `action="/signout"`)
	}
	_, body := e.get(t, "/settings/password")
	mustContain(t, body, "<h2>设置密码</h2>", `name="confirm"`)
	mustNotContain(t, body, `name="current"`, "退出所有设备")
	_, body = e.get(t, "/settings")
	mustContain(t, body, `href="/settings/password"`, "未开启")
	// 没设密码时登录页直接回去
	if resp, _ := e.do(t, "GET", "/signin?next=/search", nil, nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/search" {
		t.Fatalf("signin: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestGuardWhenPasswordSet(t *testing.T) {
	e := newEnv(t)
	e.setPassword(t, testPW)
	cases := []struct {
		method, path string
		code         int
		loc          string
	}{
		{"GET", "/", 303, "/signin"},
		{"GET", "/?cat=todo&done=1", 303, "/signin?next=" + url.QueryEscape("/?cat=todo&done=1")},
		{"GET", "/settings", 303, "/signin?next=%2Fsettings"},
		{"GET", "/items/1", 303, "/signin?next=%2Fitems%2F1"},
		{"GET", "/media/a.jpg", 303, "/signin?next=%2Fmedia%2Fa.jpg"},
		{"GET", "/login", 303, "/signin?next=%2Flogin"},
		{"POST", "/batch/run", 303, "/signin"},
		{"POST", "/settings/general", 303, "/signin"},
		{"POST", "/settings/password", 303, "/signin"},
		{"POST", "/settings/password/signout-all", 303, "/signin"},
		{"POST", "/settings/providers/p1/models", 401, ""},
		{"GET", "/login/status", 401, ""},
		{"GET", "/login/qr.png", 401, ""},
		{"GET", "/static/app.css", 200, ""},
		{"GET", "/static/app.js", 200, ""},
		{"GET", "/signin", 200, ""},
	}
	for _, c := range cases {
		var form url.Values
		if c.method == "POST" {
			form = url.Values{"x": {"1"}}
		}
		resp, body := e.do(t, c.method, c.path, form, nil)
		if resp.StatusCode != c.code || resp.Header.Get("Location") != c.loc {
			t.Errorf("%s %s: %d %q", c.method, c.path, resp.StatusCode, resp.Header.Get("Location"))
		}
		if c.code == 401 {
			mustContain(t, body, `"error"`)
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("%s: content-type %q", c.path, ct)
			}
		}
	}
	// 被拒的 POST 没有生效
	if e.batches.Load() != 0 {
		t.Fatal("未登录的立即整理不应执行")
	}
	// 登录页不泄露顶栏状态
	_, body := e.get(t, "/signin?next=/settings")
	mustContain(t, body, `name="next" value="/settings"`, `action="/signin"`, `type="password"`)
	mustNotContain(t, body, `class="dot`, "立即整理", "还没开启登录")
}

func TestSigninFlow(t *testing.T) {
	e := newEnv(t)
	hash := e.setPassword(t, testPW)

	resp, body := e.do(t, "POST", "/signin", url.Values{"password": {"wrong password"}, "next": {"/search?q=x"}}, nil)
	if resp.StatusCode != http.StatusUnauthorized || sessionOf(resp) != nil {
		t.Fatalf("错误密码：%d", resp.StatusCode)
	}
	mustContain(t, body, "密码不对", `value="/search?q=x"`)

	resp, _ = e.do(t, "POST", "/signin", url.Values{"password": {testPW}, "next": {"/search?q=x"}}, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/search?q=x" {
		t.Fatalf("登录：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	c := sessionOf(resp)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure || c.MaxAge != int(sessionTTL/time.Second) {
		t.Fatalf("cookie %+v", c)
	}
	// 数据库里只有令牌的哈希
	if ok, _ := e.st.SessionValid(t.Context(), c.Value, e.clk.Now()); ok {
		t.Fatal("数据库里不应存令牌原文")
	}
	resp, body = e.do(t, "GET", "/", nil, c)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录后 / = %d", resp.StatusCode)
	}
	mustContain(t, body, `action="/signout"`)
	mustNotContain(t, body, "还没开启登录", hash, testPW)
	_, body = e.do(t, "GET", "/settings/password", nil, c)
	mustContain(t, body, "<h2>修改密码</h2>", `name="current"`, "退出所有设备")
	mustNotContain(t, body, hash)
	_, body = e.do(t, "GET", "/settings", nil, c)
	mustContain(t, body, `class="m-signout"`, "需要登录")
	if code := e.code(t, "/login/status", c); code != http.StatusOK {
		t.Fatalf("/login/status = %d", code)
	}
	// 已登录时打开登录页直接回去
	if resp, _ := e.do(t, "GET", "/signin?next=/usage", nil, c); resp.Header.Get("Location") != "/usage" {
		t.Fatalf("已登录 signin -> %s", resp.Header.Get("Location"))
	}
	// 30 天后过期
	e.clk.Advance(sessionTTL)
	if code := e.code(t, "/", c); code != http.StatusSeeOther {
		t.Fatalf("过期后 / = %d", code)
	}
	if strings.Contains(e.logs.String(), hash) || strings.Contains(e.logs.String(), testPW) {
		t.Fatal("日志里出现了密码或哈希")
	}
}

func TestSigninNextStaysOnSite(t *testing.T) {
	e := newEnv(t)
	e.setPassword(t, testPW)
	for _, next := range []string{"//evil.example", "https://evil.example/x", "/\\evil.example", "/\t/evil.example", "javascript:alert(1)", "evil.example"} {
		resp, _ := e.do(t, "POST", "/signin", url.Values{"password": {testPW}, "next": {next}}, nil)
		if loc := resp.Header.Get("Location"); loc != "/" {
			t.Errorf("next=%q -> %q", next, loc)
		}
		_, body := e.get(t, "/signin?next="+url.QueryEscape(next))
		mustContain(t, body, `name="next" value="/"`)
	}
}

func TestCrossSiteSigninRejected(t *testing.T) {
	e := newEnv(t)
	e.setPassword(t, testPW)
	resp, _ := e.postFrom(t, "/signin", url.Values{"password": {testPW}}, "cross-site")
	if resp.StatusCode != http.StatusForbidden || sessionOf(resp) != nil {
		t.Fatalf("跨站登录：%d", resp.StatusCode)
	}
	resp, _ = e.postFrom(t, "/signout", nil, "cross-site")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("跨站退出：%d", resp.StatusCode)
	}
	if n, _ := e.st.CountSessions(t.Context()); n != 0 {
		t.Fatalf("sessions = %d", n)
	}
}

func TestFirstSetPassword(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, pw, confirm string
		code              int
		want              string
	}{
		{"空", "", "", 400, "不能为空"},
		{"不一致", testPW, testPW + "x", 400, "不一致"},
	}
	for _, c := range cases {
		resp, body := e.do(t, "POST", "/settings/password", url.Values{"new": {c.pw}, "confirm": {c.confirm}}, nil)
		if resp.StatusCode != c.code {
			t.Errorf("%s: %d", c.name, resp.StatusCode)
		}
		mustContain(t, body, c.want, "<h2>设置密码</h2>")
		if v, _ := e.st.PasswordRecord(t.Context()); v != "" {
			t.Fatalf("%s: 不应保存", c.name)
		}
	}
	resp, _ := e.do(t, "POST", "/settings/password", url.Values{"new": {testPW}, "confirm": {testPW}}, nil)
	c := sessionOf(resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/password?msg=pwset" || c == nil {
		t.Fatalf("设置：%d %s %v", resp.StatusCode, resp.Header.Get("Location"), c)
	}
	raw, _ := e.st.PasswordRecord(t.Context())
	if rec, err := auth.Decode(raw); err != nil || !rec.Verify(testPW) || strings.Contains(raw, testPW) {
		t.Fatalf("记录 %v", err)
	}
	if resp, body := e.do(t, "GET", "/settings/password?msg=pwset", nil, c); resp.StatusCode != 200 {
		t.Fatalf("设置后当前浏览器应已登录：%d", resp.StatusCode)
	} else {
		mustContain(t, body, "密码已设置")
	}
	// 0004 起设密码不再自动要求登录
	if code := e.code(t, "/", nil); code != http.StatusOK {
		t.Fatalf("没开启登录时别的浏览器照常访问：%d", code)
	}
}

func TestChangePasswordRevokesOthers(t *testing.T) {
	e := newEnv(t)
	e.setPassword(t, testPW)
	a, b := e.signin(t, "", testPW), e.signin(t, "", testPW)

	const newPW = "brand new secret"
	cases := []struct {
		name string
		form url.Values
		code int
		want string
	}{
		{"当前密码错", url.Values{"current": {"nope nope"}, "new": {newPW}, "confirm": {newPW}}, 401, "当前密码不对"},
		{"新密码为空", url.Values{"current": {testPW}, "new": {""}, "confirm": {""}}, 400, "不能为空"},
		{"两次不一致", url.Values{"current": {testPW}, "new": {newPW}, "confirm": {newPW + "!"}}, 400, "不一致"},
	}
	for _, c := range cases {
		resp, body := e.do(t, "POST", "/settings/password", c.form, a)
		if resp.StatusCode != c.code {
			t.Errorf("%s: %d", c.name, resp.StatusCode)
		}
		mustContain(t, body, c.want)
	}
	if e.code(t, "/", b) != 200 {
		t.Fatal("校验失败时不应吊销会话")
	}

	resp, _ := e.do(t, "POST", "/settings/password", url.Values{"current": {testPW}, "new": {newPW}, "confirm": {newPW}}, a)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/password?msg=pwchanged" {
		t.Fatalf("修改：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if e.code(t, "/", a) != 200 {
		t.Fatal("当前设备应保持登录")
	}
	if e.code(t, "/", b) != http.StatusSeeOther {
		t.Fatal("其他设备应失效")
	}
	if resp, _ := e.do(t, "POST", "/signin", url.Values{"password": {testPW}}, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("旧密码：%d", resp.StatusCode)
	}
	e.signin(t, "", newPW)
}

func TestSignout(t *testing.T) {
	e := newEnv(t)
	e.setPassword(t, testPW)
	a, b, c := e.signin(t, "", testPW), e.signin(t, "", testPW), e.signin(t, "", testPW)

	resp, _ := e.do(t, "POST", "/signout", url.Values{}, a)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/signin" {
		t.Fatalf("退出：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if k := sessionOf(resp); k == nil || k.MaxAge >= 0 || k.Value != "" {
		t.Fatalf("应清除 cookie：%+v", k)
	}
	if e.code(t, "/", a) != http.StatusSeeOther || e.code(t, "/", b) != 200 {
		t.Fatal("退出只影响当前设备")
	}

	resp, _ = e.do(t, "POST", "/settings/password/signout-all", url.Values{}, b)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/signin" || sessionOf(resp) == nil {
		t.Fatalf("退出所有设备：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if e.code(t, "/", b) != http.StatusSeeOther || e.code(t, "/", c) != http.StatusSeeOther {
		t.Fatal("所有设备都应失效")
	}
}

func TestLockout(t *testing.T) {
	e := newEnv(t)
	e.setPassword(t, testPW)
	for i := 1; i <= auth.MaxFails; i++ {
		resp, body := e.do(t, "POST", "/signin", url.Values{"password": {"guess-" + string(rune('a'+i))}}, nil)
		want := http.StatusUnauthorized
		if i == auth.MaxFails {
			want = http.StatusTooManyRequests
			mustContain(t, body, "15 分钟后再试")
		}
		if resp.StatusCode != want {
			t.Fatalf("第 %d 次：%d", i, resp.StatusCode)
		}
	}
	logs := e.logs.String()
	if strings.Count(logs, "level=WARN") != 1 || strings.Contains(logs, "guess-") {
		t.Fatalf("日志：%s", logs)
	}
	// 锁定期间正确密码也拒绝，登录页显示剩余时间
	if resp, _ := e.do(t, "POST", "/signin", url.Values{"password": {testPW}}, nil); resp.StatusCode != http.StatusTooManyRequests || sessionOf(resp) != nil {
		t.Fatalf("锁定中：%d", resp.StatusCode)
	}
	e.clk.Advance(10 * time.Minute)
	_, body := e.get(t, "/signin")
	mustContain(t, body, "5 分钟后再试")
	e.clk.Advance(5 * time.Minute)
	e.signin(t, "", testPW)

	// 再次锁定后，命令行重置密码能立即解锁
	for i := 0; i < auth.MaxFails; i++ {
		e.do(t, "POST", "/signin", url.Values{"password": {"x"}}, nil)
	}
	if resp, _ := e.do(t, "POST", "/signin", url.Values{"password": {testPW}}, nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("应再次锁定：%d", resp.StatusCode)
	}
	e.setPassword(t, "reset by cli")
	e.signin(t, "", "reset by cli")
}

func TestCLIChangesTakeEffect(t *testing.T) {
	e := newEnv(t)
	e.setPassword(t, testPW)
	c := e.signin(t, "", testPW)
	// 命令行重置密码：所有会话失效
	e.setPassword(t, "another password")
	if e.code(t, "/", c) != http.StatusSeeOther {
		t.Fatal("命令行重置后旧会话应失效")
	}
	// 命令行清除（微信不可用时同时关闭「需要登录」）：恢复成不需要登录
	if err := e.st.ClearPassword(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.st.SetLoginRequired(t.Context(), false)
	code, body := e.get(t, "/")
	if code != 200 {
		t.Fatalf("清除后 / = %d", code)
	}
	mustContain(t, body, "还没开启登录")
}

func TestBasePathSignin(t *testing.T) {
	e := newEnvBase(t, "/todo")
	e.setPassword(t, testPW)
	resp, _ := e.do(t, "GET", "/todo/settings/rules", nil, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/todo/signin?next=%2Fsettings%2Frules" {
		t.Fatalf("跳转：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	code, body := e.get(t, "/todo/signin?next=%2Fsettings%2Frules")
	if code != 200 {
		t.Fatalf("signin %d", code)
	}
	checkPrefixed(t, "/signin", body)
	mustContain(t, body, `value="/settings/rules"`)
	if e.code(t, "/todo/static/app.css", nil) != 200 {
		t.Fatal("静态文件不需要登录")
	}
	if e.code(t, "/signin", nil) != http.StatusNotFound {
		t.Fatal("前缀之外不应有登录页")
	}

	resp, _ = e.do(t, "POST", "/todo/signin", url.Values{"password": {testPW}, "next": {"/settings/rules"}}, nil)
	c := sessionOf(resp)
	if resp.Header.Get("Location") != "/todo/settings/rules" || c == nil || c.Path != "/todo" {
		t.Fatalf("登录：%s %+v", resp.Header.Get("Location"), c)
	}
	code2, body := e.do(t, "GET", "/todo/", nil, c)
	if code2.StatusCode != 200 {
		t.Fatalf("登录后 %d", code2.StatusCode)
	}
	mustContain(t, body, `action="/todo/signout"`)
	resp, _ = e.do(t, "POST", "/todo/signout", url.Values{}, c)
	if resp.Header.Get("Location") != "/todo/signin" || sessionOf(resp).Path != "/todo" {
		t.Fatalf("退出：%s", resp.Header.Get("Location"))
	}
	if e.code(t, "/todo/", c) != http.StatusSeeOther {
		t.Fatal("退出后应失效")
	}
}

func TestSessionCookieAttrs(t *testing.T) {
	exp := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		base   string
		remote string
		tls    bool
		xfp    string
		path   string
		secure bool
	}{
		{"根路径 http", "", "192.0.2.1:5000", false, "", "/", false},
		{"子路径", "/todo", "192.0.2.1:5000", false, "", "/todo", false},
		{"直接 TLS", "", "192.0.2.1:5000", true, "", "/", true},
		{"本机反代 https", "/todo", "127.0.0.1:5000", false, "https", "/todo", true},
		{"本机反代 IPv6", "", "[::1]:5000", false, "HTTPS, http", "/", true},
		{"本机反代 http", "", "127.0.0.1:5000", false, "http", "/", false},
		{"非本机伪造头", "", "192.0.2.1:5000", false, "https", "/", false},
	}
	for _, c := range cases {
		s := New(Deps{BasePath: c.base})
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if c.xfp != "" {
			r.Header.Set("X-Forwarded-Proto", c.xfp)
		}
		k := s.sessionCookieFor(r, "tok", exp)
		if k.Name != sessionCookie || k.Path != c.path || k.Secure != c.secure || !k.HttpOnly || k.SameSite != http.SameSiteLaxMode || !k.Expires.Equal(exp) {
			t.Errorf("%s: %+v", c.name, k)
		}
		if del := s.sessionCookieFor(r, "", time.Time{}); del.MaxAge >= 0 || del.Path != c.path {
			t.Errorf("%s 删除: %+v", c.name, del)
		}
	}
}

// 并发提交错误密码绕不过锁定：真正校验的次数不超过 MaxFails，锁定后正确密码也被拒；校验串行进行。
func TestConcurrentSigninCannotBypassLockout(t *testing.T) {
	e := newEnv(t)
	e.setPassword(t, testPW)
	var calls, running, maxRunning atomic.Int32
	e.web.verify = func(rec *auth.Record, pw string) bool {
		calls.Add(1)
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond) // 模拟 PBKDF2 的耗时，让请求真正重叠
		running.Add(-1)
		return rec.Verify(pw)
	}

	const n = 40
	codes := make(chan int, n+1)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := e.do(t, "POST", "/signin", url.Values{"password": {"wrong guess"}}, nil)
			codes <- resp.StatusCode
		}()
	}
	// 错误密码还在排队校验时，送一个正确密码
	eventually(t, func() bool { return e.web.limiter.Locked() > 0 })
	resp, _ := e.do(t, "POST", "/signin", url.Values{"password": {testPW}}, nil)
	if resp.StatusCode != http.StatusTooManyRequests || sessionOf(resp) != nil {
		t.Fatalf("锁定后正确密码：%d", resp.StatusCode)
	}
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for c := range codes {
		count[c]++
	}
	if c := calls.Load(); c > auth.MaxFails {
		t.Fatalf("校验了 %d 次，超过 %d", c, auth.MaxFails)
	}
	if count[http.StatusUnauthorized]+count[http.StatusTooManyRequests] != n || count[http.StatusTooManyRequests] < n-auth.MaxFails {
		t.Fatalf("状态码分布 %v", count)
	}
	if m := maxRunning.Load(); m != 1 {
		t.Fatalf("同时在跑的校验 %d 个", m)
	}
	if strings.Count(e.logs.String(), "level=WARN") != 1 {
		t.Fatalf("WARN 应只有一条：%s", e.logs.String())
	}
}

// 首次设置期间密码已被别人设好：不覆盖，提示去登录。
func TestFirstSetPasswordDoesNotOverwrite(t *testing.T) {
	e := newEnv(t)
	e.web.kdfSem <- struct{}{} // 占住 PBKDF2，让设置请求停在哈希之前
	type result struct {
		resp *http.Response
		body string
	}
	done := make(chan result, 1)
	go func() {
		resp, body := e.do(t, "POST", "/settings/password", url.Values{"new": {"from browser A"}, "confirm": {"from browser A"}}, nil)
		done <- result{resp, body}
	}()
	time.Sleep(50 * time.Millisecond) // 请求已过 guard（当时没设密码），在等 PBKDF2
	e.setPassword(t, testPW)          // 另一方（命令行或另一个浏览器）先设好了
	<-e.web.kdfSem
	r := <-done
	if r.resp.StatusCode != http.StatusSeeOther || r.resp.Header.Get("Location") != "/settings/password?msg=pwexists" || sessionOf(r.resp) != nil {
		t.Fatalf("应拒绝覆盖：%d %s", r.resp.StatusCode, r.resp.Header.Get("Location"))
	}
	raw, _ := e.st.PasswordRecord(t.Context())
	if rec, _ := auth.Decode(raw); !rec.Verify(testPW) {
		t.Fatal("先设的密码被覆盖了")
	}
}

// 设了密码时，需要登录的响应（页面、附件）不进浏览器缓存；没设密码时保持原样。
func TestNoStoreWhenPasswordSet(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(filepath.Join(e.media, "2026/10"), 0o700)
	os.WriteFile(filepath.Join(e.media, "2026/10/1-1.jpg"), []byte("\xff\xd8\xff"), 0o600)
	id := e.item(t, &model.Item{MsgID: "1", RawText: "x"})
	if _, err := e.st.InsertAttachment(t.Context(), &model.Attachment{ItemID: id, Kind: "image", RelPath: "2026/10/1-1.jpg", State: "ok"}); err != nil {
		t.Fatal(err)
	}
	check := func(c *http.Cookie, path, want string) {
		t.Helper()
		resp, _ := e.do(t, "GET", path, nil, c)
		if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != want {
			t.Errorf("%s: %d Cache-Control=%q want %q", path, resp.StatusCode, resp.Header.Get("Cache-Control"), want)
		}
	}
	check(nil, "/", "")
	check(nil, "/media/2026/10/1-1.jpg", "private, max-age=86400")

	e.setPassword(t, testPW)
	c := e.signin(t, "", testPW)
	for _, p := range []string{"/", "/items/1", "/settings", "/media/2026/10/1-1.jpg"} {
		check(c, p, "no-store")
	}
	check(c, "/static/app.css", "")
}
