// Package web 是 Vinx 助手的网页：看板、详情、搜索、设置、用量、扫码登录。
// 服务端用 html/template 渲染，模板、样式和少量脚本用 embed 打包，不引用任何外部资源。
//
// 目前只有骨架和扫码登录（从计划 4 提前实现）；其余页面由计划 4 在此基础上补齐。
//
// 第一期没有登录密码（spec 已接受，靠网络边界保护）。所有 POST 都经过 http.CrossOriginProtection，
// 防止别的网页借浏览器偷偷提交表单（比如改服务商地址把 API 密钥发出去）。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/redact"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Deps 是网页依赖的东西。函数字段由 app 装配，测试里换成假的。
type Deps struct {
	Store    *store.Store
	Session  *session.Session
	Clock    clock.Clock
	MediaDir string
	Log      *slog.Logger

	BatchNow     func() bool // 后台立即整理；已在整理时返回 false
	BatchRunning func() bool
	NextBatch    func(now time.Time, times []string) (time.Time, bool) // batch.NextSlot
	ListModels   func(ctx context.Context, p model.Provider) ([]string, error)
	NewLogin     func() *ilink.Login

	// AllowedHosts 是除 localhost、回环地址、本机网卡 IP 之外允许的 Host（如 Tailscale 域名），见 hosts.go。
	AllowedHosts []string
}

type Server struct {
	d     Deps
	pages map[string]*template.Template
	login *loginManager
	hosts *hostGuard
}

var pageNames = []string{"login", "settings"}

func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	s := &Server{d: d, pages: map[string]*template.Template{}, login: &loginManager{}, hosts: newHostGuard(d.AllowedHosts)}
	for _, name := range pageNames {
		s.pages[name] = template.Must(template.New(name).Funcs(s.funcs()).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html"))
	}
	return s
}

// Register 把网页挂到 app 的 mux 上。
func Register(mux *http.ServeMux, d Deps) *Server {
	s := New(d)
	s.Routes(mux)
	return s
}

func (s *Server) Routes(mux *http.ServeMux) {
	cop := http.NewCrossOriginProtection()
	get := func(pattern string, h http.HandlerFunc) { mux.Handle("GET "+pattern, s.secure(h)) }
	post := func(pattern string, h http.HandlerFunc) { mux.Handle("POST "+pattern, s.secure(cop.Handler(h))) }

	// 计划 4 Task 3 把这里换成看板。
	get("/{$}", func(w http.ResponseWriter, r *http.Request) { redirect(w, r, "/login") })
	get("/settings", s.settingsPage)
	post("/settings/general", s.settingsGeneral)
	post("/settings/models", s.settingsModels)
	post("/settings/providers", s.providerSave)
	post("/settings/providers/{id}/delete", s.providerDelete)
	post("/settings/providers/{id}/models", s.providerModels)
	get("/login", s.loginPage)
	post("/login/start", s.loginStart)
	post("/login/verify", s.loginVerify)
	get("/login/status", s.loginStatus)
	get("/login/qr.png", s.loginQR)
	get("/media/{path...}", s.media)
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", s.secure(http.StripPrefix("/static/", http.FileServerFS(sub))))
}

// secure 先检查 Host 头（防 DNS rebinding），再给所有响应加上安全头：脚本、样式只能来自本站，页面不能被嵌进别的网站。
func (s *Server) secure(h http.Handler) http.Handler {
	return s.hosts.handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	}), s)
}

// Page 是每个页面都有的数据。
type Page struct {
	Title string
	Nav   string
	Msg   string // 操作结果提示
	Error string // 校验错误
}

var messages = map[string]string{
	"started": "已开始整理，稍后刷新查看结果。",
	"busy":    "正在整理中，请稍后再试。",
	"saved":   "已保存。",
	"deep":    "已标记为深入研究，下次整理时处理。",
	"deepnow": "已标记为深入研究，并开始整理。",
	"status":  "状态已更新。",
	"deleted": "已删除。",
}

func (s *Server) page(r *http.Request, title, nav string) Page {
	return Page{Title: title, Nav: nav, Msg: messages[r.URL.Query().Get("msg")]}
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var b strings.Builder
	if err := s.pages[name].ExecuteTemplate(&b, "layout", data); err != nil {
		s.d.Log.Error("渲染页面失败", "page", name, "err", err)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(b.String()))
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.d.Log.Error("网页请求失败", "err", err)
	http.Error(w, "内部错误，详见服务日志", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// safeBack 只接受站内相对路径，防止表单里的 back 被利用来跳到外站。
func safeBack(v string) string {
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") && !strings.ContainsAny(v, "\\\r\n") {
		return v
	}
	return "/"
}

func withMsg(path, msg string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "msg=" + msg
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// funcs 只含不依赖计划 2/3 的模板函数；计划 4 补齐 display、statusName、due 等。
func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"secret":  redact.Secret,
		"fmtTime": func(t time.Time) string { return t.In(clock.Zone).Format("2006-01-02 15:04") },
		"join":    strings.Join,
	}
}
