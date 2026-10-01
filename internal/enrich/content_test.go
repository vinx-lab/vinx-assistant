package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGithubRepo(t *testing.T) {
	cases := []struct {
		in          string
		owner, repo string
		ok          bool
	}{
		{"https://github.com/vinx-lab/demo", "vinx-lab", "demo", true},
		{"https://github.com/vinx-lab/demo.git", "vinx-lab", "demo", true},
		{"https://www.github.com/vinx-lab/demo/tree/main/docs", "vinx-lab", "demo", true},
		{"https://github.com/vinx-lab", "", "", false},
		{"https://github.com/topics/go", "", "", false},
		{"https://gist.github.com/a/b", "", "", false},
		{"https://example.com/a/b", "", "", false},
	}
	for _, c := range cases {
		o, r, ok := githubRepo(c.in)
		if o != c.owner || r != c.repo || ok != c.ok {
			t.Errorf("githubRepo(%q) = %q %q %v", c.in, o, r, ok)
		}
	}
}

func TestContentGitHubReadme(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/vinx-lab/demo/readme" || r.Header.Get("Accept") != "application/vnd.github.raw" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("# Demo\n\n这是 README 内容"))
	}))
	defer gh.Close()
	f := NewFetcher(nil)
	f.GitHubAPI = gh.URL
	got, err := f.Content(context.Background(), "https://github.com/vinx-lab/demo", 1000)
	if err != nil || got != "# Demo\n\n这是 README 内容" {
		t.Fatalf("got %q err=%v", got, err)
	}
	if _, err := f.Content(context.Background(), "https://github.com/vinx-lab/missing", 1000); err == nil {
		t.Fatal("want error for missing repo")
	}
}

func TestContentHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>t</title><script>var secret = 1</script></head><body>
			<nav><li>导航项</li></nav><header><p>页头</p></header>
			<article><h1>大标题</h1><p>正文  第一段</p><ul><li>要点一</li></ul><pre>code()</pre></article>
			<aside><p>侧栏</p></aside><footer><p>页脚</p></footer></body></html>`))
	}))
	defer srv.Close()
	got, err := NewFetcher(nil).Content(context.Background(), srv.URL, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got != "大标题\n正文 第一段\n要点一\ncode()" {
		t.Fatalf("got %q", got)
	}
	short, _ := NewFetcher(nil).Content(context.Background(), srv.URL, 3)
	if short != "大标题…" {
		t.Fatalf("truncated = %q", short)
	}
}

func TestContentEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><script>only()</script></body></html>`))
	}))
	defer srv.Close()
	if _, err := NewFetcher(nil).Content(context.Background(), srv.URL, 100); err == nil || !strings.Contains(err.Error(), "正文") {
		t.Fatalf("err = %v", err)
	}
}
