package ingest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

const keyBuf = "ilink.buf"

// 重试策略对齐上游 monitor.ts：失败后等 2 秒重试，连续 3 次失败退避 30 秒；
// -14 时暂停 1 小时（由 session 记录，暂停期间 Client 返回 ErrPaused）。
const (
	maxConsecutiveFailures = 3
	maxBatchFailures       = 5
	defaultRetryDelay      = 2 * time.Second
	defaultBackoffDelay    = 30 * time.Second
	defaultIdleDelay       = 30 * time.Second
)

// Poller 长轮询收消息。没有凭证或暂停中时每隔 IdleDelay 检查一次。
type Poller struct {
	Session      *session.Session
	Service      *Service
	Store        *store.Store
	Log          *slog.Logger
	RetryDelay   time.Duration
	BackoffDelay time.Duration
	IdleDelay    time.Duration
}

func (p *Poller) Run(ctx context.Context) error {
	if p.Log == nil {
		p.Log = slog.Default()
	}
	if p.RetryDelay == 0 {
		p.RetryDelay = defaultRetryDelay
	}
	if p.BackoffDelay == 0 {
		p.BackoffDelay = defaultBackoffDelay
	}
	if p.IdleDelay == 0 {
		p.IdleDelay = defaultIdleDelay
	}
	var started *ilink.Client
	defer func() {
		if started != nil {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			started.NotifyStop(sctx)
			cancel()
		}
	}()
	failures := 0
	batchFailures := 0
	fail := func(msg string, err error) {
		failures++
		p.Log.Warn(msg, "err", err, "consecutive", failures)
		if failures >= maxConsecutiveFailures {
			failures = 0
			wait(ctx, p.BackoffDelay)
			return
		}
		wait(ctx, p.RetryDelay)
	}
	for ctx.Err() == nil {
		c, err := p.Session.Client(ctx)
		if err != nil {
			if !errors.Is(err, session.ErrNoCred) && !errors.Is(err, session.ErrPaused) {
				p.Log.Error("读取微信凭证失败", "err", err)
			}
			wait(ctx, p.IdleDelay)
			continue
		}
		if c != started {
			if err := c.NotifyStart(ctx); err != nil {
				p.Log.Warn("notifystart 失败（不影响收消息）", "err", err)
			}
			started = c
		}
		buf, _, err := p.Store.GetKV(ctx, keyBuf)
		if err != nil {
			fail("读取游标失败", err)
			continue
		}
		u, err := c.GetUpdates(ctx, buf)
		if errors.Is(err, ilink.ErrSessionExpired) {
			p.Log.Error("微信凭证失效（-14），暂停 1 小时后自动重试；反复出现请重新扫码")
			if err := p.Session.Pause(ctx); err != nil {
				p.Log.Error("记录暂停状态失败", "err", err)
				wait(ctx, p.BackoffDelay)
			}
			failures = 0
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				fail("getupdates 出错", err)
			}
			continue
		}
		failures = 0
		p.Session.MarkHealthy(ctx)
		if len(u.Undecodable) > 0 { // 不记录原文（含令牌）
			p.Log.Warn("有消息无法解析，已跳过（可能是上游协议变化，请运行 make check-upstream 对照）", "count", len(u.Undecodable))
		}
		var failedIDs []string
		for _, m := range u.Msgs {
			if err := p.Service.Handle(ctx, m); err != nil {
				p.Log.Error("处理消息失败", "msg_id", m.ID(), "err", err)
				failedIDs = append(failedIDs, m.ID())
			}
		}
		if len(failedIDs) > 0 {
			// 不推进游标，等上游重发；Seen/MarkSeen 保证重放安全。同一批连续失败太多次就跳过，免得毒消息卡死。
			batchFailures++
			if batchFailures < maxBatchFailures {
				fail("本批消息处理失败，游标不推进，稍后重试", errors.New("handle failed"))
				continue
			}
			p.Log.Error("多次处理失败，跳过这批消息", "msg_ids", failedIDs)
		}
		if len(u.Msgs) > 0 {
			batchFailures = 0
		}
		if u.Buf != "" && u.Buf != buf { // 只有非空才更新游标（与上游一致）
			if err := p.Store.SetKV(ctx, keyBuf, u.Buf); err != nil {
				p.Log.Error("保存游标失败", "err", err)
			}
		}
	}
	return ctx.Err()
}

func wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
