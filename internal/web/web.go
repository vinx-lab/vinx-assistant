// Package web 是 Vinx 助手的网页：看板、详情、搜索、设置、用量、扫码登录。
// 服务端用 html/template 渲染，模板、样式和少量脚本用 embed 打包，不引用任何外部资源。
//
// 可选的网页密码见 signin.go（spec 0003）；没设密码时靠网络边界保护。所有 POST 都经过 http.CrossOriginProtection，
// 防止别的网页借浏览器偷偷提交表单（比如改服务商地址把 API 密钥发出去）。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/auth"
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

	// BasePath 是挂在反向代理子路径下时的前缀（如 "/todo"，规范化见 NormalizeBasePath）；空表示挂在根上。
	BasePath string
}

// NormalizeBasePath 规范化子路径前缀：空或 "/" 表示无前缀；否则以 "/" 开头、去掉结尾 "/"。
// 含 ? # 空白、反斜杠、控制字符、百分号或 ".." 的值视为非法。
func NormalizeBasePath(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "/" {
		return "", nil
	}
	if strings.ContainsAny(v, "?#% \\\t\r\n") || strings.Contains(v, "..") || strings.Contains(v, "//") {
		return "", fmt.Errorf("非法的 base-path %q", v)
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f {
			return "", fmt.Errorf("非法的 base-path %q", v)
		}
	}
	if !strings.HasPrefix(v, "/") {
		v = "/" + v
	}
	return strings.TrimRight(v, "/"), nil
}

type Server struct {
	d     Deps
	pages map[string]*template.Template
	login *loginManager
	hosts *hostGuard

	limiter    *auth.Limiter // 密码失败计数（登录和改密码共用）
	iter       int           // 新密码的 PBKDF2 迭代次数；测试里调小
	authMu     sync.Mutex
	lastRecord string                          // 上次读到的密码记录，变了就清空失败计数
	kdfSem     chan struct{}                   // PBKDF2 串行化，见 serialKDF
	verify     func(*auth.Record, string) bool // 校验密码；测试里换成计数的版本
	codes      *codeThrottle                   // 生成微信登录验证码的节流
}

var pageNames = []string{"board", "item", "login", "search", "settings", "usage"}

func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if bp, err := NormalizeBasePath(d.BasePath); err == nil {
		d.BasePath = bp
	} else {
		panic(err)
	}
	s := &Server{d: d, pages: map[string]*template.Template{}, login: &loginManager{}, hosts: newHostGuard(d.AllowedHosts),
		limiter: auth.NewLimiter(d.Clock), iter: auth.DefaultIterations, kdfSem: make(chan struct{}, 1),
		verify: func(rec *auth.Record, pw string) bool { return rec.Verify(pw) }, codes: &codeThrottle{clk: d.Clock}}
	for _, name := range pageNames {
		s.pages[name] = template.Must(template.New(name).Funcs(s.funcs()).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html"))
	}
	// 登录页不用公共布局：未登录时不显示顶栏里的微信状态、整理按钮
	s.pages["signin"] = template.Must(template.New("signin").Funcs(s.funcs()).ParseFS(templateFS, "templates/signin.html"))
	return s
}

// Register 把网页挂到 app 的 mux 上。
func Register(mux *http.ServeMux, d Deps) *Server {
	s := New(d)
	s.Routes(mux)
	return s
}

// Routes 注册所有路由。BasePath 非空时全部挂在前缀下：前缀本身 308 到带斜杠的地址，前缀之外的网页路径 404。
// 前缀在进入处理器之前剥掉，处理器里的 r.URL.Path 和内部地址一律不带前缀，输出地址时再由 base 加上。
func (s *Server) Routes(root *http.ServeMux) {
	mux := root
	if s.d.BasePath != "" {
		mux = http.NewServeMux()
		root.Handle(s.d.BasePath+"/", http.StripPrefix(s.d.BasePath, mux))
		root.HandleFunc(s.d.BasePath, func(w http.ResponseWriter, r *http.Request) {
			to := s.d.BasePath + "/"
			if r.URL.RawQuery != "" {
				to += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, to, http.StatusPermanentRedirect)
		})
	}
	cop := http.NewCrossOriginProtection()
	// 顺序：Host 白名单 → 安全头 → 跨站防护（POST）→ 登录检查 → 处理器
	get := func(pattern string, h http.HandlerFunc) { mux.Handle("GET "+pattern, s.secure(s.guard(guardPage, h))) }
	post := func(pattern string, h http.HandlerFunc) {
		mux.Handle("POST "+pattern, s.secure(cop.Handler(s.guard(guardForm, h))))
	}
	// 前端脚本 fetch 的接口和 <img> 加载的二维码：未登录时 401，不跳转
	getAPI := func(pattern string, h http.HandlerFunc) { mux.Handle("GET "+pattern, s.secure(s.guard(guardAPI, h))) }
	postAPI := func(pattern string, h http.HandlerFunc) {
		mux.Handle("POST "+pattern, s.secure(cop.Handler(s.guard(guardAPI, h))))
	}

	// 不需要登录：登录页、退出（只清当前 cookie 对应的会话）、静态文件；/healthz 在 app 的根 mux 上
	mux.Handle("GET /signin", s.secure(http.HandlerFunc(s.signinPage)))
	mux.Handle("POST /signin", s.secure(cop.Handler(http.HandlerFunc(s.signinPost))))
	mux.Handle("POST /signout", s.secure(cop.Handler(http.HandlerFunc(s.signout))))
	mux.Handle("GET /signin/code/status", s.secure(http.HandlerFunc(s.codeStatus)))
	mux.Handle("POST /signin/code", s.secure(cop.Handler(http.HandlerFunc(s.codePost))))

	get("/{$}", s.board)
	post("/batch/run", s.batchRun)
	post("/items/{id}/status", s.itemStatus)
	post("/items/{id}/deep", s.itemDeep)
	get("/items/{id}", s.itemPage)
	post("/items/{id}", s.itemSave)
	get("/search", s.search)
	get("/usage", s.usage)
	get("/settings", s.settingsPage)
	get("/settings/{section}", s.settingsPage)
	post("/settings/general", s.settingsGeneral)
	post("/settings/prompt", s.settingsPrompt)
	post("/settings/models", s.settingsModels)
	post("/settings/providers", s.providerSave)
	post("/settings/providers/{id}/delete", s.providerDelete)
	postAPI("/settings/providers/{id}/models", s.providerModels)
	post("/settings/password", s.passwordSave)
	post("/settings/password/signout-all", s.signoutAll)
	post("/settings/login", s.loginRequiredSave)
	get("/login", s.loginPage)
	post("/login/start", s.loginStart)
	post("/login/verify", s.loginVerify)
	getAPI("/login/status", s.loginStatus)
	getAPI("/login/qr.png", s.loginQR)
	get("/media/{path...}", s.media)
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", s.secure(http.StripPrefix("/static/", http.FileServerFS(sub))))
}

// secure 先检查 Host 头（防 DNS rebinding），再给所有响应加上安全头：脚本、样式只能来自本站，页面不能被嵌进别的网站。
// Referrer-Policy 必须是 same-origin 而不是 no-referrer：no-referrer 下浏览器提交表单会把 Origin 写成 null，http 局域网（非安全源）又不带 Sec-Fetch-Site，会被跨站防护拒绝。
func (s *Server) secure(h http.Handler) http.Handler {
	return s.hosts.handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	}), s)
}

// Page 是每个页面都有的数据。
type Page struct {
	Title string
	Nav   string // 主导航当前项：board search settings
	Sub   string // 设置小节导航当前项（见 settingSections）
	Up    string // 手机顶栏的返回地址（设置小节、条目详情）；空表示不显示返回
	Msg   string // 操作结果提示
	Error string // 校验错误
	Top   TopStatus

	NotRequired bool // 「需要登录」没打开：顶栏下方提示
	SignedIn    bool // 需要登录且已登录：显示「退出」
}

// TopStatus 是顶栏右侧的状态：微信连接、下次整理、立即整理。
type TopStatus struct {
	WeChat    string // ok / paused / no_cred
	NextBatch string
	Running   bool
	Back      string // 「立即整理」之后回到的地址
}

func (s *Server) topStatus(r *http.Request) TopStatus {
	t := TopStatus{Back: "/"}
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		q.Del("msg")
		t.Back = r.URL.Path
		if len(q) > 0 {
			t.Back += "?" + q.Encode()
		}
	}
	ctx := r.Context()
	if s.d.Session != nil {
		t.WeChat = s.d.Session.Status(ctx)
	}
	if s.d.BatchRunning != nil {
		t.Running = s.d.BatchRunning()
	}
	if s.d.NextBatch != nil && s.d.Store != nil {
		if st, err := s.d.Store.LoadSettings(ctx); err == nil {
			now := s.d.Clock.Now()
			if at, ok := s.d.NextBatch(now, st.Schedule.BatchTimes); ok {
				t.NextBatch = relTime(at, now)
			}
		}
	}
	return t
}

var messages = map[string]string{
	"started":   "已开始整理，稍后刷新查看结果。",
	"busy":      "正在整理中，请稍后再试。",
	"saved":     "已保存。",
	"deep":      "已标记为深入研究，下次整理时处理。",
	"deepnow":   "已标记为深入研究，并开始整理。",
	"status":    "状态已更新。",
	"deleted":   "已删除。",
	"pwset":     "密码已设置。",
	"pwexists":  "已经有人设置过密码，没有覆盖。请用当前密码修改。",
	"loginon":   "已开启登录，当前浏览器保持登录。",
	"loginoff":  "已关闭登录要求。",
	"pwchanged": "密码已修改，其他设备的登录已全部失效。",
}

func (s *Server) page(r *http.Request, title, nav string) Page {
	a := authFrom(r.Context())
	return Page{Title: title, Nav: nav, Msg: messages[r.URL.Query().Get("msg")], Top: s.topStatus(r), NotRequired: !a.Required, SignedIn: a.Required && a.SignedIn}
}

// subPage 是挂在设置小节导航下的页面（微信登录、用量）。
func (s *Server) subPage(r *http.Request, title, sub string) Page {
	p := s.page(r, title, "settings")
	p.Sub, p.Up = sub, "/settings"
	return p
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

// safeBack 只接受站内相对路径，防止表单里的 back、登录页的 next 被利用来跳到外站。
// 控制字符一律拒绝：浏览器解析地址时会删掉制表符和换行，"/\t/evil.com" 会变成 "//evil.com"。
func safeBack(v string) string {
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.ContainsRune(v, '\\') {
		return "/"
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	return v
}

func withMsg(path, msg string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "msg=" + msg
}

// redirect 303 到站内地址 to（不带前缀），Location 里加上 BasePath。
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, s.d.BasePath+to, http.StatusSeeOther)
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"base":       func() string { return s.d.BasePath },
		"display":    func(it model.Item) string { return it.DisplayTitle() },
		"catName":    model.CategoryName,
		"statusName": model.StatusName,
		"prioName":   func(p model.Priority) string { return priorityNames[p] },
		"due": func(it model.Item) string {
			if it.DueAt == nil {
				return ""
			}
			return model.FormatDue(*it.DueAt, it.DueHasTime, s.d.Clock.Now())
		},
		"overdue": func(it model.Item) bool { return isOverdue(it, s.d.Clock.Now()) },
		"statusChoices": func(it model.Item) []string {
			var out []string
			for _, st := range statusOrder {
				if st != it.Status && model.ValidStatus(it.Category, st) {
					out = append(out, st)
				}
			}
			return out
		},
		// menuChoices 是「⋯」菜单里的状态：其余可选状态，已有一键完成圆圈时去掉「完成」
		"menuChoices": func(it model.Item) []string {
			var out []string
			for _, st := range statusOrder {
				if st == it.Status || !model.ValidStatus(it.Category, st) {
					continue
				}
				if st == model.StatusDone && it.Category == model.CatTodo && it.Status == model.StatusOpen {
					continue
				}
				out = append(out, st)
			}
			return out
		},
		"md":            renderMarkdown,
		"settingGroups": func() []settingGroup { return settingGroups },
		"catIcon":       func(c model.Category) string { return categoryIcons[c] },
		// canCheck：待办还没办完时，列表左侧显示「一键完成」圆圈
		"canCheck": func(it model.Item) bool { return it.Category == model.CatTodo && it.Status == model.StatusOpen },
		"secret":   redact.Secret,
		"fmtTime":  func(t time.Time) string { return t.In(clock.Zone).Format("2006-01-02 15:04") },
		"join":     strings.Join,
		"pct": func(v, max int64) int64 {
			if max <= 0 {
				return 0
			}
			return v * 100 / max
		},
	}
}
