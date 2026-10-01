package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestExtractURL(t *testing.T) {
	cases := map[string]string{
		"0.23 复制打开抖音，看看【万言观潮的作品】从 AI 到超级智能... https://v.douyin.com/NL6qweT 复制此链接": "https://v.douyin.com/NL6qweT",
		"看看 https://github.com/x/y，挺好":     "https://github.com/x/y",
		"(https://example.com/a?b=1)":      "https://example.com/a?b=1",
		"没有链接":                             "",
		"两个 https://a.com 和 https://b.com": "https://a.com",
	}
	for in, want := range cases {
		if got := ExtractURL(in); got != want {
			t.Errorf("ExtractURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMetaUTF8(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>
		  示例 标题 </title><meta property="og:description" content="一段  简介"></head><body>x</body></html>`))
	}))
	defer srv.Close()
	title, desc, err := NewFetcher(nil).Meta(context.Background(), srv.URL)
	if err != nil || title != "示例 标题" || desc != "一段 简介" {
		t.Fatalf("title=%q desc=%q err=%v", title, desc, err)
	}
}

func TestMetaGBKAndNameDescription(t *testing.T) {
	body, _ := simplifiedchinese.GBK.NewEncoder().String(`<html><head><meta charset="gbk"><title>中文网页</title><meta name="description" content="描述"></head></html>`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(body))
	}))
	defer srv.Close()
	title, desc, err := NewFetcher(nil).Meta(context.Background(), srv.URL)
	if err != nil || title != "中文网页" || desc != "描述" {
		t.Fatalf("title=%q desc=%q err=%v", title, desc, err)
	}
}

func TestMetaNonHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF"))
	}))
	defer srv.Close()
	if _, _, err := NewFetcher(nil).Meta(context.Background(), srv.URL); err == nil {
		t.Fatal("want error for non-HTML")
	}
}
