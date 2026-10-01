package web

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/url"
	"strings"

	"github.com/russross/blackfriday/v2"
)

// safeImageRenderer 包装 HTMLRenderer，把不安全的图片链接改成文本或安全链接。
type safeImageRenderer struct {
	*blackfriday.HTMLRenderer
}

// RenderNode 拦截 Image 节点进行安全处理。
func (r *safeImageRenderer) RenderNode(w io.Writer, node *blackfriday.Node, entering bool) blackfriday.WalkStatus {
	if node.Type == blackfriday.Image && entering {
		return r.renderSafeImage(w, node)
	}
	return r.HTMLRenderer.RenderNode(w, node, entering)
}

func (r *safeImageRenderer) renderSafeImage(w io.Writer, node *blackfriday.Node) blackfriday.WalkStatus {
	// 提取图片 URL 和 alt 文字
	dest := string(node.Destination)
	var altText string
	if node.FirstChild != nil {
		// alt 文字是图片节点的第一个子节点
		var buf bytes.Buffer
		r.HTMLRenderer.RenderNode(&buf, node.FirstChild, true)
		altText = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(buf.String(), "</p>\n"), "<p>"))
		if altText == "" {
			altText = html.UnescapeString(string(node.FirstChild.Literal))
		}
	}
	if altText == "" {
		altText = "图片"
	}

	// 检查 URL 协议是否安全
	u, err := url.Parse(dest)
	if err != nil || u.Scheme == "" {
		// 无法解析或相对路径：只显示 alt 文字
		fmt.Fprint(w, html.EscapeString(altText))
		return blackfriday.SkipChildren
	}

	// 只允许 http/https
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		// 不安全的协议（javascript, data, vbscript等）：只显示 alt 文字
		fmt.Fprint(w, html.EscapeString(altText))
		return blackfriday.SkipChildren
	}

	// 安全的 HTTPS 图片：转为链接
	fmt.Fprintf(w, `<a href="%s" rel="nofollow noreferrer" target="_blank">%s</a>`,
		html.EscapeString(dest), html.EscapeString(altText))
	return blackfriday.SkipChildren
}

var mdRenderer *safeImageRenderer

func init() {
	// 创建带有自定义图片处理的渲染器
	htmlRenderer := blackfriday.NewHTMLRenderer(blackfriday.HTMLRendererParameters{
		// AI 写的 detail 来自外部网页内容，不可信：丢掉原始 HTML，只允许安全协议的链接，外链和图片新窗口打开。
		Flags: blackfriday.SkipHTML | blackfriday.Safelink | blackfriday.NofollowLinks |
			blackfriday.NoreferrerLinks | blackfriday.HrefTargetBlank,
	})
	mdRenderer = &safeImageRenderer{htmlRenderer}
}

// renderMarkdown 把 detail 渲染成 HTML。图片不内联加载，只渲染为安全链接。
func renderMarkdown(s string) template.HTML {
	out := blackfriday.Run([]byte(s), blackfriday.WithRenderer(mdRenderer), blackfriday.WithExtensions(blackfriday.CommonExtensions))
	return template.HTML(out) // 已由 SkipHTML、Safelink 和自定义 safeImageRenderer 处理
}
