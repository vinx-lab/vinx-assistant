package llm_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestChatCancelKeepsChain(t *testing.T) {
	srv := llmtest.New()
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := llm.New(srv.URL, key, nil).Chat(ctx, llm.Request{Model: "m"})
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), key) {
		t.Fatalf("err = %v", err)
	}
}

func TestMaskBeforeTruncate(t *testing.T) {
	srv := llmtest.New()
	defer srv.Close()
	srv.Enqueue(llmtest.Reply{Status: 500, Raw: strings.Repeat("x", 295) + key + strings.Repeat("y", 50)})
	_, err := llm.New(srv.URL, key, nil).Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil || strings.Contains(err.Error(), "abcdef") {
		t.Fatalf("leak: %v", err)
	}
}

func TestKeyTrimmedAndNoTimeout(t *testing.T) {
	srv := llmtest.New()
	defer srv.Close()
	srv.Enqueue(llmtest.Reply{Content: "ok"})
	c := llm.New(srv.URL, " "+key+"\n", nil)
	c.Timeout = 0
	if _, err := c.Chat(context.Background(), llm.Request{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if a := srv.Requests()[0].Auth; a != "Bearer "+key {
		t.Fatalf("auth = %q", a)
	}
}

func TestChat200ErrorEnvelope(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":{"message":"quota exceeded for ` + key + `"}}`))
	}))
	defer ts.Close()
	_, err := llm.New(ts.URL, key, nil).Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") || strings.Contains(err.Error(), key) {
		t.Fatalf("err = %v", err)
	}
}

func TestChatFinishReasonAndDefaultTimeout(t *testing.T) {
	srv := llmtest.New()
	defer srv.Close()
	srv.Enqueue(llmtest.Reply{Content: `{"items":[`, FinishReason: "length"}, llmtest.Reply{Content: "ok"})
	c := llm.New(srv.URL, key, nil)
	if c.Timeout != 300*time.Second {
		t.Fatalf("timeout = %v", c.Timeout)
	}
	resp, err := c.Chat(context.Background(), llm.Request{Model: "m"})
	if err != nil || resp.FinishReason != "length" {
		t.Fatalf("resp = %+v err = %v", resp, err)
	}
	if resp, _ = c.Chat(context.Background(), llm.Request{Model: "m"}); resp.FinishReason != "stop" {
		t.Fatalf("resp = %+v", resp)
	}
}
