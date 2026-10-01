package web

import (
	"strings"
	"testing"
)

func TestRenderMarkdownIsSafe(t *testing.T) {
	in := "## 是什么\n\n<script>alert(1)</script>\n\n正文 <img src=x onerror=alert(2)>\n\n[坏链接](javascript:alert(3)) [好链接](https://example.com)\n"
	out := string(renderMarkdown(in))
	for _, bad := range []string{"<script", "onerror", `href="javascript:`} {
		if strings.Contains(out, bad) {
			t.Errorf("unsafe %q in %s", bad, out)
		}
	}
	for _, good := range []string{"<h2>是什么</h2>", `href="https://example.com"`, `target="_blank"`, "noreferrer"} {
		if !strings.Contains(out, good) {
			t.Errorf("missing %q in %s", good, out)
		}
	}
}
