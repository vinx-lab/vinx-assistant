package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/auth"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

var codeRe = regexp.MustCompile(`<p class="code">登录 (\d{6})</p>`)

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
		{"只有微信", "ok", false, 200, true, false, "把下面这段文字发给 Bot"},
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
	if _, ok, _ := e.st.ConfirmLoginCode(t.Context(), auth.CodeHash(code), e.clk.Now()); !ok {
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

	// 还没确认：原样显示同一个验证码，并给出收不到时的办法
	resp, body := e.do(t, "POST", "/signin/code", url.Values{"next": {"/usage"}}, c)
	if resp.StatusCode != 200 || sessionOf(resp) != nil || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("未确认：%d", resp.StatusCode)
	}
	mustContain(t, body, "还没收到这个验证码", "登录 "+code+"<", "--no-login")
	// 伪造的 cookie（验证码不属于这个浏览器）：不回显，换成新的
	forged := &http.Cookie{Name: codeCookie, Value: strings.Split(c.Value, ".")[0] + ".000000"}
	_, body = e.do(t, "GET", "/signin", nil, forged)
	if m := codeRe.FindStringSubmatch(body); m == nil || (m[1] == "000000" && code != "000000") {
		t.Fatalf("伪造的验证码被回显了")
	}
	code, c = e.takeCode(t, "", "/usage")

	e.st.ConfirmLoginCode(t.Context(), auth.CodeHash(code), e.clk.Now())
	resp, _ = e.do(t, "POST", "/signin/code", url.Values{"next": {"/usage"}}, c)
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
	if _, ok, _ := e.st.ConfirmLoginCode(t.Context(), auth.CodeHash(code), e.clk.Now()); ok {
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

// doFrom 发一个经本机反向代理转发的 GET，X-Forwarded-For 是 ip。
func (e *env) doFrom(t *testing.T, ip, path string, c *http.Cookie) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.250, "+ip) // 左边的可以伪造，取最右边
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

func TestCodeThrottle(t *testing.T) {
	e := newEnv(t)
	e.bindWeChat(t)
	e.requireLogin(t)
	gen := func(ip string) bool {
		t.Helper()
		resp, body := e.doFrom(t, ip, "/signin", nil)
		e.st.DeleteAllLoginCodes(t.Context()) // 只测节流，不让数量上限干扰
		if codeRe.MatchString(body) != (cookieNamed(resp, codeCookie) != nil) {
			t.Fatal("验证码和 cookie 应同时出现")
		}
		return codeRe.MatchString(body)
	}
	// 每个来源每分钟 5 个
	for i := 0; i < codeRatePerIP; i++ {
		if !gen("10.0.0.9") {
			t.Fatalf("第 %d 个就被节流", i+1)
		}
	}
	if gen("10.0.0.9") {
		t.Fatal("同一来源第 6 个应被节流")
	}
	_, body := e.doFrom(t, "10.0.0.9", "/signin", nil)
	mustContain(t, body, "太频繁")
	if !gen("10.0.0.1") {
		t.Fatal("别的来源不受影响")
	}
	// 全站每分钟 30 个
	n := codeRatePerIP + 1
	for i := 0; n < codeRateTotal; i++ {
		if !gen("10.1.0." + strconv.Itoa(i)) {
			t.Fatalf("全站第 %d 个被节流", n+1)
		}
		n++
	}
	if gen("10.2.0.1") {
		t.Fatal("全站第 31 个应被节流")
	}
	e.clk.Advance(time.Minute)
	if !gen("10.0.0.9") {
		t.Fatal("一分钟后应恢复")
	}
}

// 审查 F2 的场景：主人拿到验证码后，别人反复打开登录页，主人的码仍然有效；刷新时复用原码。
func TestCodeNotStolenByOthers(t *testing.T) {
	e := newEnv(t)
	e.bindWeChat(t)
	e.requireLogin(t)
	ownerResp, ownerBody := e.doFrom(t, "10.0.0.1", "/signin", nil)
	owner := cookieNamed(ownerResp, codeCookie)
	code := codeRe.FindStringSubmatch(ownerBody)[1]

	// 攻击者从多个来源刷登录页：最多再占 4 个名额，之后被拒绝（提示稍后再试），不挤掉主人的码
	for i := 0; i < 20; i++ {
		_, body := e.doFrom(t, "10.9.0."+strconv.Itoa(i%4), "/signin", nil)
		if i >= store.MaxActiveLoginCodes-1 {
			if codeRe.MatchString(body) {
				t.Fatalf("第 %d 次：超过上限还在生成", i+1)
			}
			if !strings.Contains(body, "稍后") && !strings.Contains(body, "太频繁") {
				t.Fatalf("第 %d 次：没有提示", i+1)
			}
		}
	}
	// 主人刷新：复用原码，不消耗额度
	for i := 0; i < 10; i++ {
		resp, body := e.doFrom(t, "10.0.0.1", "/signin", owner)
		if m := codeRe.FindStringSubmatch(body); m == nil || m[1] != code {
			t.Fatalf("刷新后验证码变了")
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("登录页应 no-store")
		}
	}
	// 主人的码仍能确认、换到会话
	if _, ok, _ := e.st.ConfirmLoginCode(t.Context(), auth.CodeHash(code), e.clk.Now()); !ok {
		t.Fatal("主人的码被挤掉了")
	}
	// 已确认还没换会话时，别人生成新码也挤不掉它
	e.clk.Advance(codeTTL)
	e.doFrom(t, "10.8.0.1", "/signin", nil)
	if r := e.poll(t, "", "/", owner); r.state != "ok" || r.session == nil {
		t.Fatalf("换会话：%+v", r)
	}
}

func TestClientInfo(t *testing.T) {
	for _, c := range []struct{ remote, xff, want string }{
		{"192.0.2.1:5000", "", "192.0.2.1"},
		{"192.0.2.1:5000", "10.0.0.1", "192.0.2.1"}, // 非本机代理的头不信
		{"127.0.0.1:5000", "", "127.0.0.1"},
		{"127.0.0.1:5000", "1.1.1.1, 10.0.0.2", "10.0.0.2"},
		{"127.0.0.1:5000", "10.0.0.2, 127.0.0.1", "10.0.0.2"},
		{"[::1]:5000", "2001:db8::1", "2001:db8::1"},
		{"127.0.0.1:5000", "garbage", "127.0.0.1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("%s %q -> %q", c.remote, c.xff, got)
		}
	}
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36":                      "Chrome / Windows",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 Edg/130.0":            "Edge / Windows",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile Safari/604.1": "Safari / iPhone",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0; rv:131.0) Gecko/20100101 Firefox/131.0":                                              "Firefox / macOS",
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Chrome/130.0 Mobile Safari/537.36 MicroMessenger/8.0":                          "微信内置浏览器 / Android",
		"": "未知浏览器 / 未知系统",
	} {
		if got := uaSummary(ua); got != want {
			t.Errorf("%q -> %q", ua, got)
		}
	}
}

// 发起方的来源和浏览器记进验证码，确认时交给 Bot 回复。
func TestCodeRecordsClient(t *testing.T) {
	e := newEnv(t)
	e.bindWeChat(t)
	e.requireLogin(t)
	req, _ := http.NewRequest("GET", e.srv.URL+"/signin", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0) Chrome/130.0 Safari/537.36")
	resp, err := e.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	mustContain(t, body, "只把你自己屏幕上的验证码发给 Bot", `data-copy="登录 `)
	code := codeRe.FindStringSubmatch(body)[1]
	client, ok, _ := e.st.ConfirmLoginCode(t.Context(), auth.CodeHash(code), e.clk.Now())
	if !ok || client.IP != "198.51.100.7" || client.UA != "Chrome / Windows" {
		t.Fatalf("%+v %v", client, ok)
	}
}

func TestShortPasswordHint(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.do(t, "POST", "/settings/password", url.Values{"new": {"1234"}, "confirm": {"1234"}}, nil)
	if resp.Header.Get("Location") != "/settings/password?msg=pwset_short" {
		t.Fatalf("短密码：%s", resp.Header.Get("Location"))
	}
	_, body := e.do(t, "GET", "/settings/password?msg=pwset_short", nil, sessionOf(resp))
	mustContain(t, body, "这个密码很短")
	resp, _ = e.do(t, "POST", "/settings/password", url.Values{"current": {"1234"}, "new": {"long enough"}, "confirm": {"long enough"}}, sessionOf(resp))
	if resp.Header.Get("Location") != "/settings/password?msg=pwchanged" {
		t.Fatalf("长密码：%s", resp.Header.Get("Location"))
	}
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
