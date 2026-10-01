// Package llmtest 是测试用的假 OpenAI 兼容服务商：按队列返回预设响应，记录收到的请求。
package llmtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Reply 是一次预设响应。Status 非 0 且不是 200 时返回该 HTTP 状态；Raw 非空时原样作为响应体。
type Reply struct {
	Status           int
	Content          string
	Raw              string
	PromptTokens     int64
	CompletionTokens int64
	FinishReason     string // 空时按 "stop"
}

// JSON 把 v 编码成 AI 的回答内容，用量记为 100/50。
func JSON(v any) Reply {
	b, _ := json.Marshal(v)
	return Reply{Content: string(b), PromptTokens: 100, CompletionTokens: 50}
}

type Captured struct {
	Auth      string
	Model     string
	MaxTokens int
	Text      string // 所有消息里的文本，按顺序拼接
	Body      string // 原始请求体
}

type Server struct {
	*httptest.Server
	ModelIDs []string
	mu       sync.Mutex
	queue    []Reply
	reqs     []Captured
}

func New() *Server {
	s := &Server{ModelIDs: []string{"test-model"}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("GET /v1/models", s.models)
	s.Server = httptest.NewServer(mux)
	return s
}

func (s *Server) Enqueue(r ...Reply) {
	s.mu.Lock()
	s.queue = append(s.queue, r...)
	s.mu.Unlock()
}

func (s *Server) Requests() []Captured {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Captured(nil), s.reqs...)
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(body, &req)
	var text []string
	for _, m := range req.Messages {
		var str string
		if json.Unmarshal(m.Content, &str) == nil {
			text = append(text, str)
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		json.Unmarshal(m.Content, &parts)
		for _, p := range parts {
			if p.Type == "text" {
				text = append(text, p.Text)
			}
		}
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, Captured{Auth: r.Header.Get("Authorization"), Model: req.Model, MaxTokens: req.MaxTokens, Text: strings.Join(text, "\n"), Body: string(body)})
	var rep Reply
	ok := len(s.queue) > 0
	if ok {
		rep, s.queue = s.queue[0], s.queue[1:]
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "llmtest: 没有排队的响应", http.StatusInternalServerError)
		return
	}
	if rep.Status != 0 && rep.Status != http.StatusOK {
		w.WriteHeader(rep.Status)
		io.WriteString(w, rep.Raw)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if rep.Raw != "" {
		io.WriteString(w, rep.Raw)
		return
	}
	finish := rep.FinishReason
	if finish == "" {
		finish = "stop"
	}
	json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": rep.Content}, "finish_reason": finish}},
		"usage":   map[string]int64{"prompt_tokens": rep.PromptTokens, "completion_tokens": rep.CompletionTokens},
	})
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	var data []map[string]string
	for _, id := range s.ModelIDs {
		data = append(data, map[string]string{"id": id})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}
