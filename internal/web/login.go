package web

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/vinx-lab/vinx-assistant/internal/ilink"
)

// LoginStatus 是扫码流程的当前状态，页面每 2 秒轮询一次。
type LoginStatus struct {
	State   string `json:"state"` // idle waiting scanned need_verify confirmed failed
	Message string `json:"message"`
	HasQR   bool   `json:"has_qr"`
}

// loginManager 同一时间只允许一个扫码流程，状态放在内存里（重启后重新扫码即可）。
type loginManager struct {
	mu       sync.Mutex
	running  bool
	qr       ilink.QR
	status   LoginStatus
	verifyCh chan string
}

const loginTimeout = 8 * time.Minute

var loginStateText = map[string]string{
	"scaned":              "已扫码，请在手机上确认",
	"need_verifycode":     "请输入手机微信上显示的数字",
	"scaned_but_redirect": "已扫码，正在切换服务器",
	"confirmed":           "登录成功",
}

func (m *loginManager) get() LoginStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status
	if st.State == "" {
		st.State = "idle"
	}
	st.HasQR = m.qr.Content != "" && (st.State == "waiting" || st.State == "scanned" || st.State == "need_verify")
	return st
}

func (m *loginManager) set(state, msg string) {
	m.mu.Lock()
	m.status = LoginStatus{State: state, Message: msg}
	m.mu.Unlock()
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	d := struct {
		Page
		WeChat string
		Status LoginStatus
	}{Page: s.subPage(r, "微信登录", "login"), WeChat: s.d.Session.Status(r.Context()), Status: s.login.get()}
	s.render(w, http.StatusOK, "login", d)
}

func (s *Server) loginStart(w http.ResponseWriter, r *http.Request) {
	m := s.login
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		s.redirect(w, r, "/login")
		return
	}
	m.running = true
	m.mu.Unlock()

	started := false
	defer func() {
		if started {
			return
		}
		rec := recover()
		m.mu.Lock()
		m.running = false
		if rec != nil {
			m.qr = ilink.QR{}
			m.status = LoginStatus{State: "failed", Message: "获取二维码失败，请重试。"}
		}
		m.mu.Unlock()
		if rec != nil {
			s.d.Log.Error("扫码登录启动异常", "panic", rec)
			s.redirect(w, r, "/login")
		}
	}()
	l := s.d.NewLogin()
	qr, err := l.Start(r.Context(), s.d.Session.TokenHistory(r.Context()))
	if err != nil {
		s.d.Log.Warn("获取二维码失败", "err", err)
		m.mu.Lock()
		m.running = false
		m.qr = ilink.QR{}
		m.status = LoginStatus{State: "failed", Message: "获取二维码失败：" + err.Error()}
		m.mu.Unlock()
		started = true
		s.redirect(w, r, "/login")
		return
	}
	m.mu.Lock()
	m.qr, m.verifyCh = qr, make(chan string, 1)
	m.status = LoginStatus{State: "waiting", Message: "请用微信扫码"}
	verifyCh := m.verifyCh
	m.mu.Unlock()
	started = true

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
		defer cancel()
		verify := func(ctx context.Context) (string, error) {
			select { // 丢掉等待之前误提交的旧验证码
			case <-verifyCh:
			default:
			}
			m.set("need_verify", loginStateText["need_verifycode"])
			select {
			case code := <-verifyCh:
				m.set("scanned", "已提交验证码，请稍候")
				return code, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		onState := func(st string) {
			if st == "need_verifycode" {
				return // verify 里设置
			}
			if txt, ok := loginStateText[st]; ok {
				m.set("scanned", txt)
			}
		}
		cred, err := l.Wait(ctx, qr, verify, onState)
		if err == nil {
			cred.LoginAt = s.d.Clock.Now()
			err = s.d.Session.SaveCred(context.Background(), cred)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.running = false
		switch {
		case err == nil:
			m.status = LoginStatus{State: "confirmed", Message: "登录成功，正在运行的服务会在 30 秒内用上新凭证。"}
		case errors.Is(err, ilink.ErrAlreadyBound):
			m.status = LoginStatus{State: "failed", Message: "这个 Bot 已绑定在另一份凭证上。请在运行服务的机器上执行 vinx-assistant login --import <凭证文件>。"}
		case errors.Is(err, ilink.ErrQRExpired):
			m.status = LoginStatus{State: "failed", Message: "二维码已过期，请重新获取。"}
		case errors.Is(err, context.DeadlineExceeded):
			m.status = LoginStatus{State: "failed", Message: "等待扫码超时，请重新获取。"}
		default:
			m.status = LoginStatus{State: "failed", Message: "登录失败：" + err.Error()}
		}
		s.d.Log.Info("网页扫码登录结束", "state", m.status.State)
	}()
	s.redirect(w, r, "/login")
}

func (s *Server) loginVerify(w http.ResponseWriter, r *http.Request) {
	m := s.login
	m.mu.Lock()
	ch := m.verifyCh
	m.mu.Unlock()
	if ch != nil {
		select {
		case ch <- r.FormValue("code"):
		default:
		}
	}
	s.redirect(w, r, "/login")
}

func (s *Server) loginStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.login.get())
}

func (s *Server) loginQR(w http.ResponseWriter, r *http.Request) {
	st := s.login.get()
	s.login.mu.Lock()
	content := s.login.qr.Content
	s.login.mu.Unlock()
	if !st.HasQR {
		http.NotFound(w, r)
		return
	}
	png, err := qrcode.Encode(content, qrcode.Medium, 280)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}
