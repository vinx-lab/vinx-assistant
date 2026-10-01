package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type env struct {
	st    *store.Store
	clk   *clock.Fake
	sess  *session.Session
	ilink *ilinktest.Server
	srv   *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{}
	dir := t.TempDir()
	var err error
	if e.st, err = store.Open(filepath.Join(dir, "t.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.st.Close() })
	e.clk = clock.NewFake(clock.At(2026, 10, 1, 9, 0))
	e.st.SetClock(e.clk)
	e.sess = session.New(e.st, nil, e.clk)
	e.ilink = ilinktest.New()
	t.Cleanup(e.ilink.Close)
	d := Deps{
		Store: e.st, Session: e.sess, Clock: e.clk, MediaDir: filepath.Join(dir, "media"),
		NewLogin: func() *ilink.Login {
			l := ilink.NewLogin(nil)
			l.BaseURL, l.PollDelay = e.ilink.URL, time.Millisecond
			return l
		},
	}
	mux := http.NewServeMux()
	New(d).Routes(mux)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// client 不跟随跳转，方便检查 303。
func (e *env) client() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := e.client().Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// post 模拟同源的浏览器表单提交。
func (e *env) post(t *testing.T, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	return e.postFrom(t, path, form, "same-origin")
}

func (e *env) postFrom(t *testing.T, path string, form url.Values, site string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", site)
	resp, err := e.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func mustContain(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("body missing %q", w)
		}
	}
}

func mustNotContain(t *testing.T, body string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(body, b) {
			t.Errorf("body must not contain %q", b)
		}
	}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in 3s")
}

func TestSafeBack(t *testing.T) {
	cases := map[string]string{"/?cat=todo": "/?cat=todo", "//evil.example": "/", "https://evil.example": "/", "/a\\b": "/", "": "/"}
	for in, want := range cases {
		if got := safeBack(in); got != want {
			t.Errorf("safeBack(%q) = %q", in, got)
		}
	}
}

func TestRootRedirectsToLogin(t *testing.T) {
	e := newEnv(t)
	resp, err := e.client().Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestCrossSitePostRejected(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/login/start", "/login/verify"} {
		resp, _ := e.postFrom(t, p, url.Values{"code": {"1"}}, "cross-site")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s cross-site: %d", p, resp.StatusCode)
		}
	}
	// 被拒绝后没有开始任何流程
	_, body := e.get(t, "/login/status")
	mustContain(t, body, `"state":"idle"`)
}

func TestLoginPageSafeAndSelfContained(t *testing.T) {
	e := newEnv(t)
	e.sess.SaveCred(t.Context(), e.ilink.Cred())
	code, body := e.get(t, "/login")
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	mustNotContain(t, body, "bot_token", ilinktest.Token, "http://", "https://")
}

func TestStaticServed(t *testing.T) {
	e := newEnv(t)
	code, body := e.get(t, "/static/app.css")
	if code != 200 || !strings.Contains(body, "--accent") {
		t.Fatalf("css %d", code)
	}
	if code, _ := e.get(t, "/static/app.js"); code != 200 {
		t.Fatalf("js %d", code)
	}
}
