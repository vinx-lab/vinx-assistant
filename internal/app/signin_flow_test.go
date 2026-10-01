package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
	"github.com/vinx-lab/vinx-assistant/internal/ingest"
	"github.com/vinx-lab/vinx-assistant/internal/session"
)

// 完整链路（spec 0004）：服务真实运行（收件轮询 + 网页），iLink 后端是 httptest 假服务。
// 网页取码 → 主人在微信里发验证码 → Bot 回复「✓ 已登录网页」→ 网页查询拿到会话；另一个浏览器拿不到。
func TestWeChatCodeSigninFlow(t *testing.T) {
	srv := ilinktest.New()
	t.Cleanup(srv.Close)
	cfg := Config{Listen: freeAddr(t), DataDir: t.TempDir(), BasePath: "/todo"}
	a, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	if err := a.Session.SaveCred(ctx, srv.Cred()); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.SetLoginRequired(ctx, true); err != nil {
		t.Fatal(err)
	}
	if a.Session.Status(ctx) != session.StatusOK {
		t.Fatal("微信应可用")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Run(runCtx) }()
	defer func() {
		cancel()
		<-done
	}()
	base := "http://" + cfg.Listen + "/todo"

	browser := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	get := func(c *http.Client, path string) (int, string) {
		t.Helper()
		var resp *http.Response
		var err error
		for i := 0; i < 50; i++ { // 等服务起来
			if resp, err = c.Get(base + path); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	mine, other := browser(), browser()

	// 未登录访问 → 登录页
	if code, _ := get(mine, "/search"); code != http.StatusSeeOther {
		t.Fatalf("未登录 /search = %d", code)
	}
	re := regexp.MustCompile(`<p class="code">登录 (\d{6})</p>`)
	_, page := get(mine, "/signin?next="+url.QueryEscape("/search"))
	m := re.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("登录页没有验证码：%s", page)
	}
	code := m[1]
	get(other, "/signin") // 另一个浏览器也开着登录页（拿到的是自己的验证码）

	poll := func(c *http.Client) map[string]string {
		t.Helper()
		_, body := get(c, "/signin/code/status?next="+url.QueryEscape("/search"))
		var j map[string]string
		json.Unmarshal([]byte(body), &j)
		return j
	}
	if j := poll(mine); j["state"] != "pending" {
		t.Fatalf("发送前：%v", j)
	}

	// 主人在微信里发验证码：经假后端的 getupdates 进入收件
	srv.Push(ilinktest.TextMsg(7001, ilinktest.OwnerID, "登录 "+code))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s := srv.Sent(); len(s) > 0 {
			if !strings.HasPrefix(s[len(s)-1].Text, ingest.SigninReplyPrefix) || s[len(s)-1].ContextToken != "ctx-7001" {
				t.Fatalf("回复 %+v", s[len(s)-1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Bot 没有回复")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if seen, _ := a.Store.Seen(ctx, "7001"); !seen {
		t.Fatal("验证码消息应记入 seen_msgs")
	}
	if _, err := a.Store.GetItemByMsgID(ctx, "7001"); err == nil {
		t.Fatal("验证码消息不应入库")
	}

	// 另一个浏览器拿不到会话
	if j := poll(other); j["state"] != "pending" {
		t.Fatalf("另一个浏览器：%v", j)
	}
	if code, _ := get(other, "/search"); code != http.StatusSeeOther {
		t.Fatalf("另一个浏览器 /search = %d", code)
	}
	// 发起登录的浏览器拿到会话，回到原地址
	if j := poll(mine); j["state"] != "ok" || j["next"] != "/todo/search" {
		t.Fatalf("换会话：%v", j)
	}
	if code, _ := get(mine, "/search"); code != http.StatusOK {
		t.Fatalf("登录后 /search = %d", code)
	}
	if j := poll(mine); j["state"] != "ok" { // 已登录时查询直接 ok
		t.Fatalf("已登录：%v", j)
	}

	// 主人在微信里发「退出网页登录」：所有网页登录失效
	srv.Push(ilinktest.TextMsg(7002, ilinktest.OwnerID, "退出网页登录"))
	deadline = time.Now().Add(5 * time.Second)
	for {
		if s := srv.Sent(); s[len(s)-1].Text == ingest.SignoutReply {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("没有回复退出")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, _ := get(mine, "/search"); code != http.StatusSeeOther {
		t.Fatalf("退出后 /search = %d", code)
	}
}
