// Package app 把各个包装配成 serve 进程：长轮询、回执、附件重试、定时 Tick、HTTP。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/enrich"
	"github.com/vinx-lab/vinx-assistant/internal/ingest"
	"github.com/vinx-lab/vinx-assistant/internal/notify"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type Config struct {
	Listen  string
	DataDir string
}

func (c Config) DBPath() string   { return filepath.Join(c.DataDir, "vinx-assistant.db") }
func (c Config) MediaDir() string { return filepath.Join(c.DataDir, "media") }

// Ticker 由定时循环调用（默认每 30 秒），实现方自己判断是否到点。
type Ticker interface {
	Tick(ctx context.Context, now time.Time)
}

type App struct {
	Cfg       Config
	Store     *store.Store
	Clock     clock.Clock
	Session   *session.Session
	Notifier  notify.Notifier
	Fetcher   *enrich.Fetcher
	Ingest    *ingest.Service
	Mux       *http.ServeMux
	Tickers   []Ticker
	Log       *slog.Logger
	TickEvery time.Duration
}

func New(cfg Config, log *slog.Logger) (*App, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	if err := os.MkdirAll(cfg.MediaDir(), 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	hc := &http.Client{} // 默认 Transport 读 HTTPS_PROXY
	a := &App{Cfg: cfg, Store: st, Clock: clock.Real{}, Log: log, Mux: http.NewServeMux(), TickEvery: 30 * time.Second}
	a.Session = session.New(st, hc, a.Clock)
	a.Notifier = notify.NewWeChat(a.Session, st)
	a.Fetcher = enrich.NewFetcher(hc)
	a.Ingest = ingest.New(ingest.Deps{
		Store: st, Session: a.Session, Notifier: a.Notifier, Clock: a.Clock,
		MediaDir: cfg.MediaDir(), Log: log,
		Enricher: &enrich.Linker{Fetcher: a.Fetcher, Store: st, Log: log},
	})
	a.Mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "wechat": a.Session.Status(r.Context())})
	})
	// 计划 2–4 在这里追加装配。
	return a, nil
}

func (a *App) Close() error { return a.Store.Close() }

func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.Cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: a.Mux, ReadHeaderTimeout: 10 * time.Second}
	a.Log.Info("Vinx 助手已启动", "listen", ln.Addr().String(), "data", a.Cfg.DataDir, "wechat", a.Session.Status(ctx))

	var wg sync.WaitGroup
	run := func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }
	poller := &ingest.Poller{Session: a.Session, Service: a.Ingest, Store: a.Store, Log: a.Log}
	run(func() { poller.Run(ctx) })
	run(func() { every(ctx, time.Second, func() { a.Ingest.FlushAcks(ctx) }) })
	run(func() {
		every(ctx, 5*time.Minute, func() {
			a.Ingest.RetryAttachments(ctx)
			a.Store.PruneSent(ctx, a.Clock.Now().Add(-30*24*time.Hour)) // 与上游引用缓存默认保留 30 天一致
		})
	})
	run(func() {
		every(ctx, a.TickEvery, func() {
			now := a.Clock.Now()
			for _, t := range a.Tickers {
				t.Tick(ctx, now)
			}
		})
	})

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	wg.Wait()
	a.Ingest.Wait()
	a.Log.Info("Vinx 助手已退出")
	return nil
}
