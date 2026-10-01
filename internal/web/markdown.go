package web

import (
	"html/template"

	"github.com/russross/blackfriday/v2"
)

var mdRenderer = blackfriday.NewHTMLRenderer(blackfriday.HTMLRendererParameters{
	// AI 写的 detail 来自外部网页内容，不可信：丢掉原始 HTML，只允许安全协议的链接，外链新窗口打开。
	Flags: blackfriday.SkipHTML | blackfriday.Safelink | blackfriday.NofollowLinks |
		blackfriday.NoreferrerLinks | blackfriday.HrefTargetBlank,
})

// renderMarkdown 把 detail 渲染成 HTML。
func renderMarkdown(s string) template.HTML {
	out := blackfriday.Run([]byte(s), blackfriday.WithRenderer(mdRenderer), blackfriday.WithExtensions(blackfriday.CommonExtensions))
	return template.HTML(out) // 已由 SkipHTML、Safelink 处理
}
