package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/ingest"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

const Usage = "指令格式：完成 12｜推迟 12 明天｜改到 12 10-08 15:00｜取消 12｜列表｜撤销"

const listMax = 20

// RefUnknown 是引用的消息没解析出来时的回复。
const RefUnknown = "没认出引用的是哪条，请带上编号，如「完成 12」"

type Handler struct {
	Store      *store.Store
	Clock      clock.Clock
	Translator func(ctx context.Context) (Translator, error) // nil 或返回 nil：AI 未配置
	Log        *slog.Logger
}

func (h *Handler) log() *slog.Logger {
	if h.Log == nil {
		return slog.Default()
	}
	return h.Log
}

// Handle 实现 ingest.CommandHandler。handled=false 表示这不是指令，由 ingest 当普通条目保存。
func (h *Handler) Handle(ctx context.Context, in ingest.CommandInput) (string, bool, error) {
	if in.MsgID != "" {
		// 重放（进程在 MarkSeen 前崩溃）：同一 msg_id 已执行过就只回显，不再执行
		prev, err := h.Store.ActionByMsgID(ctx, in.MsgID)
		if err == nil {
			return h.replayReply(ctx, prev), true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", false, err
		}
	}
	now := h.Clock.Now()
	set, err := h.Store.LoadSettings(ctx)
	if err != nil {
		return "", false, err
	}
	cmd, err := Parse(in.Text, in.RefText, set.Rules.ActionWords, now)
	var de *DateError
	switch {
	case err == nil:
		return h.run(ctx, cmd, in, now)
	case errors.As(err, &de):
		return de.Error() + "。" + Usage, true, nil
	}
	hasWord := !errors.Is(err, ErrNoActionWord)
	if in.HasRef && strings.TrimSpace(in.RefText) == "" && !refIDRe.MatchString(in.Text) {
		// 引用存在但没解析出原文（新版微信只带 svr_id，又查不到），AI 也无从判断，不去猜
		if hasWord {
			return RefUnknown, true, nil
		}
		return "", false, nil
	}
	return h.viaAI(ctx, in, hasWord, now)
}

func (h *Handler) viaAI(ctx context.Context, in ingest.CommandInput, hasWord bool, now time.Time) (string, bool, error) {
	fallback := func(why string) (string, bool, error) {
		if hasWord {
			return why + Usage, true, nil
		}
		return "", false, nil
	}
	var tr Translator
	if h.Translator != nil {
		var err error
		if tr, err = h.Translator(ctx); err != nil {
			h.log().Warn("读取 AI 配置失败", "err", err)
		}
	}
	if tr == nil {
		return fallback("没看懂这条指令。")
	}
	todos, err := h.Store.OpenTodos(ctx, 50)
	if err != nil {
		return "", false, err
	}
	text := in.Text
	if in.RefText != "" {
		text += "\n（引用的消息：" + model.TruncateRunes(in.RefText, 300) + "）"
	}
	tl, err := tr.Translate(ctx, text, todos, now)
	if err != nil {
		h.log().Warn("AI 翻译指令失败", "err", err)
		return fallback("没看懂这条指令（AI 暂时不可用）。")
	}
	cmd, reply, ok := fromTranslation(tl, todos, now)
	if !ok {
		return "", false, nil
	}
	if reply != "" {
		return reply, true, nil
	}
	return h.run(ctx, cmd, in, now)
}

// fromTranslation 校验 AI 的结果：只能操作给出的未完成待办。ok=false 表示「不是指令」。
func fromTranslation(tl Translation, todos []model.Item, now time.Time) (Cmd, string, bool) {
	open := map[int64]bool{}
	for _, it := range todos {
		open[it.ID] = true
	}
	notFound := func(id int64) (Cmd, string, bool) {
		return Cmd{}, fmt.Sprintf("没找到未完成的待办 #%d。", id), true
	}
	switch tl.Op {
	case OpNone, "":
		return Cmd{}, "", false
	case OpList, OpUndo:
		return Cmd{Op: tl.Op}, "", true
	case OpAsk:
		var cands []int64
		for _, id := range tl.Candidates {
			if open[id] {
				cands = append(cands, id)
			}
		}
		if len(cands) == 0 {
			return Cmd{}, "没看懂是哪一条。" + Usage, true
		}
		return Cmd{Op: OpDone, Candidates: cands}, "", true
	case OpDone, OpCancel:
		if !open[tl.ID] {
			return notFound(tl.ID)
		}
		return Cmd{Op: tl.Op, ID: tl.ID}, "", true
	case OpReschedule, OpPostpone:
		if !open[tl.ID] {
			return notFound(tl.ID)
		}
		for _, layout := range []struct {
			l       string
			hasTime bool
		}{{"2006-01-02 15:04", true}, {"2006-01-02", false}} {
			if w, err := time.ParseInLocation(layout.l, strings.TrimSpace(tl.When), now.Location()); err == nil {
				return Cmd{Op: OpReschedule, ID: tl.ID, When: w, HasTime: layout.hasTime}, "", true
			}
		}
		return Cmd{}, "没看懂要改到什么时候。" + Usage, true
	}
	return Cmd{}, "没看懂这条指令。" + Usage, true
}

func (h *Handler) run(ctx context.Context, c Cmd, in ingest.CommandInput, now time.Time) (string, bool, error) {
	if len(c.Candidates) > 0 {
		reply, err := h.ask(ctx, c)
		return reply, true, err
	}
	switch c.Op {
	case OpList:
		reply, err := h.list(ctx, now)
		return reply, true, err
	case OpUndo:
		reply, err := h.undo(ctx, in.MsgID)
		return reply, true, err
	}
	var (
		reply         string
		before, after Snapshot
	)
	// 改条目与记录指令在同一事务；无改动时 fn 返回哨兵错误整体回滚
	_, err := h.Store.ModifyItemWithAction(ctx, c.ID, func(it *model.Item) (*store.Action, error) {
		before = SnapshotOf(it)
		r, changed := Apply(it, c, now)
		reply = r
		if !changed {
			return nil, errNoChange
		}
		after = SnapshotOf(it)
		b, _ := json.Marshal(before)
		a, _ := json.Marshal(after)
		return &store.Action{MsgID: in.MsgID, Command: strings.TrimSpace(in.Text), Before: string(b), After: string(a)}, nil
	})
	switch {
	case errors.Is(err, errNoChange):
		return reply, true, nil
	case errors.Is(err, store.ErrNotFound):
		return fmt.Sprintf("没有 #%d。", c.ID), true, nil
	case errors.Is(err, store.ErrDuplicate):
		prev, perr := h.Store.ActionByMsgID(ctx, in.MsgID)
		if perr != nil {
			return "", true, perr
		}
		return h.replayReply(ctx, prev), true, nil
	case err != nil:
		return "", true, err
	}
	return reply, true, nil
}

// replayReply 用已有的指令记录和条目现状拼出回显，不再执行。
func (h *Handler) replayReply(ctx context.Context, prev *store.Action) string {
	const head = "这条指令已处理过"
	if prev.ItemID == 0 {
		return head + "：" + prev.Command + "。"
	}
	it, err := h.Store.GetItem(ctx, prev.ItemID)
	if err != nil {
		return fmt.Sprintf("%s：%s（#%d）。", head, prev.Command, prev.ItemID)
	}
	now := h.Clock.Now()
	desc := fmt.Sprintf("#%d %s 现为「%s」", it.ID, model.TruncateRunes(it.DisplayTitle(), 30), model.StatusName(it.Status))
	if it.DueAt != nil {
		desc += "，截止 " + model.FormatDue(*it.DueAt, it.DueHasTime, now)
	}
	return fmt.Sprintf("%s：%s → %s。", head, prev.Command, desc)
}

var errNoChange = errors.New("command: no change")

var opWord = map[Op]string{OpDone: "完成", OpCancel: "取消", OpPostpone: "推迟", OpReschedule: "改到"}

func (h *Handler) ask(ctx context.Context, c Cmd) (string, error) {
	var parts []string
	for _, id := range c.Candidates {
		it, err := h.Store.GetItem(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("#%d %s", id, model.TruncateRunes(it.DisplayTitle(), 20)))
	}
	if len(parts) == 0 {
		return "引用的消息里的条目都不存在了。", nil
	}
	word := opWord[c.Op]
	if word == "" {
		word = "完成"
	}
	return fmt.Sprintf("是指哪一条？%s。回复时带上编号，如「%s %d」。", strings.Join(parts, "；"), word, c.Candidates[0]), nil
}

func (h *Handler) list(ctx context.Context, now time.Time) (string, error) {
	todos, err := h.Store.OpenTodos(ctx, 1000)
	if err != nil {
		return "", err
	}
	if len(todos) == 0 {
		return "没有未完成的待办。", nil
	}
	lines := []string{fmt.Sprintf("未完成的待办（%d）：", len(todos))}
	for i, it := range todos {
		if i == listMax {
			lines = append(lines, fmt.Sprintf("…还有 %d 条，见网页", len(todos)-listMax))
			break
		}
		line := fmt.Sprintf("#%d %s", it.ID, model.TruncateRunes(it.DisplayTitle(), 30))
		if it.DueAt != nil {
			line += " · " + model.FormatDue(*it.DueAt, it.DueHasTime, now)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

var errCatChanged = errors.New("command: category changed")

func (h *Handler) undo(ctx context.Context, msgID string) (string, error) {
	a, err := h.Store.LastAction(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return "没有可以撤销的操作。", nil
	}
	if err != nil {
		return "", err
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(a.Before), &snap); err != nil {
		return "", err
	}
	it, err := h.Store.UndoAction(ctx, a, msgID, func(it *model.Item) error {
		if !statusValid(it.Category, snap.Status) {
			return errCatChanged
		}
		snap.ApplyTo(it)
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		_ = h.Store.MarkUndone(ctx, a.ID)
		return "原条目已不存在，无法撤销。", nil
	case errors.Is(err, errCatChanged):
		return fmt.Sprintf("#%d 的分类已变，无法撤销。", a.ItemID), nil
	case errors.Is(err, store.ErrDuplicate):
		prev, perr := h.Store.ActionByMsgID(ctx, msgID)
		if perr != nil {
			return "", perr
		}
		return h.replayReply(ctx, prev), nil
	case err != nil:
		return "", err
	}
	reply := fmt.Sprintf("↩ 已撤销「%s」：#%d 恢复为「%s」", a.Command, it.ID, model.StatusName(it.Status))
	if it.DueAt != nil && (snap.DueAt != nil) {
		reply += "，截止 " + model.FormatDue(*it.DueAt, it.DueHasTime, h.Clock.Now())
	}
	return reply, nil
}
