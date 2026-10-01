package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestNormalizeBasePath(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "todo": "/todo", "/todo/": "/todo", "/a/b/": "/a/b"} {
		if got, err := NormalizeBasePath(in); err != nil || got != want {
			t.Errorf("%q -> %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"/a?b", "/a#b", "/a b", "/../x", "/a/..", "/a//b", `/a\b`, "/a%2fb", "/a\nb"} {
		if _, err := NormalizeBasePath(bad); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}

var attrRe = regexp.MustCompile(`\b(href|src|action|formaction)="([^"]*)"`)

// 页面里所有站内地址都带前缀；外部链接和锚点除外。
func checkPrefixed(t *testing.T, path, body string) {
	t.Helper()
	ms := attrRe.FindAllStringSubmatch(body, -1)
	if len(ms) == 0 {
		t.Errorf("%s: 没扫到任何地址", path)
	}
	for _, m := range ms {
		v := m[2]
		if strings.HasPrefix(v, "#") || strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
			continue
		}
		if !strings.HasPrefix(v, "/todo/") {
			t.Errorf("%s: %s=%q 没有 /todo/ 前缀", path, m[1], v)
		}
	}
}

func TestBasePathPages(t *testing.T) {
	e := newEnvBase(t, "/todo")
	past := clock.At(2026, 9, 30, 10, 0)
	e.item(t, &model.Item{MsgID: "1", RawText: "交房租", Category: model.CatTodo, DueAt: &past, DueHasTime: true, Summary: "报销"})
	e.item(t, &model.Item{MsgID: "2", RawText: "研究 htmx", Category: model.CatResearch})
	paths := []string{"/", "/?cat=todo", "/?cat=todo&done=1", "/items/1", "/search?q=htmx", "/usage", "/login",
		"/settings", "/settings/providers", "/settings/models", "/settings/prompt", "/settings/rules", "/settings/keywords"}
	for _, p := range paths {
		code, body := e.get(t, "/todo"+p)
		if code != http.StatusOK {
			t.Fatalf("%s: code %d", p, code)
		}
		checkPrefixed(t, p, body)
		mustContain(t, body, `data-base="/todo"`)
	}
	_, body := e.get(t, "/todo/static/app.js")
	mustContain(t, body, "data-base")
	if code, _ := e.get(t, "/todo/static/app.css"); code != http.StatusOK {
		t.Fatalf("css code %d", code)
	}
}

func TestBasePathRoutingAndRedirects(t *testing.T) {
	e := newEnvBase(t, "/todo")
	e.item(t, &model.Item{MsgID: "1", RawText: "交房租", Category: model.CatTodo})

	resp, err := e.client().Get(e.srv.URL + "/todo?x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "/todo/?x=1" {
		t.Fatalf("/todo: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, p := range []string{"/", "/settings", "/items/1", "/static/app.css", "/login", "/todoo/"} {
		if code, _ := e.get(t, p); code != http.StatusNotFound {
			t.Errorf("%s 无前缀应 404，得到 %d", p, code)
		}
	}

	resp, _ = e.post(t, "/todo/items/1/status", url.Values{"status": {"done"}, "back": {"/?cat=todo"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/todo/?cat=todo&msg=status" {
		t.Fatalf("status: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, bad := range []string{"https://evil.example/", "//evil.example/x", "/\\evil.example", "evil"} {
		resp, _ = e.post(t, "/todo/items/1/status", url.Values{"status": {"open"}, "back": {bad}})
		loc := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/todo/") || strings.HasPrefix(loc, "/todo//") {
			t.Errorf("back=%q -> %d %s", bad, resp.StatusCode, loc)
		}
	}
	resp, _ = e.post(t, "/todo/items/1/deep", url.Values{})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/todo/") {
		t.Fatalf("deep: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = e.post(t, "/todo/login/start", url.Values{})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/todo/login" {
		t.Fatalf("login: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = e.postFrom(t, "/todo/items/1/status", url.Values{"status": {"done"}}, "cross-site")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site: %d", resp.StatusCode)
	}
}
