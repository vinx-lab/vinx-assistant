// Package llm 是 OpenAI 兼容接口的最小客户端（/chat/completions、/models），不依赖任何厂商 SDK。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/redact"
)

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type Part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// Message 的 Content 是 string，或者带图片时的 []Part。
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func System(text string) Message { return Message{Role: "system", Content: text} }
func User(text string) Message   { return Message{Role: "user", Content: text} }

// UserWithImages 附上图片（data URL），一律用低清晰度模式省 token。
func UserWithImages(text string, dataURLs []string) Message {
	parts := []Part{{Type: "text", Text: text}}
	for _, u := range dataURLs {
		parts = append(parts, Part{Type: "image_url", ImageURL: &ImageURL{URL: u, Detail: "low"}})
	}
	return Message{Role: "user", Content: parts}
}

type Request struct {
	Model     string
	Messages  []Message
	MaxTokens int
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

func (u Usage) Total() int64 { return u.PromptTokens + u.CompletionTokens }

type Response struct {
	Content string
	Usage   Usage
}

type Chatter interface {
	Chat(ctx context.Context, req Request) (Response, error)
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("llm: HTTP %d：%s", e.Status, e.Body) }

type Client struct {
	BaseURL string
	HTTP    *http.Client
	Timeout time.Duration
	apiKey  string
}

func New(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{BaseURL: baseURL, HTTP: hc, Timeout: 180 * time.Second, apiKey: apiKey}
}

var versionSuffix = regexp.MustCompile(`/v\d+$`)

// Endpoint 拼接接口地址：地址末尾已有 /v1、/v4 这类版本段就直接接 path，否则先补 /v1。
func Endpoint(base, path string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if !versionSuffix.MatchString(b) {
		b += "/v1"
	}
	return b + path
}

func (c *Client) mask(s string) string {
	if c.apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.apiKey, redact.Secret(c.apiKey))
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, Endpoint(c.BaseURL, path), rdr)
	if err != nil {
		return errors.New(c.mask(err.Error()))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return errors.New(c.mask(err.Error()))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return errors.New(c.mask(err.Error()))
	}
	if resp.StatusCode/100 != 2 {
		return &HTTPError{Status: resp.StatusCode, Body: c.mask(model.TruncateRunes(strings.TrimSpace(string(data)), 300))}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("llm: 响应不是 JSON：%w", err)
	}
	return nil
}

func (c *Client) Chat(ctx context.Context, req Request) (Response, error) {
	body := map[string]any{"model": req.Model, "messages": req.Messages}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := c.do(ctx, http.MethodPost, "/chat/completions", body, &out); err != nil {
		return Response{}, err
	}
	if len(out.Choices) == 0 {
		return Response{}, errors.New("llm: 响应里没有 choices")
	}
	return Response{Content: out.Choices[0].Message.Content, Usage: out.Usage}, nil
}

// Models 拉取服务商的模型列表，按名称排序。
func (c *Client) Models(ctx context.Context) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/models", nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
