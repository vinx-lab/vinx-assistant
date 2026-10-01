package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/ingest"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// Thinking 是 AI 指令处理较慢时的进度提示：只对以操作词开头的指令、且超过 ThinkingAfter 仍没出结果时发一次。
// 不在收到时立即回，避免短时间连发两条触发微信限流。
const Thinking = "收到，正在理解…"

const (
	defaultJobTimeout = 2 * time.Minute
	thinkingAfter     = 8 * time.Second
	staleAfter        = time.Hour          // 超过这么久还没处理的指令不再执行（回复的 token 多半已过期，结果也发不出）
	keepDone          = 7 * 24 * time.Hour // 已处理的 AI 指令记录保留多久（用于重放查重）
	pendingBatch      = 50
	replyTimeout      = 30 * time.Second
)

// worker 是 Handler 自带的后台：单个 goroutine 串行处理 ai_commands 里未完成的指令。
// 唤醒通道容量为 1、非阻塞发送，所以 Handle 永远不会因后台忙而阻塞；待处理的指令本身在数据库里，不会丢。
type worker struct {
	initOnce  sync.Once
	startOnce sync.Once
	wake      chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}

	mu      sync.Mutex
	cond    *sync.Cond
	dirty   bool // 上一轮扫描之后又有新指令入队
	idle    bool // 已扫描完、没有待处理的指令
	stopped bool
}

func (w *worker) init() {
	w.initOnce.Do(func() {
		w.wake = make(chan struct{}, 1)
		w.cond = sync.NewCond(&w.mu)
	})
}

func (w *worker) notify() {
	w.init()
	w.mu.Lock()
	w.dirty = true
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Start 启动后台并立即续做上次没处理完的指令。只有第一次调用生效；Close 之后再调用不做任何事。
func (h *Handler) Start(ctx context.Context) {
	w := &h.w
	w.init()
	w.startOnce.Do(func() {
		ctx, w.cancel = context.WithCancel(ctx)
		w.done = make(chan struct{})
		if err := h.Store.PruneAICommands(ctx, h.Clock.Now().Add(-keepDone)); err != nil {
			h.log().Warn("清理已处理的 AI 指令失败", "err", err)
		}
		go h.loop(ctx)
	})
}

// Close 停止后台：进行中的 AI 调用被取消，该指令留在待处理表里，下次 Start 时续做。
// 已拿到翻译结果的指令会执行完再退出。
func (h *Handler) Close() {
	w := &h.w
	w.init()
	w.startOnce.Do(func() {}) // 没 Start 过就占掉，之后的 Start 不再启动；Start 进行中则等它完成
	if w.cancel != nil {
		w.cancel()
		<-w.done
	}
	w.mu.Lock()
	w.stopped = true
	w.cond.Broadcast()
	w.mu.Unlock()
}

// Wait 等到后台空闲（没有待处理的指令）或后台已停止。只在 Start 之后调用有意义。
func (h *Handler) Wait(ctx context.Context) error {
	w := &h.w
	w.init()
	ok := make(chan struct{})
	go func() {
		w.mu.Lock()
		for !(w.idle && !w.dirty) && !w.stopped {
			w.cond.Wait()
		}
		w.mu.Unlock()
		close(ok)
	}()
	select {
	case <-ok:
		return nil
	case <-ctx.Done():
		return ctx.Err() // 上面的 goroutine 在下次空闲或 Close 时退出
	}
}

func (h *Handler) loop(ctx context.Context) {
	w := &h.w
	defer close(w.done)
	for {
		w.mu.Lock()
		w.dirty, w.idle = false, false
		w.mu.Unlock()
		h.drain(ctx)
		w.mu.Lock()
		if !w.dirty {
			w.idle = true
			w.cond.Broadcast()
		}
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		}
	}
}

// drain 处理完所有待处理的指令。返回时要么都处理完，要么 ctx 已取消，要么遇到数据库故障（等下次唤醒再试）。
func (h *Handler) drain(ctx context.Context) {
	for ctx.Err() == nil {
		cmds, err := h.Store.PendingAICommands(ctx, pendingBatch)
		if err != nil {
			if ctx.Err() == nil {
				h.log().Error("读取待处理 AI 指令失败", "err", err)
			}
			return
		}
		if len(cmds) == 0 {
			return
		}
		for _, c := range cmds {
			if !h.process(ctx, c) {
				return
			}
		}
	}
}

// process 处理一条指令，返回 false 表示应停止本轮（关停或数据库故障）。
func (h *Handler) process(ctx context.Context, c store.AICommand) bool {
	if ctx.Err() != nil { // 关停中：同一批里剩下的指令留在待处理表，下次 Start 续做
		return false
	}
	wctx := context.WithoutCancel(ctx)
	if age := h.Clock.Now().Sub(c.CreatedAt); age > staleAfter {
		return h.expire(wctx, c, age)
	}
	timeout := h.JobTimeout
	if timeout <= 0 {
		timeout = defaultJobTimeout
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stopThinking := h.thinkingTimer(wctx, c)
	reply, err := h.translateAndRun(jctx, c)
	stopThinking()
	if errors.Is(err, errShutdown) {
		return false
	}
	// 拿到结果之后不受关停影响：回复、标完成都要做完
	if err != nil {
		h.log().Error("AI 指令执行失败", "msg_id", c.MsgID, "err", err)
		reply = "指令执行失败，请稍后再试"
	}
	// 先回复再标完成：两步之间崩溃时重启会再处理一次，改条目的部分按 msg_id 去重，只会多回一条「已处理过」
	if reply != "" {
		h.sendReply(wctx, reply)
	}
	if err := h.Store.FinishAICommand(wctx, c.MsgID, reply); err != nil {
		h.log().Error("标记 AI 指令完成失败", "msg_id", c.MsgID, "err", err)
		return false
	}
	return true
}

func (h *Handler) sendReply(ctx context.Context, text string) {
	if h.Reply == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, replyTimeout) // 发送卡住时不让 Close 一直等
	defer cancel()
	h.Reply(rctx, text)
}

// thinkingTimer 对以操作词开头的指令计时：超过 ThinkingAfter 还没出结果就回一次 Thinking。
// 返回的 stop 在结果出来后调用：取消计时；提示正在发送时等它发完，保证结果排在提示之后。
func (h *Handler) thinkingTimer(ctx context.Context, c store.AICommand) (stop func()) {
	if !c.HasWord || h.Reply == nil {
		return func() {}
	}
	d := h.ThinkingAfter
	if d <= 0 {
		d = thinkingAfter
	}
	after := h.afterFunc
	if after == nil {
		after = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	var mu sync.Mutex
	finished := false
	cancelTimer := after(d, func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			h.sendReply(ctx, Thinking)
		}
	})
	return func() {
		cancelTimer()
		mu.Lock()
		finished = true
		mu.Unlock()
	}
}

// expire 作废积压太久的指令：不再执行、不回复，只记 WARN。不带操作词的消息原本可能是普通收件，照常存为条目，不丢内容。
func (h *Handler) expire(ctx context.Context, c store.AICommand, age time.Duration) bool {
	h.log().Warn("AI 指令积压过久，作废不执行", "msg_id", c.MsgID, "age", age.Round(time.Second).String(), "text", model.TruncateRunes(c.Text, 40))
	if !c.HasWord && h.SaveAsItem != nil {
		if err := h.saveAsItem(ctx, c); err != nil {
			h.log().Error("积压的消息存为条目失败", "msg_id", c.MsgID, "err", err)
		}
	}
	if err := h.Store.FinishAICommand(ctx, c.MsgID, ""); err != nil {
		h.log().Error("标记 AI 指令作废失败", "msg_id", c.MsgID, "err", err)
		return false
	}
	return true
}

var errShutdown = errors.New("command: 关停中")

// translateAndRun 翻译并执行，返回要回给用户的文字（空表示不回复）。
func (h *Handler) translateAndRun(ctx context.Context, c store.AICommand) (string, error) {
	wctx := context.WithoutCancel(ctx)
	// 崩溃恢复：这条指令已经改过条目（同一事务记了 msg_id），只回显
	if prev, err := h.Store.ActionByMsgID(wctx, c.MsgID); err == nil {
		return h.replayReply(wctx, prev), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	notUnderstood := func(why string) (string, error) {
		if c.HasWord {
			return why + Usage, nil
		}
		return "", h.saveAsItem(wctx, c)
	}
	tr := h.translator(ctx)
	if tr == nil {
		if ctx.Err() != nil { // 读设置因关停失败，不是「没配 AI」：留待下次续做
			return "", errShutdown
		}
		return notUnderstood("没看懂这条指令。")
	}
	todos, err := h.Store.OpenTodos(wctx, 50)
	if err != nil {
		return "", err
	}
	text := c.Text
	if c.RefText != "" {
		text += "\n（引用的消息：" + model.TruncateRunes(c.RefText, 300) + "）"
	}
	tl, err := tr.Translate(ctx, text, todos, c.CreatedAt)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) { // 关停（超时是 DeadlineExceeded，按 AI 不可用处理）
			return "", errShutdown
		}
		h.log().Warn("AI 翻译指令失败", "msg_id", c.MsgID, "err", err)
		return notUnderstood("没看懂这条指令（AI 暂时不可用）。")
	}
	now := h.Clock.Now()
	cmd, reply, ok := fromTranslation(tl, todos, now)
	if !ok { // AI 认为这不是指令：当普通条目保存
		if h.SaveAsItem == nil {
			return "没看懂这条指令。" + Usage, nil
		}
		return "", h.saveAsItem(wctx, c)
	}
	if reply != "" {
		return reply, nil
	}
	reply, _, err = h.run(wctx, cmd, ingest.CommandInput{Text: c.Text, RefText: c.RefText, HasRef: c.RefText != "", MsgID: c.MsgID}, now)
	return reply, err
}

func (h *Handler) saveAsItem(ctx context.Context, c store.AICommand) error {
	if h.SaveAsItem == nil {
		return fmt.Errorf("command: 未配置 SaveAsItem，消息 %s 无法存为条目", c.MsgID)
	}
	err := h.SaveAsItem(ctx, c.MsgID, c.Text)
	if errors.Is(err, store.ErrDuplicate) {
		return nil // 重放：条目已经存过
	}
	return err
}

// enqueueAI 把需要 AI 翻译的指令按 msg_id 落库并唤醒后台，立即返回。
// 不即时回复：结果由后台回；处理慢时后台再补一句 Thinking。
func (h *Handler) enqueueAI(ctx context.Context, in ingest.CommandInput, hasWord bool, now time.Time) (string, bool, error) {
	msgID := in.MsgID
	if msgID == "" {
		msgID = fmt.Sprintf("local-%d", now.UnixNano()) // 没有 msg_id 无从查重，只保证能落库
	}
	err := h.Store.EnqueueAICommand(ctx, &store.AICommand{MsgID: msgID, Text: strings.TrimSpace(in.Text), RefText: in.RefText, HasWord: hasWord, CreatedAt: now})
	if errors.Is(err, store.ErrDuplicate) {
		// 重放：已在排队或已处理完（处理结果当时已回过），不再排队、不再回复
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	h.w.notify()
	return "", true, nil
}
