package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestSearchPage(t *testing.T) {
	e := newEnv(t)
	e.item(t, &model.Item{MsgID: "1", RawText: "月底前处理报销发票"})
	e.item(t, &model.Item{MsgID: "2", RawText: "无关"})
	_, body := e.get(t, "/search?q="+url.QueryEscape("发票"))
	mustContain(t, body, "找到 1 条", "#1 月底前处理报销发票")
	_, body = e.get(t, "/search?q="+url.QueryEscape("无关"))
	mustContain(t, body, "找到 1 条") // 2 个汉字走 LIKE
	_, body = e.get(t, "/search?q="+url.QueryEscape(`报销" OR "无关`))
	mustContain(t, body, "找到 0 条")
	code, body := e.get(t, "/search?from=2026-13-01")
	if code != http.StatusBadRequest || !strings.Contains(body, "日期格式不对") {
		t.Fatalf("bad date: %d", code)
	}
	if code, _ = e.get(t, "/search"); code != 200 {
		t.Fatalf("empty search code %d", code)
	}
	e.item(t, &model.Item{MsgID: "3", RawText: `<script>alert(1)</script>`})
	_, body = e.get(t, "/search?q="+url.QueryEscape("script"))
	mustContain(t, body, "找到 1 条")
	mustNotContain(t, body, "<script>alert")
}

func TestSearchDateToInclusive(t *testing.T) {
	e := newEnv(t) // 时钟 2026-10-01 09:00
	e.item(t, &model.Item{MsgID: "1", RawText: "今天收到的东西"})
	_, body := e.get(t, "/search?from=2026-10-01&to=2026-10-01")
	mustContain(t, body, "找到 1 条")
	_, body = e.get(t, "/search?to=2026-09-30")
	mustContain(t, body, "找到 0 条")
	_, body = e.get(t, "/search?from=2026-10-02")
	mustContain(t, body, "找到 0 条")
}

func TestNavLinks(t *testing.T) {
	e := newEnv(t)
	_, body := e.get(t, "/")
	mustContain(t, body, `href="/search"`, `href="/settings"`)
	// 用量并入设置的二级菜单
	_, body = e.get(t, "/settings/models")
	mustContain(t, body, `href="/usage"`)
}
