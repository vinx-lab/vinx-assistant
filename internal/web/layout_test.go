package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// 页面必须离线可用、手机上正常：不引用任何外部资源，有 viewport，没有内联脚本和样式（CSP 也不允许）。
func TestNoExternalResourcesAndMobileReady(t *testing.T) {
	external := regexp.MustCompile(`(?i)(src|href)\s*=\s*"(https?:)?//|url\(\s*['"]?(https?:)?//|@import|<script>|style="`)
	for _, root := range []fs.FS{templateFS, staticFS} {
		fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, _ := fs.ReadFile(root, p)
			if m := external.FindString(string(b)); m != "" {
				t.Errorf("%s 引用了外部资源或内联代码：%q", p, m)
			}
			return nil
		})
	}
	layout, _ := fs.ReadFile(templateFS, "templates/layout.html")
	if !strings.Contains(string(layout), `name="viewport" content="width=device-width, initial-scale=1"`) {
		t.Error("layout 缺少 viewport")
	}
	css, _ := fs.ReadFile(staticFS, "static/app.css")
	for _, want := range []string{"min-height: 44px", "overflow-wrap: anywhere", ".w100 { width: 100%; }"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("app.css 缺少 %q（触控尺寸、长链接换行、条形图宽度）", want)
		}
	}
}
