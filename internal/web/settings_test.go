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
