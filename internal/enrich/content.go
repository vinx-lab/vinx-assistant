package enrich

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

const defaultGitHubAPI = "https://api.github.com"

// 这些路径段在 github.com 下不是用户名。
var githubReserved = map[string]bool{
	"orgs": true, "topics": true, "settings": true, "features": true, "marketplace": true,
	"sponsors": true, "about": true, "login": true, "explore": true, "trending": true,
	"collections": true, "search": true, "notifications": true, "pulls": true, "issues": true,
}

func githubRepo(raw string) (owner, repo string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" || githubReserved[strings.ToLower(parts[0])] {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}

// Content 抓正文：GitHub 仓库取 README 原文，其他网页取正文段落；超过 maxRunes 截断。
func (f *Fetcher) Content(ctx context.Context, rawURL string, maxRunes int) (string, error) {
	var text string
	if owner, repo, ok := githubRepo(rawURL); ok {
		t, err := f.readme(ctx, owner, repo)
		if err != nil {
			return "", err
		}
		text = t
	} else {
		doc, _, err := f.get(ctx, rawURL)
		if err != nil {
			return "", err
		}
		text = mainText(doc)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("enrich: 没有提取到正文")
	}
	return model.TruncateRunes(text, maxRunes), nil
}

func (f *Fetcher) readme(ctx context.Context, owner, repo string) (string, error) {
	base := strings.TrimRight(f.GitHubAPI, "/")
	if base == "" {
		base = defaultGitHubAPI
	}
	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/readme", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.raw")
	req.Header.Set("User-Agent", f.UserAgent)
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("enrich: 取 README 失败，HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes))
	return string(b), err
}

var skipTags = map[string]bool{
	"script": true, "style": true, "nav": true, "header": true, "footer": true, "aside": true,
	"noscript": true, "form": true, "svg": true, "iframe": true, "template": true,
}

var blockTags = map[string]bool{
	"p": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"li": true, "pre": true, "blockquote": true, "td": true,
}

// mainText 去掉导航、页头页脚、侧栏和脚本后，按块收集正文，每块一行。
func mainText(doc *html.Node) string {
	var lines []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skipTags[n.Data] {
				return
			}
			if blockTags[n.Data] {
				if t := collapse(textOf(n)); t != "" {
					lines = append(lines, t)
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.Join(lines, "\n")
}
