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

func TestRenderMarkdownImagesSafe(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		badList []string // 不应该出现的内容
		good    []string // 应该出现的内容
	}{
		{
			name:    "javascript image URL",
			input:   "![click me](javascript:alert(1))",
			badList: []string{"<img", `href="javascript:`, `src="javascript:`},
			good:    []string{"click me"}, // alt 文字保留
		},
		{
			name:    "data URL image",
			input:   "![x](data:text/html,<script>alert(1)</script>)",
			badList: []string{"<img", `src="data:`, `href="data:`},
			good:    []string{"x"}, // alt 文字保留
		},
		{
			name:    "vbscript URL",
			input:   "![xss](vbscript:alert(1))",
			badList: []string{"<img", `src="vbscript:`, `href="vbscript:`},
			good:    []string{"xss"},
		},
		{
			name:    "mixed-case javascript",
			input:   "![bad](JaVaScRiPt:alert(1))",
			badList: []string{"<img", "<a href=\"JaVaScRiPt:"},
			good:    []string{"bad"},
		},
		{
			name:    "entity trick javascript",
			input:   "![x](javascript&#58;alert(1))",
			badList: []string{"<img", "javascript"},
			good:    []string{"x"},
		},
		{
			name:    "HTTPS image becomes link",
			input:   "![alt text](https://example.com/image.jpg)",
			badList: []string{"<img", `<img src=`},
			good:    []string{`<a href="https://example.com/image.jpg"`, "alt text", `target="_blank"`, "noreferrer"},
		},
		{
			name:    "HTTP image becomes link",
			input:   "![pic](http://example.com/pic.png)",
			badList: []string{"<img"},
			good:    []string{`<a href="http://example.com/pic.png"`, "pic"},
		},
		{
			name:    "empty alt text defaults to 图片",
			input:   "![](https://example.com/img.gif)",
			badList: []string{"<img"},
			good:    []string{`<a href="https://example.com/img.gif"`, "图片"},
		},
		{
			name:    "relative URL shows alt only",
			input:   "![local](../images/pic.jpg)",
			badList: []string{"<img", "<a href"},
			good:    []string{"local"},
		},
		{
			name:    "no protocol shows alt only",
			input:   "![x](example.com/pic.jpg)",
			badList: []string{"<img", "<a href"},
			good:    []string{"x"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := string(renderMarkdown(c.input))
			for _, bad := range c.badList {
				if strings.Contains(out, bad) {
					t.Errorf("unsafe %q found in output:\n%s", bad, out)
				}
			}
			for _, good := range c.good {
				if !strings.Contains(out, good) {
					t.Errorf("expected %q not found in output:\n%s", good, out)
				}
			}
		})
	}
}
