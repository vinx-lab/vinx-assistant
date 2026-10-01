package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/session"
)

func TestRelTime(t *testing.T) {
	now := time.Date(2026, 10, 1, 21, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if got := relTime(now.Add(4*time.Hour), now); got != "明天 01:00" {
		t.Fatalf("got %s", got)
	}
	if got := relTime(now.Add(2*time.Hour), now); got != "今天 23:00" {
		t.Fatalf("got %s", got)
	}
	if got := relTime(now.Add(50*time.Hour), now); got != "10-03 23:00" {
		t.Fatalf("got %s", got)
	}
}

func TestBoardRendersStatusTabsAndCards(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	past, today := clock.At(2026, 9, 30, 10, 0), clock.At(2026, 10, 1, 18, 0)
	e.item(t, &model.Item{MsgID: "1", RawText: "交房租", Category: model.CatTodo, DueAt: &past, DueHasTime: true})
	id := e.item(t, &model.Item{MsgID: "2", RawText: "交发票", Category: model.CatTodo, DueAt: &today, DueHasTime: true, Summary: "报销"})
	e.st.SetTags(ctx, id, []string{"财务"})
	e.st.SetKV(ctx, "ilink.drift", `{"msg.new_field":1}`)
	code, body := e.get(t, "/")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	mustContain(t, body, "逾期 1", "今天到期 1", "微信未登录", "下次整理 今天 20:00", "收到 1 个协议之外的字段",
		"待办 <b>2</b>", "#2 交发票", "#财务", "截止 10-01 周四 18:00", `class="overdue"`, "立即整理", "完成", "已取消", "深入研究")
	_, body = e.get(t, "/?cat=todo&tag="+url.QueryEscape("财务"))
	mustContain(t, body, "#2 交发票")
	mustNotContain(t, body, "#1 交房租")
}

func TestBoardTokenLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	st, _ := e.st.LoadSettings(ctx)
	st.AI.DailyTokenLimit = 5000
	if err := e.st.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	_, body := e.get(t, "/")
	mustContain(t, body, "今日 token 0 / 5000")
	st.AI.DailyTokenLimit = 0
	if err := e.st.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	_, body = e.get(t, "/")
	mustContain(t, body, "今日 token 0（不限）")
}

func TestBoardWeChatPaused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.sess.SaveCred(ctx, e.ilink.Cred())
	e.sess.Pause(ctx)
	e.sess.Pause(ctx)
	_, body := e.get(t, "/")
	mustContain(t, body, "微信暂停（-14），10:00 自动重试", "凭证反复失效，建议重新扫码")
	e.clk.Advance(session.PauseDuration)
	e.sess.MarkHealthy(ctx)
	_, body = e.get(t, "/")
	mustContain(t, body, "微信正常")
}

func TestBatchRunOnceAtATime(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.post(t, "/batch/run", url.Values{"back": {"/?cat=idea"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?cat=idea&msg=started" || e.batches.Load() != 1 {
		t.Fatalf("first: %d %s %d", resp.StatusCode, resp.Header.Get("Location"), e.batches.Load())
	}
	e.running.Store(true)
	resp, _ = e.post(t, "/batch/run", url.Values{"back": {"//evil.example"}})
	if resp.Header.Get("Location") != "/?msg=busy" || e.batches.Load() != 1 {
		t.Fatalf("second: %s %d", resp.Header.Get("Location"), e.batches.Load())
	}
	_, body := e.get(t, "/")
	mustContain(t, body, "正在整理…", "disabled")
}

func TestStatusAndDeep(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.item(t, &model.Item{MsgID: "1", RawText: "htmx", Category: model.CatResearch, ProcessAttempts: 3, ProcessError: "坏 JSON", ProcessedLevel: model.LevelLight})
	resp, _ := e.post(t, "/items/1/status", url.Values{"status": {"doing"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status code %d", resp.StatusCode)
	}
	if it, _ := e.st.GetItem(ctx, id); it.Status != model.StatusDoing {
		t.Fatalf("status = %s", it.Status)
	}
	if resp, _ := e.post(t, "/items/1/status", url.Values{"status": {"open"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid status accepted: %d", resp.StatusCode)
	}
	if resp, _ := e.post(t, "/items/99/status", url.Values{"status": {"done"}}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing item status code %d", resp.StatusCode)
	}
	e.post(t, "/items/1/deep", url.Values{"now": {"1"}})
	it, _ := e.st.GetItem(ctx, id)
	if it.Level != model.LevelDeep || it.ProcessAttempts != 0 || it.ProcessError != "" || it.ProcessedLevel != model.LevelLight || e.batches.Load() != 1 {
		t.Fatalf("after deep: %+v batches=%d", it, e.batches.Load())
	}
	if code, _ := e.get(t, "/items/99"); code != http.StatusNotFound {
		t.Fatalf("missing item code %d", code)
	}
}

func TestBoardCrossSitePostRejected(t *testing.T) {
	e := newEnv(t)
	e.item(t, &model.Item{MsgID: "1", RawText: "x", Category: model.CatTodo})
	for _, h := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://evil.example"},
	} {
		for _, path := range []string{"/items/1/status", "/items/1/deep", "/items/1", "/batch/run"} {
			req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader("status=done&category=idea&now=1"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for k, v := range h {
				req.Header.Set(k, v)
			}
			resp, err := e.client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %v: code %d, want 403", path, h, resp.StatusCode)
			}
		}
	}
	if it, _ := e.st.GetItem(context.Background(), 1); it.Status != model.StatusOpen || it.Category != model.CatTodo || it.Level == model.LevelDeep || e.batches.Load() != 0 {
		t.Fatal("cross-site POST took effect")
	}
}

func TestXSSIsEscaped(t *testing.T) {
	e := newEnv(t)
	e.item(t, &model.Item{MsgID: "1", RawText: `<script>alert(1)</script>`, Title: `<img src=x onerror=alert(2)>`,
		Detail: "<script>alert(3)</script>\n\n[点我](javascript:alert(4))", URL: "javascript:alert(5)", Category: model.CatResearch})
	for _, p := range []string{"/?cat=research", "/items/1"} {
		code, body := e.get(t, p)
		if code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
		mustNotContain(t, body, "<script>alert", "<img src=x", `href="javascript:`)
	}
	resp, _ := e.client().Get(e.srv.URL + "/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("csp = %q", csp)
	}
}

// 点子、资料默认就列出（计数也算上）；待办等分类可在「未处理 / 已处理」之间切换，点子、资料没有这个切换。
func TestBoardOpenAndDoneViews(t *testing.T) {
	e := newEnv(t)
	e.item(t, &model.Item{MsgID: "1", RawText: "还没办的事", Category: model.CatTodo})
	e.item(t, &model.Item{MsgID: "2", RawText: "已经办完的事", Category: model.CatTodo, Status: model.StatusDone})
	e.item(t, &model.Item{MsgID: "3", RawText: "取消了的事", Category: model.CatTodo, Status: model.StatusCancelled})
	e.item(t, &model.Item{MsgID: "4", RawText: "一个点子", Category: model.CatIdea})
	e.item(t, &model.Item{MsgID: "5", RawText: "一份资料", Category: model.CatArchive})

	_, body := e.get(t, "/?cat=todo")
	mustContain(t, body, "待办 <b>1</b>", "点子 <b>1</b>", "资料 <b>1</b>", "#1 还没办的事",
		`<a href="/?cat=todo" aria-current="page">未处理 <b>1</b></a>`, `<a href="/?cat=todo&amp;done=1" >已处理 <b>2</b></a>`)
	mustNotContain(t, body, "#2 已经办完的事", "#3 取消了的事")

	_, body = e.get(t, "/?cat=todo&done=1")
	mustContain(t, body, "#2 已经办完的事", "#3 取消了的事", `aria-current="page">已处理 <b>2</b></a>`)
	mustNotContain(t, body, "#1 还没办的事")

	for cat, want := range map[string]string{"idea": "#4 一个点子", "archive": "#5 一份资料"} {
		_, body = e.get(t, "/?cat="+cat)
		mustContain(t, body, want)
		mustNotContain(t, body, "未处理 <b>", "已处理 <b>")
		_, body = e.get(t, "/?cat="+cat+"&done=1") // 没有已处理视图的分类忽略 done
		mustContain(t, body, want)
	}
}
