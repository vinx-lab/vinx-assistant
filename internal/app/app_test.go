package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

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
