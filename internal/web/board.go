package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

var priorityNames = map[model.Priority]string{model.PriorityHigh: "高", model.PriorityMedium: "中", model.PriorityLow: "低", model.PriorityNone: "—"}

var statusOrder = []string{model.StatusNew, model.StatusDoing, model.StatusOpen, model.StatusRead, model.StatusDone, model.StatusDropped, model.StatusCancelled, model.StatusKept}

// isOverdue 与计划 3 的 DigestData 一致：只有日期的待办，过了当天才算逾期。
func isOverdue(it model.Item, now time.Time) bool {
	if it.DueAt == nil || !model.IsOpen(it.Category, it.Status) {
		return false
	}
	if it.DueHasTime {
		return it.DueAt.Before(now)
	}
	y, m, d := now.In(clock.Zone).Date()
	return it.DueAt.Before(time.Date(y, m, d, 0, 0, 0, 0, clock.Zone))
}

// StatusBar 是看板顶部的状态栏。
type StatusBar struct {
	Overdue, DueToday int
	WeChat            string // ok / paused / no_cred
	PausedUntil       string // 暂停中时自动重试的时刻
	NeedRelogin       bool   // 连续两次以上 -14
	TokensToday       int64
	TokenLimit        int64 // 0 = 不限
	NextBatch         string
	Running           bool
	Drift             int // 协议之外的字段个数
}

func (s *Server) statusBar(ctx context.Context, now time.Time, st model.Settings) (StatusBar, error) {
	var b StatusBar
	dd, err := s.d.Store.DigestData(ctx, now)
	if err != nil {
		return b, err
	}
	b.Overdue, b.DueToday = len(dd.Overdue), len(dd.DueToday)
	b.WeChat = s.d.Session.Status(ctx)
	if b.WeChat == session.StatusPaused {
		b.PausedUntil = s.d.Session.PausedUntil(ctx).In(clock.Zone).Format("15:04")
	}
	b.NeedRelogin = s.d.Session.StaleCount(ctx) >= 2
	if b.TokensToday, err = s.d.Store.TokensOn(ctx, clock.DayString(now)); err != nil {
		return b, err
	}
	b.TokenLimit = st.AI.DailyTokenLimit
	if s.d.NextBatch != nil {
		if t, ok := s.d.NextBatch(now, st.Schedule.BatchTimes); ok {
			b.NextBatch = relTime(t, now)
		}
	}
	if s.d.BatchRunning != nil {
		b.Running = s.d.BatchRunning()
	}
	if raw, ok, _ := s.d.Store.GetKV(ctx, "ilink.drift"); ok && raw != "" {
		var m map[string]int64
		if json.Unmarshal([]byte(raw), &m) == nil {
			b.Drift = len(m)
		}
	}
	return b, nil
}

// relTime 显示「今天 20:00」「明天 08:00」或「10-08 08:00」。
func relTime(t, now time.Time) string {
	t, now = t.In(clock.Zone), now.In(clock.Zone)
	day := func(x time.Time) time.Time { y, m, d := x.Date(); return time.Date(y, m, d, 0, 0, 0, 0, clock.Zone) }
	switch day(t).Sub(day(now)) {
	case 0:
		return "今天 " + t.Format("15:04")
	case 24 * time.Hour:
		return "明天 " + t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// boardOrder 是看板分类的顺序：未整理放最前面，提醒先把它们处理掉。
var boardOrder = []model.Category{model.CatInbox, model.CatTodo, model.CatResearch, model.CatLater, model.CatIdea, model.CatArchive}

// categoryIcons 是分类对应的图标（layout.html 里 SVG sprite 的 id 后缀）。
var categoryIcons = map[model.Category]string{
	model.CatInbox: "tray", model.CatTodo: "todo", model.CatResearch: "flask",
	model.CatLater: "clock", model.CatIdea: "bulb", model.CatArchive: "archive",
}

type tab struct {
	Cat    model.Category
	Name   string
	Count  int
	Active bool
	Href   string
}

type boardData struct {
	Page
	Status StatusBar
	Tabs   []tab
	Cat    model.Category
	Tag    string
	Done   bool // 「已处理」视图
	Toggle bool // 这个分类有「未处理 / 已处理」切换（点子、资料、未整理没有）
	// 切换和清除标签的链接，以及两个视图的条目数
	OpenHref, DoneHref, ClearTagHref string
	OpenCount, DoneCount             int
	Labels                           []store.TagCount // 类别标签（关键词规则产生）
	Topics                           []store.TagCount // 内容标签（AI 整理生成）
	// 侧栏「内容标签」：前 topicHeadN 个直接显示，其余折叠；当前筛选的标签在折叠部分时默认展开
	TopicHead, TopicMore []store.TagCount
	TopicMoreOpen        bool
	Items                []model.Item
	Thumbs               map[int64]string
	Back                 string
}

func boardHref(cat model.Category, tag string, done bool) string {
	q := url.Values{"cat": {string(cat)}}
	if tag != "" {
		q.Set("tag", tag)
	}
	if done {
		q.Set("done", "1")
	}
	return "/?" + q.Encode()
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), s.d.Clock.Now()
	q := r.URL.Query()
	cat := model.Category(q.Get("cat"))
	if !model.ValidCategory(cat) {
		cat = model.CatTodo
	}
	tag, done := q.Get("tag"), q.Get("done") == "1" && store.HasDoneView(cat)
	st, err := s.d.Store.LoadSettings(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	d := boardData{Page: s.page(r, "看板", "board"), Cat: cat, Tag: tag, Done: done, Toggle: store.HasDoneView(cat), Back: boardHref(cat, tag, done),
		OpenHref: boardHref(cat, tag, false), DoneHref: boardHref(cat, tag, true), ClearTagHref: boardHref(cat, "", done)}
	if d.Status, err = s.statusBar(ctx, now, st); err != nil {
		s.fail(w, err)
		return
	}
	open, doneCounts, err := s.d.Store.BoardCounts(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.OpenCount, d.DoneCount = open[cat], doneCounts[cat]
	// 分类标签上的数字是默认视图（未处理 + 点子、资料）的条目数；切到别的分类回到默认视图
	for _, c := range boardOrder {
		d.Tabs = append(d.Tabs, tab{Cat: c, Name: model.CategoryName(c), Count: open[c], Active: c == cat, Href: boardHref(c, tag, false)})
	}
	if d.Items, err = s.d.Store.ListItems(ctx, store.ListQuery{Category: cat, Tag: tag, Done: done}); err != nil {
		s.fail(w, err)
		return
	}
	ids := make([]int64, len(d.Items))
	for i, it := range d.Items {
		ids[i] = it.ID
	}
	if d.Thumbs, err = s.d.Store.FirstImages(ctx, ids); err != nil {
		s.fail(w, err)
		return
	}
	if d.Labels, err = s.d.Store.AllTags(ctx, store.TagKindLabel); err != nil {
		s.fail(w, err)
		return
	}
	if d.Topics, err = s.d.Store.AllTags(ctx, store.TagKindTopic); err != nil {
		s.fail(w, err)
		return
	}
	d.TopicHead, d.TopicMore, d.TopicMoreOpen = splitTopics(d.Topics, topicHeadN, tag)
	s.render(w, http.StatusOK, "board", d)
}

// topicHeadN 是侧栏默认显示的内容标签个数。
const topicHeadN = 15

// splitTopics 把标签分成默认显示的前 n 个和折叠的其余部分；active 落在折叠部分时 open 为 true。
func splitTopics(tags []store.TagCount, n int, active string) (head, more []store.TagCount, open bool) {
	if len(tags) <= n {
		return tags, nil, false
	}
	head, more = tags[:n], tags[n:]
	for _, t := range more {
		if t.Name == active {
			open = true
		}
	}
	return head, more, open
}

func (s *Server) batchRun(w http.ResponseWriter, r *http.Request) {
	back := safeBack(r.FormValue("back"))
	if s.d.BatchNow != nil && s.d.BatchNow() {
		redirect(w, r, withMsg(back, "started"))
		return
	}
	redirect(w, r, withMsg(back, "busy"))
}

func (s *Server) itemID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func (s *Server) loadItem(w http.ResponseWriter, r *http.Request) (*model.Item, bool) {
	id, ok := s.itemID(w, r)
	if !ok {
		return nil, false
	}
	it, err := s.d.Store.GetItem(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return it, true
}

// badInput 是表单校验错误，与存储错误区分开：前者回 400，后者回 500。
type badInput struct{ msg string }

func (e badInput) Error() string { return e.msg }

// modify 用 store.ModifyItem 在事务里读改写条目，避免覆盖期间别处（如 AI 批处理）的修改。
// 处理好 404/400/500，返回 true 表示成功。
func (s *Server) modify(w http.ResponseWriter, r *http.Request, fn func(it *model.Item) error) (*model.Item, bool) {
	id, ok := s.itemID(w, r)
	if !ok {
		return nil, false
	}
	it, err := s.d.Store.ModifyItem(r.Context(), id, fn)
	var bad badInput
	switch {
	case err == nil:
		return it, true
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	case errors.As(err, &bad):
		http.Error(w, bad.msg, http.StatusBadRequest)
	default:
		s.fail(w, err)
	}
	return nil, false
}

func (s *Server) itemStatus(w http.ResponseWriter, r *http.Request) {
	st := r.FormValue("status")
	if _, ok := s.modify(w, r, func(it *model.Item) error {
		if !model.ValidStatus(it.Category, st) {
			return badInput{"这个分类没有该状态"}
		}
		it.Status = st
		return nil
	}); !ok {
		return
	}
	redirect(w, r, withMsg(safeBack(r.FormValue("back")), "status"))
}

// itemDeep 是「深入研究」按钮：把深度提到 deep、清零失败次数，等下一批次处理；勾了「立即整理」就马上跑。
func (s *Server) itemDeep(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.modify(w, r, func(it *model.Item) error {
		it.Level, it.ProcessAttempts, it.ProcessError = model.LevelDeep, 0, ""
		return nil
	}); !ok {
		return
	}
	msg := "deep"
	if r.FormValue("now") == "1" && s.d.BatchNow != nil {
		if s.d.BatchNow() {
			msg = "deepnow"
		} else {
			msg = "busy"
		}
	}
	redirect(w, r, withMsg(safeBack(r.FormValue("back")), msg))
}
