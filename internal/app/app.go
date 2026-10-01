// Package app 把各个包装配成 serve 进程：长轮询、回执、附件重试、定时 Tick、HTTP。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/batch"
	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/command"
	"github.com/vinx-lab/vinx-assistant/internal/enrich"
	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/ingest"
	"github.com/vinx-lab/vinx-assistant/internal/llm"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/notify"
	"github.com/vinx-lab/vinx-assistant/internal/remind"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
	"github.com/vinx-lab/vinx-assistant/internal/web"
)

type Config struct {
	Listen  string
	DataDir string
	// BasePath 是网页挂在反向代理子路径下的前缀（已规范化，如 "/todo"），空表示根。
	BasePath string
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
	Batch     *batch.Runner
	Remind    *remind.Service
	Commands  *command.Handler

	baseCtx context.Context // Run 期间的根 context；BatchNow 用它，服务退出时后台批次随之取消
	bg      sync.WaitGroup  // BatchNow 启动的后台批次；Close 前等它结束
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
	a := &App{Cfg: cfg, Store: st, Clock: clock.Real{}, Log: log, Mux: http.NewServeMux(), TickEvery: 30 * time.Second, baseCtx: context.Background()}
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
	a.Batch = &batch.Runner{Store: st, Clock: a.Clock, Fetcher: a.Fetcher, MediaDir: cfg.MediaDir(), Log: log}
	a.Tickers = append(a.Tickers, &batch.Scheduler{Runner: a.Batch, Store: st, Log: log})
	a.wireCommandsAndReminders(hc)
	web.Register(a.Mux, web.Deps{
		BatchNow: a.BatchNow, BatchRunning: a.Batch.Running, NextBatch: batch.NextSlot,
		Store: st, Session: a.Session, Clock: a.Clock, MediaDir: cfg.MediaDir(), Log: log,
		ListModels: func(ctx context.Context, p model.Provider) ([]string, error) {
			return llm.New(p.BaseURL, p.APIKey, hc).Models(ctx)
		},
		NewLogin: func() *ilink.Login { return ilink.NewLogin(hc) },
		// 用域名（如 Tailscale MagicDNS）访问网页时，把域名写进 VINX_ALLOWED_HOSTS（逗号分隔）。
		BasePath:     cfg.BasePath,
		AllowedHosts: web.ParseHosts(os.Getenv("VINX_ALLOWED_HOSTS")),
	})
	return a, nil
}

// BatchNow 在后台立即跑一次整理（网页「立即整理」按钮用）。已有整理在进行时返回 false。
func (a *App) BatchNow() bool {
	if a.Batch.Running() {
		return false
	}
	ctx := a.baseCtx
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		rep, err := a.Batch.Run(ctx)
		if err != nil {
			batch.LogRunError(a.Log, "手动整理", err)
			return
		}
		a.Log.Info("手动整理", "report", rep.String())
	}()
	return true
}

// Close 先等 BatchNow 的后台批次结束，再停掉指令后台和后处理队列（serve 退出时已停过，重复调用无害），最后关数据库。
func (a *App) Close() error {
	a.bg.Wait()
	a.Commands.Close()
	a.Ingest.Close()
	return a.Store.Close()
}

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

// CheckPrivateDir 在目录对组或其他用户可访问时返回警告文本，否则返回空串。
// 数据目录和备份里有微信凭证，不自动 chmod，只提示。
func CheckPrivateDir(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ""
	}
	// Windows 没有 Unix 权限位（总是报 0777），检查无意义
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("数据目录权限过宽（%04o），其中含微信凭证，请执行：chmod 700 %s", info.Mode().Perm(), path)
	}
	return ""
}

func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.Cfg.Listen)
	if err != nil {
		return err
	}
	return a.serve(ctx, ln)
}

// serve 在 ln 上提供 HTTP；无论正常退出还是 HTTP 出错，都先停掉全部后台协程再返回。
func (a *App) serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.baseCtx = ctx
	if w := CheckPrivateDir(a.Cfg.DataDir); w != "" {
		a.Log.Warn(w)
	}
	srv := &http.Server{Handler: a.Mux, ReadHeaderTimeout: 10 * time.Second}
	a.Log.Info("Vinx 助手已启动", "listen", ln.Addr().String(), "data", a.Cfg.DataDir, "wechat", a.Session.Status(ctx))

	var wg sync.WaitGroup
	run := func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }
	// 指令 AI 兜底的后台：续做上次没处理完的指令。
	a.Commands.Start(ctx)
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
	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
		cancel()
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	srv.Shutdown(sctx)
	wg.Wait()
	// 收件已停：停掉指令后台（进行中的 AI 调用取消，指令留在库里下次续做；已拿到结果的执行完、回复完）。
	// 它可能把消息补存为条目、往后处理队列里放任务，所以要在后处理队列之前停。
	a.Commands.Close()
	a.Commands.Wait(context.Background()) // Close 后立即返回
	// 停掉后处理队列（正在下的附件中断、积压的丢弃，附件仍是 pending，下次启动由重试补下）。
	a.Ingest.Close()
	a.Ingest.Wait()
	// 定时器触发的后台批次：ctx 已取消，正在进行的 AI 调用很快返回。
	for _, t := range a.Tickers {
		if w, ok := t.(interface{ Wait() }); ok {
			w.Wait()
		}
	}
	// 退出前把还没到点的合并回执发掉；ctx 已取消，另起一个 3 秒的。
	fctx, fcancel := context.WithTimeout(context.Background(), 3*time.Second)
	a.Ingest.FlushAllAcks(fctx)
	fcancel()
	a.Log.Info("Vinx 助手已退出")
	return serveErr
}
