package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestSettingsProvidersNeverLeakKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	resp, _ := e.post(t, "/settings/providers", url.Values{"name": {"主力"}, "base_url": {"https://api.example.com/v1"}, "api_key": {secretKey}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add code %d", resp.StatusCode)
	}
	st, _ := e.st.LoadSettings(ctx)
	if len(st.AI.Providers) != 1 || st.AI.Providers[0].APIKey != secretKey {
		t.Fatalf("providers %+v", st.AI.Providers)
	}
	pid := st.AI.Providers[0].ID
	// 编辑时密钥留空 = 不改
	e.post(t, "/settings/providers", url.Values{"id": {pid}, "name": {"主力2"}, "base_url": {"https://api.example.com/v1"}, "api_key": {""}})
	st, _ = e.st.LoadSettings(ctx)
	if st.AI.Providers[0].APIKey != secretKey || st.AI.Providers[0].Name != "主力2" {
		t.Fatalf("edit lost key: %+v", st.AI.Providers[0])
	}
	e.post(t, "/settings/models", url.Values{"light_provider": {pid}, "light_model": {"m-small"}})
	// 设置分小节后，每个小节和登录页都不能出现密钥
	for _, p := range []string{"/settings", "/settings/models", "/settings/rules", "/settings/keywords", "/settings/prompt", "/login", "/usage"} {
		_, body := e.get(t, p)
		mustNotContain(t, body, secretKey, "sk-very-secret")
	}
	_, body := e.get(t, "/settings")
	mustContain(t, body, "***7890", "主力2")
	_, body = e.get(t, "/settings/models")
	mustContain(t, body, `value="m-small"`)
	// 拉模型列表
	resp, body = e.post(t, "/settings/providers/"+pid+"/models", nil)
	var got map[string][]string
	json.Unmarshal([]byte(body), &got)
	if resp.StatusCode != 200 || len(got["models"]) != 2 {
		t.Fatalf("models: %d %s", resp.StatusCode, body)
	}
	// 出错时错误里的密钥打码
	e.post(t, "/settings/providers", url.Values{"name": {"坏的"}, "base_url": {"https://bad.example"}, "api_key": {secretKey}})
	st, _ = e.st.LoadSettings(ctx)
	resp, body = e.post(t, "/settings/providers/"+st.AI.Providers[1].ID+"/models", nil)
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(body, secretKey) {
		t.Fatalf("error leak: %d %s", resp.StatusCode, body)
	}
	// 删除时清空引用它的档位
	e.post(t, "/settings/providers/"+pid+"/delete", nil)
	st, _ = e.st.LoadSettings(ctx)
	if len(st.AI.Providers) != 1 || st.AI.Light.ProviderID != "" {
		t.Fatalf("after delete %+v", st.AI)
	}
}

func TestSettingsValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if resp, _ := e.post(t, "/settings/providers", url.Values{"name": {"x"}, "base_url": {"ftp://x"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad url accepted: %d", resp.StatusCode)
	}
	good := url.Values{"token_limit": {"0"}, "batch_times": {"9:00 21:00"}, "digest_time": {"8:30"}, "images": {"1"},
		"prefixes": {"待办=待办\n灵感=点子"}, "medium_keywords": {"研究一下"}, "deep_keywords": {"深入研究"}, "action_words": {"完成=done\n撤销=undo"}}
	if resp, _ := e.post(t, "/settings/general", good); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("good general: %d", resp.StatusCode)
	}
	st, _ := e.st.LoadSettings(ctx)
	if st.AI.DailyTokenLimit != 0 || !st.AI.Images || st.Schedule.DigestTime != "08:30" || st.Schedule.BatchTimes[0] != "09:00" || st.Rules.Prefixes[1].Category != model.CatIdea {
		t.Fatalf("saved %+v", st)
	}
	bad := url.Values{}
	for k, v := range good {
		bad[k] = v
	}
	bad.Set("prefixes", "待办=未知分类")
	resp, body := e.post(t, "/settings/general", bad)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "分类「未知分类」不认识") || !strings.Contains(body, "待办=未知分类") {
		t.Fatalf("bad general: %d", resp.StatusCode)
	}
	if again, _ := e.st.LoadSettings(ctx); len(again.Rules.Prefixes) != 2 {
		t.Fatal("invalid form must not save")
	}
	bad.Set("prefixes", good.Get("prefixes"))
	bad.Set("token_limit", "-1")
	if resp, _ := e.post(t, "/settings/general", bad); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("negative limit accepted")
	}
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/settings/providers", strings.NewReader("name=偷&base_url=https://evil.example"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := e.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site settings POST: %d", resp.StatusCode)
	}
}

func TestSettingsPromptSaveAndReset(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if resp, _ := e.post(t, "/settings/prompt", url.Values{"prompt": {"自定义说明ABC"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save prompt: %d", resp.StatusCode)
	}
	st, _ := e.st.LoadSettings(ctx)
	if st.Prompt != "自定义说明ABC" {
		t.Fatalf("prompt = %q", st.Prompt)
	}
	// 恢复默认
	if resp, _ := e.post(t, "/settings/prompt", url.Values{"prompt": {"自定义说明ABC"}, "reset": {"1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset prompt: %d", resp.StatusCode)
	}
	if st, _ = e.st.LoadSettings(ctx); st.Prompt != "" {
		t.Fatalf("prompt after reset = %q", st.Prompt)
	}
	// 留空也回落默认
	e.post(t, "/settings/prompt", url.Values{"prompt": {"x"}})
	e.post(t, "/settings/prompt", url.Values{"prompt": {"  "}})
	if st, _ = e.st.LoadSettings(ctx); st.Prompt != "" {
		t.Fatalf("blank prompt saved as %q", st.Prompt)
	}
	// 过长拒绝
	if resp, _ := e.post(t, "/settings/prompt", url.Values{"prompt": {strings.Repeat("长", maxPromptRunes+1)}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("too long prompt: %d", resp.StatusCode)
	}
}

func TestSettingsLabelRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	st, _ := e.st.LoadSettings(ctx)
	form := generalFormOf(st)
	form.LabelRules = "测试=测试\n发票＝票据"
	v := url.Values{"token_limit": {form.TokenLimit}, "batch_times": {form.BatchTimes}, "digest_time": {form.DigestTime}, "prefixes": {form.Prefixes},
		"action_words": {form.ActionWords}, "label_rules": {form.LabelRules}}
	if resp, _ := e.post(t, "/settings/general", v); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	st, _ = e.st.LoadSettings(ctx)
	if len(st.Rules.LabelRules) != 2 || st.Rules.LabelRules[1] != (model.LabelRule{Keyword: "发票", Label: "票据"}) {
		t.Fatalf("rules = %+v", st.Rules.LabelRules)
	}
	v.Set("label_rules", "没有等号")
	if resp, body := e.post(t, "/settings/general", v); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "关键词=标签") {
		t.Fatalf("bad rule: %d", resp.StatusCode)
	}
}

// 网站没有密码：改了服务商地址却留空密钥，会把旧密钥发到新地址，必须拒绝。
func TestProviderBaseURLChangeRequiresKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.post(t, "/settings/providers", url.Values{"name": {"主力"}, "base_url": {"https://api.example.com"}, "api_key": {secretKey}})
	st, _ := e.st.LoadSettings(ctx)
	pid := st.AI.Providers[0].ID

	resp, body := e.post(t, "/settings/providers", url.Values{"id": {pid}, "name": {"主力"}, "base_url": {"https://attacker.example"}, "api_key": {""}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "API 地址变了，请重新填写密钥") {
		t.Fatalf("code %d", resp.StatusCode)
	}
	mustNotContain(t, body, secretKey)
	if st, _ = e.st.LoadSettings(ctx); st.AI.Providers[0].BaseURL != "https://api.example.com" || st.AI.Providers[0].APIKey != secretKey {
		t.Fatalf("provider changed: %+v", st.AI.Providers[0])
	}
	// 地址和新密钥一起改：可以。
	resp, _ = e.post(t, "/settings/providers", url.Values{"id": {pid}, "name": {"主力"}, "base_url": {"https://api2.example.com"}, "api_key": {"sk-new-key-0000"}})
	if st, _ = e.st.LoadSettings(ctx); resp.StatusCode != http.StatusSeeOther || st.AI.Providers[0].BaseURL != "https://api2.example.com" || st.AI.Providers[0].APIKey != "sk-new-key-0000" {
		t.Fatalf("code %d provider %+v", resp.StatusCode, st.AI.Providers[0])
	}
	// 原来就没有密钥的服务商改地址：没有密钥可泄露，允许。
	e.post(t, "/settings/providers", url.Values{"name": {"本地"}, "base_url": {"http://127.0.0.1:11434"}})
	st, _ = e.st.LoadSettings(ctx)
	resp, _ = e.post(t, "/settings/providers", url.Values{"id": {st.AI.Providers[1].ID}, "name": {"本地"}, "base_url": {"http://127.0.0.1:8080"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("keyless provider edit: %d", resp.StatusCode)
	}
}

func TestSettingsShowsModelAdvice(t *testing.T) {
	e := newEnv(t)
	_, body := e.get(t, "/settings/models")
	mustContain(t, body, "三档建议都用 deepseek-chat", "deepseek-reasoner 的 max_tokens 含思考过程")
}

// 拉取模型列表后保存到服务商上；设置页三档渲染成下拉，当前值选中，不在列表里的当前值也列出；拿不到列表时手动输入照样能存。
func TestSettingsModelDropdown(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.post(t, "/settings/providers", url.Values{"name": {"主力"}, "base_url": {"https://api.example.com"}, "api_key": {secretKey}})
	st, _ := e.st.LoadSettings(ctx)
	pid := st.AI.Providers[0].ID

	// 还没拉取：模型下拉为空，手动输入展开
	_, body := e.get(t, "/settings/models")
	mustContain(t, body, `<select name="light_model" data-model-for="light"><option value="">没有可选模型</option></select>`, `<details class="manual" data-manual-box="light" open>`, `name="light_model_manual"`)

	// 手动输入照样能存（且优先于下拉）
	if resp, _ := e.post(t, "/settings/models", url.Values{"light_provider": {pid}, "light_model": {""}, "light_model_manual": {" custom-x "}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("manual save %d", resp.StatusCode)
	}
	if st, _ = e.st.LoadSettings(ctx); st.AI.Light.Model != "custom-x" {
		t.Fatalf("manual model %+v", st.AI.Light)
	}

	// 拉取后保存在服务商上
	if resp, _ := e.post(t, "/settings/providers/"+pid+"/models", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch %d", resp.StatusCode)
	}
	st, _ = e.st.LoadSettings(ctx)
	if got := st.AI.Providers[0].Models; len(got) != 2 || got[0] != "m-small" || got[1] != "m-large" {
		t.Fatalf("models not persisted: %v", got)
	}
	e.post(t, "/settings/models", url.Values{"light_provider": {pid}, "light_model": {"m-large"}, "medium_provider": {pid}, "medium_model": {"custom-y"}})
	_, body = e.get(t, "/settings/models")
	mustContain(t, body,
		`<option value="m-small" >m-small</option><option value="m-large" selected>m-large</option>`,   // 轻量：列表里的当前值被选中
		`<option value="custom-y" selected>custom-y</option><option value="m-small" >m-small</option>`, // 中等：列表外的当前值也列出
		"data-models=\"m-small\nm-large\"")
	mustNotContain(t, body, secretKey)
	_, body = e.get(t, "/settings") // 模型个数显示在服务商小节
	mustContain(t, body, "已保存 2 个模型")
	if st, _ = e.st.LoadSettings(ctx); st.AI.Light.Model != "m-large" || st.AI.Medium.Model != "custom-y" {
		t.Fatalf("dropdown save %+v %+v", st.AI.Light, st.AI.Medium)
	}

	// 改了 API 地址：旧地址的模型列表作废
	e.post(t, "/settings/providers", url.Values{"id": {pid}, "name": {"主力"}, "base_url": {"https://api2.example.com"}, "api_key": {"sk-new-key-0000"}})
	if st, _ = e.st.LoadSettings(ctx); len(st.AI.Providers[0].Models) != 0 {
		t.Fatalf("models kept after base change: %v", st.AI.Providers[0].Models)
	}
}

// 设置按小节分开：每个小节一个地址，小节导航标出当前项；保存后回到原小节；分小节保存时不动其他小节的字段。
func TestSettingsSections(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for path, want := range map[string]string{
		"/settings":          `<a href="/settings" aria-current="page">AI 服务商</a>`,
		"/settings/models":   `<a href="/settings/models" aria-current="page">模型</a>`,
		"/settings/rules":    `<a href="/settings/rules" aria-current="page">整理与规则</a>`,
		"/settings/keywords": `<a href="/settings/keywords" aria-current="page">关键词</a>`,
		"/settings/prompt":   `<a href="/settings/prompt" aria-current="page">提示词</a>`,
		"/login":             `<a href="/login" aria-current="page">微信登录</a>`,
		"/usage":             `<a href="/usage" aria-current="page">用量</a>`,
	} {
		code, body := e.get(t, path)
		if code != http.StatusOK {
			t.Fatalf("%s: code %d", path, code)
		}
		mustContain(t, body, want, `<a href="/settings" aria-current="page">设置</a>`)
	}
	if code, _ := e.get(t, "/settings/nope"); code != http.StatusNotFound {
		t.Fatalf("unknown section: %d", code)
	}
	_, body := e.get(t, "/settings/rules")
	mustContain(t, body, `name="batch_times"`, `name="section" value="rules"`)
	mustNotContain(t, body, `name="prefixes"`, `name="light_model"`)
	_, body = e.get(t, "/settings/keywords")
	mustContain(t, body, `name="prefixes"`, `name="action_words"`, `name="section" value="keywords"`)
	mustNotContain(t, body, `name="batch_times"`)

	before, _ := e.st.LoadSettings(ctx)
	resp, _ := e.post(t, "/settings/general", url.Values{"section": {"rules"}, "token_limit": {"777"}, "batch_times": {"07:00"}, "digest_time": {"07:30"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/rules?msg=saved" {
		t.Fatalf("rules save: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	st, _ := e.st.LoadSettings(ctx)
	if st.AI.DailyTokenLimit != 777 || st.Schedule.BatchTimes[0] != "07:00" || len(st.Rules.Prefixes) != len(before.Rules.Prefixes) || len(st.Rules.ActionWords) != len(before.Rules.ActionWords) {
		t.Fatalf("rules save touched keywords: %+v", st)
	}
	resp, _ = e.post(t, "/settings/general", url.Values{"section": {"keywords"}, "prefixes": {"买=待办"}, "action_words": {"完成=done"}})
	if resp.Header.Get("Location") != "/settings/keywords?msg=saved" {
		t.Fatalf("keywords save: %s", resp.Header.Get("Location"))
	}
	if st, _ = e.st.LoadSettings(ctx); st.AI.DailyTokenLimit != 777 || len(st.Rules.Prefixes) != 1 {
		t.Fatalf("keywords save: %+v", st)
	}
	// 出错时回显在出错的小节
	resp, body = e.post(t, "/settings/general", url.Values{"section": {"rules"}, "token_limit": {"-5"}, "batch_times": {"07:00"}, "digest_time": {"07:30"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad rules: %d", resp.StatusCode)
	}
	mustContain(t, body, `name="token_limit" inputmode="numeric" value="-5"`, `<a href="/settings/rules" aria-current="page">`)
	resp, _ = e.post(t, "/settings/models", url.Values{})
	if resp.Header.Get("Location") != "/settings/models?msg=saved" {
		t.Fatalf("models save: %s", resp.Header.Get("Location"))
	}
	// 标签关键词在关键词小节，随 section=keywords 保存
	resp, _ = e.post(t, "/settings/general", url.Values{"section": {"keywords"}, "prefixes": {"买=待办"}, "action_words": {"完成=done"}, "label_rules": {"发票=票据"}})
	if st, _ = e.st.LoadSettings(ctx); resp.StatusCode != http.StatusSeeOther || len(st.Rules.LabelRules) != 1 || st.AI.DailyTokenLimit != 777 {
		t.Fatalf("label rules save: %d %+v", resp.StatusCode, st.Rules.LabelRules)
	}
	_, body = e.get(t, "/settings/keywords")
	mustContain(t, body, `name="label_rules"`, "发票=票据")
	// 提示词小节：文本框、恢复默认、只读预览；保存后回到提示词小节
	_, body = e.get(t, "/settings/prompt")
	mustContain(t, body, `action="/settings/prompt"`, `name="prompt"`, `name="reset" value="1"`, `class="raw preview"`)
	resp, _ = e.post(t, "/settings/prompt", url.Values{"prompt": {"自定义说明"}})
	if resp.Header.Get("Location") != "/settings/prompt?msg=saved" {
		t.Fatalf("prompt save: %s", resp.Header.Get("Location"))
	}
	_, body = e.get(t, "/settings/prompt")
	mustContain(t, body, ">自定义说明</textarea>")
}
