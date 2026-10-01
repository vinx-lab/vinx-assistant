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
	for _, p := range []string{"/settings", "/login"} {
		_, body := e.get(t, p)
		mustNotContain(t, body, secretKey, "sk-very-secret")
	}
	_, body := e.get(t, "/settings")
	mustContain(t, body, "***7890", "主力2", `value="m-small"`)
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
	_, body := e.get(t, "/settings")
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
	_, body := e.get(t, "/settings")
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
	_, body = e.get(t, "/settings")
	mustContain(t, body,
		`<option value="m-small" >m-small</option><option value="m-large" selected>m-large</option>`,   // 轻量：列表里的当前值被选中
		`<option value="custom-y" selected>custom-y</option><option value="m-small" >m-small</option>`, // 中等：列表外的当前值也列出
		"data-models=\"m-small\nm-large\"", "已保存 2 个模型")
	mustNotContain(t, body, secretKey)
	if st, _ = e.st.LoadSettings(ctx); st.AI.Light.Model != "m-large" || st.AI.Medium.Model != "custom-y" {
		t.Fatalf("dropdown save %+v %+v", st.AI.Light, st.AI.Medium)
	}

	// 改了 API 地址：旧地址的模型列表作废
	e.post(t, "/settings/providers", url.Values{"id": {pid}, "name": {"主力"}, "base_url": {"https://api2.example.com"}, "api_key": {"sk-new-key-0000"}})
	if st, _ = e.st.LoadSettings(ctx); len(st.AI.Providers[0].Models) != 0 {
		t.Fatalf("models kept after base change: %v", st.AI.Providers[0].Models)
	}
}
