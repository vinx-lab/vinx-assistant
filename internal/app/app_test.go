package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/auth"
	"github.com/vinx-lab/vinx-assistant/internal/ingest"
)

// 设了网页密码后 /healthz 仍不需要登录，网页要登录（含带前缀的情况）。
func TestHealthzWithPassword(t *testing.T) {
	for _, base := range []string{"", "/todo"} {
		a, err := New(Config{Listen: "127.0.0.1:0", DataDir: t.TempDir(), BasePath: base}, nil)
		if err != nil {
			t.Fatal(err)
		}
		rec, _ := auth.Hash("password123", 1000)
		raw, _ := rec.Encode()
		if err := a.Store.SetPassword(context.Background(), raw, ""); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(a.Mux)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		for p, want := range map[string]int{"/healthz": 200, base + "/": http.StatusSeeOther, base + "/signin": 200, base + "/static/app.css": 200} {
			resp, err := client.Get(srv.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != want {
				t.Errorf("base=%q %s: %d", base, p, resp.StatusCode)
			}
		}
		srv.Close()
		a.Close()
	}
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

type countTicker struct{ n chan time.Time }

func (c *countTicker) Tick(ctx context.Context, now time.Time) {
	select {
	case c.n <- now:
	default:
	}
}

func TestServeHealthzAndTicks(t *testing.T) {
	cfg := Config{Listen: freeAddr(t), DataDir: t.TempDir()}
	a, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	tk := &countTicker{n: make(chan time.Time, 1)}
	a.Tickers = append(a.Tickers, tk)
	a.TickEvery = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	var resp *http.Response
	for i := 0; i < 50; i++ {
		resp, err = http.Get("http://" + cfg.Listen + "/healthz")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var h map[string]string
	if json.Unmarshal(body, &h); h["status"] != "ok" || h["wechat"] != "no_cred" {
		t.Fatalf("healthz = %s", body)
	}
	lr, err := http.Get("http://" + cfg.Listen + "/login")
	if err != nil {
		t.Fatal(err)
	}
	lr.Body.Close()
	if lr.StatusCode != 200 {
		t.Fatalf("/login = %d", lr.StatusCode)
	}
	select {
	case <-tk.n:
	case <-time.After(2 * time.Second):
		t.Fatal("ticker not called")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestServeErrorStopsEverything(t *testing.T) {
	a, err := New(Config{DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // Serve 会立刻返回非 ErrServerClosed 的错误
	done := make(chan error, 1)
	go func() { done <- a.serve(context.Background(), ln) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want serve error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop background goroutines and return")
	}
}

func TestCheckPrivateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		if CheckPrivateDir(t.TempDir()) != "" {
			t.Fatal("windows should never warn")
		}
		return
	}
	d := t.TempDir()
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if CheckPrivateDir(d) == "" {
		t.Fatal("0755 should warn")
	}
	os.Chmod(d, 0o700)
	if w := CheckPrivateDir(d); w != "" {
		t.Fatalf("0700 warned: %s", w)
	}
	if CheckPrivateDir(d+"/nope") != "" {
		t.Fatal("missing dir should not warn")
	}
}

func TestBatchNow(t *testing.T) {
	a, err := New(Config{Listen: freeAddr(t), DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !a.BatchNow() {
		t.Fatal("BatchNow must start when idle")
	}
	// defer a.Close() 会等后台批次结束再关数据库；用 -race 跑能发现两者的竞争。
}

func TestBatchNowWiredIntoWebDeps(t *testing.T) {
	a, err := New(Config{Listen: freeAddr(t), DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Batch == nil || len(a.Tickers) == 0 {
		t.Fatal("batch runner and scheduler must be wired")
	}
}

func TestNewWiresCommandsAndReminders(t *testing.T) {
	a, err := New(Config{Listen: "127.0.0.1:0", DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Commands == nil || a.Remind == nil {
		t.Fatal("commands/remind not built")
	}
	if a.Ingest.Commands != ingest.CommandHandler(a.Commands) || a.Ingest.Deferred != ingest.Deferred(a.Remind) {
		t.Fatal("ingest not wired")
	}
	if a.Remind.Session == nil || a.Remind.Notifier == nil || a.Remind.Clock == nil {
		t.Fatalf("remind deps missing: %+v", a.Remind)
	}
	if a.Commands.Reply == nil || a.Commands.SaveAsItem == nil || a.Commands.Translator == nil {
		t.Fatal("command handler deps missing")
	}
	found := false
	for _, tk := range a.Tickers {
		if tk == Ticker(a.Remind) {
			found = true
		}
	}
	if !found {
		t.Fatal("remind ticker missing")
	}
	tr, err := commandTranslator(a.Store, nil, a.Clock)(context.Background())
	if err != nil || tr != nil {
		t.Fatalf("unconfigured AI must give nil translator: %v %v", tr, err)
	}
}

func TestWebMountedAndBatchRun(t *testing.T) {
	a, err := New(Config{Listen: "127.0.0.1:0", DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	srv := httptest.NewServer(a.Mux)
	defer srv.Close()
	for _, p := range []string{"/", "/settings", "/search", "/usage", "/login", "/static/app.css", "/healthz"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/batch/run", strings.NewReader(url.Values{"back": {"/"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("batch/run: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}
