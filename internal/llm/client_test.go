package llm_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/llm"
	"github.com/vinx-lab/vinx-assistant/internal/llm/llmtest"
)

const key = "sk-abcdef123456"

func TestEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com":             "https://api.example.com/v1/chat/completions",
		"https://api.example.com/v1/":         "https://api.example.com/v1/chat/completions",
		"https://open.example.cn/api/paas/v4": "https://open.example.cn/api/paas/v4/chat/completions",
		" http://localhost:11434/ ":           "http://localhost:11434/v1/chat/completions",
		"https://example.com/openai/v1beta":   "https://example.com/openai/v1beta/v1/chat/completions",
	}
	for in, want := range cases {
		if got := llm.Endpoint(in, "/chat/completions"); got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChat(t *testing.T) {
	srv := llmtest.New()
	defer srv.Close()
	srv.Enqueue(llmtest.Reply{Content: "你好", PromptTokens: 12, CompletionTokens: 3})
	c := llm.New(srv.URL, key, nil)
	resp, err := c.Chat(context.Background(), llm.Request{
		Model:     "m1",
		Messages:  []llm.Message{llm.System("规则"), llm.UserWithImages("看图", []string{"data:image/png;base64,AAAA"})},
		MaxTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "你好" || resp.Usage.Total() != 15 {
		t.Fatalf("resp = %+v", resp)
	}
	r := srv.Requests()[0]
	if r.Auth != "Bearer "+key || r.Model != "m1" || r.MaxTokens != 100 || r.Text != "规则\n看图" {
		t.Fatalf("captured = %+v", r)
	}
	if !strings.Contains(r.Body, `"detail":"low"`) || strings.Contains(r.Body, "response_format") {
		t.Fatalf("body = %s", r.Body)
	}
}

func TestChatHTTPErrorMasksKey(t *testing.T) {
	srv := llmtest.New()
	defer srv.Close()
	srv.Enqueue(llmtest.Reply{Status: 429, Raw: "rate limited for key " + key})
	_, err := llm.New(srv.URL, key, nil).Chat(context.Background(), llm.Request{Model: "m"})
	var he *llm.HTTPError
	if !errors.As(err, &he) || he.Status != 429 {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "***3456") {
		t.Fatalf("key not masked: %v", err)
	}
}

func TestChatBadEnvelope(t *testing.T) {
	srv := llmtest.New()
	defer srv.Close()
	srv.Enqueue(llmtest.Reply{Raw: "not json"}, llmtest.Reply{Raw: `{"choices":[]}`})
	c := llm.New(srv.URL, key, nil)
	if _, err := c.Chat(context.Background(), llm.Request{Model: "m"}); err == nil {
		t.Fatal("want error for non-JSON body")
	}
	if _, err := c.Chat(context.Background(), llm.Request{Model: "m"}); err == nil {
		t.Fatal("want error for empty choices")
	}
}

func TestModels(t *testing.T) {
	srv := llmtest.New()
	defer srv.Close()
	srv.ModelIDs = []string{"b-model", "a-model"}
	got, err := llm.New(srv.URL+"/v1", key, nil).Models(context.Background())
	if err != nil || !reflect.DeepEqual(got, []string{"a-model", "b-model"}) {
		t.Fatalf("models = %v err=%v", got, err)
	}
}
