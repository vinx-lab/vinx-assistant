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
	// FinishReason 是服务商给的结束原因；"length" 表示输出到了 max_tokens 被截断。
	FinishReason string
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

// maskedError 的文字已打码，同时保留原始错误链，调用方仍可 errors.Is / errors.As。
type maskedError struct {
	msg string
	err error
}

func (e *maskedError) Error() string { return e.msg }
func (e *maskedError) Unwrap() error { return e.err }

func (c *Client) wrap(err error) error {
	return &maskedError{msg: c.mask(err.Error()), err: err}
}

// DefaultTimeout 是单次请求的上限。深度档要读两万字正文、推理模型还要先想一阵，留足 5 分钟。
const DefaultTimeout = 300 * time.Second

func New(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{BaseURL: baseURL, HTTP: hc, Timeout: DefaultTimeout, apiKey: strings.TrimSpace(apiKey)}
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
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
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
		return c.wrap(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return c.wrap(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return c.wrap(err)
	}
	if resp.StatusCode/100 != 2 {
		return &HTTPError{Status: resp.StatusCode, Body: model.TruncateRunes(c.mask(strings.TrimSpace(string(data))), 300)}
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
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := c.do(ctx, http.MethodPost, "/chat/completions", body, &out); err != nil {
		return Response{}, err
	}
	if len(out.Choices) == 0 {
		if out.Error != nil && out.Error.Message != "" {
			return Response{}, fmt.Errorf("llm: 服务商返回错误：%s", model.TruncateRunes(c.mask(out.Error.Message), 300))
		}
		return Response{}, errors.New("llm: 响应里没有 choices")
	}
	return Response{Content: out.Choices[0].Message.Content, Usage: out.Usage, FinishReason: out.Choices[0].FinishReason}, nil
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
