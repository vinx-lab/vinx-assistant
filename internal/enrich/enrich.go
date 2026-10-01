// Package enrich 是不调用 AI 的轻处理：从原文里取链接，抓网页标题和简介。
package enrich

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

var urlRe = regexp.MustCompile(`https?://[^\s<>"'，。！？、；：（）【】《》「」]+`)

// ExtractURL 返回原文里的第一个 http(s) 链接，去掉粘在末尾的标点。
func ExtractURL(text string) string {
	return strings.TrimRight(urlRe.FindString(text), ".,;:!?)]}'\"")
}

type Fetcher struct {
	HTTP      *http.Client
	UserAgent string
	MaxBytes  int64
	Timeout   time.Duration
	GitHubAPI string // GitHub API 地址，空串表示 https://api.github.com；测试时指向假服务器
}

func NewFetcher(hc *http.Client) *Fetcher {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Fetcher{HTTP: hc, UserAgent: "Mozilla/5.0 (compatible; VinxAssistant/0.1)", MaxBytes: 2 << 20, Timeout: 10 * time.Second}
}

func (f *Fetcher) get(ctx context.Context, u string) (*html.Node, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, nil, fmt.Errorf("enrich: HTTP %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if mt, _, _ := mime.ParseMediaType(ct); ct != "" && mt != "text/html" && mt != "application/xhtml+xml" {
		return nil, nil, fmt.Errorf("enrich: 不是网页（%s）", mt)
	}
	r, err := charset.NewReader(io.LimitReader(resp.Body, f.MaxBytes), ct)
	if err != nil {
		return nil, nil, err
	}
	doc, err := html.Parse(r)
	return doc, resp.Header, err
}

// Meta 取 <title> 和简介（og:description，没有时用 name=description）。
func (f *Fetcher) Meta(ctx context.Context, u string) (title, desc string, err error) {
	doc, _, err := f.get(ctx, u)
	if err != nil {
		return "", "", err
	}
	var ogTitle, ogDesc, nameDesc string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if title == "" {
					title = textOf(n)
				}
			case "meta":
				prop, name, content := attr(n, "property"), attr(n, "name"), attr(n, "content")
				switch {
				case prop == "og:title":
					ogTitle = content
				case prop == "og:description":
					ogDesc = content
				case strings.EqualFold(name, "description"):
					nameDesc = content
				}
			case "body":
				return // 标题和 meta 都在 head 里
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if title == "" {
		title = ogTitle
	}
	if ogDesc == "" {
		ogDesc = nameDesc
	}
	return model.TruncateRunes(collapse(title), 200), model.TruncateRunes(collapse(ogDesc), 500), nil
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// Linker 把抓到的标题和简介写回条目，实现 ingest.Enricher。失败只记日志。
type Linker struct {
	Fetcher *Fetcher
	Store   *store.Store
	Log     *slog.Logger
}

func (l *Linker) Enrich(ctx context.Context, itemID int64, u string) {
	title, desc, err := l.Fetcher.Meta(ctx, u)
	if err != nil {
		if l.Log != nil {
			l.Log.Info("抓取网页标题失败", "item", itemID, "err", err)
		}
		return
	}
	if err := l.Store.SetLink(ctx, itemID, u, title, desc); err != nil && l.Log != nil {
		l.Log.Error("写入网页标题失败", "item", itemID, "err", err)
	}
}
