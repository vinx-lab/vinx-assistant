package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type fieldResp struct {
	Item  itemView `json:"item"`
	Error string   `json:"error"`
}

func (e *env) field(t *testing.T, path string, v url.Values) (int, fieldResp) {
	t.Helper()
	resp, body := e.post(t, path, v)
	var r fieldResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("%s: 不是 JSON：%d %s", path, resp.StatusCode, body)
	}
	return resp.StatusCode, r
}

// 原地编辑：每个字段单独保存，只改这一个字段，返回条目当前状态。
func TestItemFieldSave(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.item(t, &model.Item{MsgID: "1", RawText: "合同\n第二行", Category: model.CatInbox, Title: "AI 标题", Priority: model.PriorityLow})
	e.st.SetTags(ctx, id, store.TagKindTopic, []string{"法务"})
	p := "/items/1/field"

	// 标题：手动改；清空交回 AI（TitleBy 置空）
	code, r := e.field(t, p, url.Values{"field": {"title"}, "title": {"  我的标题  "}})
	if it, _ := e.st.GetItem(ctx, id); code != 200 || r.Item.Display != "我的标题" || it.Title != "我的标题" || it.TitleBy != "manual" || it.Priority != model.PriorityLow || len(it.Topics) != 1 {
		t.Fatalf("title: %d %+v %+v", code, r, it)
	}
	code, r = e.field(t, p, url.Values{"field": {"title"}, "title": {""}})
	if it, _ := e.st.GetItem(ctx, id); code != 200 || it.Title != "" || it.TitleBy != "" || r.Item.Display != "合同" {
		t.Fatalf("clear title: %d %+v %+v", code, r, it)
	}

	// 分类：选中即存；已整理的不能改回未整理
	code, r = e.field(t, p, url.Values{"field": {"category"}, "category": {"todo"}})
	if it, _ := e.st.GetItem(ctx, id); code != 200 || r.Item.CategoryName != "待办" || it.CategoryBy != model.ByManual || it.Status != model.StatusOpen {
		t.Fatalf("category: %d %+v %+v", code, r, it)
	}
	if code, r = e.field(t, p, url.Values{"field": {"category"}, "category": {"inbox"}}); code != 400 || r.Error != "不能手动改回未整理" {
		t.Fatalf("inbox: %d %+v", code, r)
	}
	if code, r = e.field(t, p, url.Values{"field": {"category"}, "category": {"nope"}}); code != 400 || r.Error != "分类不对" {
		t.Fatalf("bad category: %d %+v", code, r)
	}

	// 优先级
	if code, r = e.field(t, p, url.Values{"field": {"priority"}, "priority": {"high"}}); code != 200 || r.Item.PriorityName != "高" {
		t.Fatalf("priority: %d %+v", code, r)
	}
	if code, r = e.field(t, p, url.Values{"field": {"priority"}, "priority": {"urgent"}}); code != 400 || r.Error != "优先级不对" {
		t.Fatalf("bad priority: %d %+v", code, r)
	}

	// 截止：日期 + 可选时刻；清除；只有时刻报错
	code, r = e.field(t, p, url.Values{"field": {"due"}, "due_date": {"2026-10-08"}, "due_time": {"15:00"}})
	if it, _ := e.st.GetItem(ctx, id); code != 200 || !it.DueHasTime || !it.DueAt.Equal(clock.At(2026, 10, 8, 15, 0)) || r.Item.DueDate != "2026-10-08" || r.Item.DueTime != "15:00" || !strings.Contains(r.Item.Due, "15:00") {
		t.Fatalf("due: %d %+v %+v", code, r, it)
	}
	if code, r = e.field(t, p, url.Values{"field": {"due"}, "due_time": {"15:00"}}); code != 400 || r.Error != "填了时刻就要填日期" {
		t.Fatalf("bad due: %d %+v", code, r)
	}
	if code, r = e.field(t, p, url.Values{"field": {"due"}, "due_date": {"2026-13-01"}}); code != 400 || r.Error != "日期格式不对" {
		t.Fatalf("bad date: %d %+v", code, r)
	}
	if code, r = e.field(t, p, url.Values{"field": {"due"}}); code != 200 || r.Item.Due != "" {
		t.Fatalf("clear due: %d %+v", code, r)
	}

	// 两种标签各自整组保存；同名时类别标签优先
	if code, r = e.field(t, p, url.Values{"field": {"labels"}, "labels": {"票据,法务"}}); code != 200 || strings.Join(r.Item.Labels, ",") != "票据,法务" && strings.Join(r.Item.Labels, ",") != "法务,票据" {
		t.Fatalf("labels: %d %+v", code, r)
	}
	if code, r = e.field(t, p, url.Values{"field": {"topics"}, "topics": {"#合同、法务"}}); code != 200 || strings.Join(r.Item.Topics, ",") != "合同" {
		t.Fatalf("topics: %d %+v", code, r)
	}
	if code, r = e.field(t, p, url.Values{"field": {"topics"}, "topics": {""}}); code != 200 || len(r.Item.Topics) != 0 || len(r.Item.Labels) != 2 {
		t.Fatalf("clear topics: %d %+v", code, r)
	}
	it, _ := e.st.GetItem(ctx, id)
	if it.Priority != model.PriorityHigh || it.Category != model.CatTodo || it.DueAt != nil {
		t.Fatalf("其他字段被动过：%+v", it)
	}

	// 不认识的字段、不存在的条目
	if code, r = e.field(t, p, url.Values{"field": {"raw_text"}}); code != 400 || r.Error == "" {
		t.Fatalf("unknown field: %d %+v", code, r)
	}
	for _, f := range []string{"title", "labels"} {
		if code, r = e.field(t, "/items/99/field", url.Values{"field": {f}, f: {"x"}}); code != 404 || r.Error != "条目不存在" {
			t.Fatalf("missing %s: %d %+v", f, code, r)
		}
	}
}

func TestItemFieldGuards(t *testing.T) {
	e := newEnv(t)
	e.item(t, &model.Item{MsgID: "1", RawText: "合同", Category: model.CatTodo})
	// 跨站被拒
	for _, h := range []map[string]string{{"Sec-Fetch-Site": "cross-site"}, {"Origin": "https://evil.example"}} {
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/items/1/field", strings.NewReader("field=title&title=x"))
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
			t.Errorf("%v: %d", h, resp.StatusCode)
		}
	}
	// 开启登录后未登录：401（前端据此跳登录页），不跳转
	e.setPassword(t, testPW)
	resp, _ := e.post(t, "/items/1/field", url.Values{"field": {"title"}, "title": {"x"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录：%d", resp.StatusCode)
	}
	c := e.signin(t, "", testPW)
	resp, _ = e.do(t, http.MethodPost, "/items/1/field", url.Values{"field": {"title"}, "title": {"登录后改"}}, c)
	if it, _ := e.st.GetItem(context.Background(), 1); resp.StatusCode != 200 || it.Title != "登录后改" {
		t.Fatalf("登录后：%d %+v", resp.StatusCode, it)
	}
}

func TestItemFieldBasePath(t *testing.T) {
	e := newEnvBase(t, "/todo")
	e.item(t, &model.Item{MsgID: "1", RawText: "合同", Category: model.CatTodo})
	_, body := e.get(t, "/todo/items/1")
	mustContain(t, body, `data-field-url="/todo/items/1/field"`)
	if code, r := e.field(t, "/todo/items/1/field", url.Values{"field": {"priority"}, "priority": {"medium"}}); code != 200 || r.Item.Priority != "medium" {
		t.Fatalf("base: %d %+v", code, r)
	}
	if code, _ := e.post(t, "/items/1/field", url.Values{"field": {"priority"}, "priority": {"low"}}); code.StatusCode != http.StatusNotFound {
		t.Fatalf("前缀之外：%d", code.StatusCode)
	}
}

// 详情页：原地编辑的控件都在；整表表单只在 <noscript> 里（开脚本时不出现在页面下方）。
func TestItemPageInlineEdit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.item(t, &model.Item{MsgID: "1", RawText: "合同", Category: model.CatTodo})
	e.st.SetTags(ctx, id, store.TagKindLabel, []string{"票据"})
	e.st.SetTags(ctx, id, store.TagKindTopic, []string{"法务"})
	_, body := e.get(t, "/items/1")
	mustContain(t, body,
		`data-field-url="/items/1/field"`, `<button type="button" class="icon-btn pen js-only" data-edit-title aria-label="改标题">`,
		`<select data-field="category">`, `<select data-field="priority">`, `data-due-save`, `加截止时间`,
		`data-tags="labels"`, `data-tags="topics"`, `aria-label="删除类别标签 票据"`, `aria-label="删除内容标签 法务"`,
		`<input list="dl-labels"`, `<datalist id="dl-topics"><option value="法务"></option></datalist>`)
	form := strings.Index(body, `action="/items/1" class="stack"`)
	open, end := strings.LastIndex(body[:max(form, 0)], "<noscript>"), strings.LastIndex(body[:max(form, 0)], "</noscript>")
	if form < 0 || open < 0 || end > open {
		t.Fatalf("整表表单应在 <noscript> 里：form=%d noscript=%d /noscript=%d", form, open, end)
	}
	mustNotContain(t, body, "onclick", "ondblclick", "<script>")
}
