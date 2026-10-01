package command

import (
	"fmt"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

// Snapshot 是撤销要恢复的三列。
type Snapshot struct {
	Status     string `json:"status"`
	DueAt      *int64 `json:"due_at"`
	DueHasTime bool   `json:"due_has_time"`
}

func SnapshotOf(it *model.Item) Snapshot {
	s := Snapshot{Status: it.Status, DueHasTime: it.DueHasTime}
	if it.DueAt != nil {
		v := it.DueAt.Unix()
		s.DueAt = &v
	}
	return s
}

func (s Snapshot) ApplyTo(it *model.Item) {
	it.Status, it.DueHasTime = s.Status, s.DueHasTime
	it.DueAt = nil
	if s.DueAt != nil {
		t := time.Unix(*s.DueAt, 0).In(clock.Zone)
		it.DueAt = &t
	}
}

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func timeOfDay(t time.Time) time.Duration {
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
}

// NewDue 计算推迟或改期后的截止时间。
func NewDue(it *model.Item, c Cmd, now time.Time) (time.Time, bool) {
	if c.Op == OpReschedule {
		return c.When, c.HasTime
	}
	today := dayStart(now)
	s := c.Shift
	switch {
	case s.Absolute:
		d := today.AddDate(0, 0, s.Days)
		if it.DueAt != nil && it.DueHasTime {
			return d.Add(timeOfDay(it.DueAt.In(now.Location()))), true
		}
		return d, false
	case s.Hours > 0:
		base := now
		if it.DueAt != nil && it.DueHasTime && it.DueAt.After(now) {
			base = *it.DueAt
		}
		return base.Add(time.Duration(s.Hours) * time.Hour), true
	default:
		if it.DueAt == nil {
			return today.AddDate(0, 0, s.Days), false
		}
		due := it.DueAt.In(now.Location())
		day := dayStart(due)
		if day.Before(today) {
			day = today
		}
		d := day.AddDate(0, 0, s.Days)
		if it.DueHasTime {
			return d.Add(timeOfDay(due)), true
		}
		return d, false
	}
}

var doneTarget = map[model.Category]string{
	model.CatTodo: model.StatusDone, model.CatResearch: model.StatusDone, model.CatLater: model.StatusRead,
}

var cancelTarget = map[model.Category]string{
	model.CatTodo: model.StatusCancelled, model.CatResearch: model.StatusDropped,
}

// Apply 把指令作用到条目上（只改内存），返回回复文本和是否有改动。
func Apply(it *model.Item, c Cmd, now time.Time) (string, bool) {
	ref := fmt.Sprintf("#%d %s", it.ID, model.TruncateRunes(it.DisplayTitle(), 30))
	cat := model.CategoryName(it.Category)
	switch c.Op {
	case OpDone, OpCancel:
		targets, verb, refuse := doneTarget, "已完成", "不需要标完成"
		if c.Op == OpCancel {
			targets, verb, refuse = cancelTarget, "已取消", "不能取消"
		}
		target, ok := targets[it.Category]
		if !ok {
			return fmt.Sprintf("%s 是「%s」，%s。", ref, cat, refuse), false
		}
		if it.Status == target {
			return fmt.Sprintf("%s 已经是「%s」。", ref, model.StatusName(target)), false
		}
		it.Status = target
		return "✓ " + verb + " " + ref, true
	case OpPostpone, OpReschedule:
		if it.Category != model.CatTodo {
			return fmt.Sprintf("只有待办可以改截止时间，%s 是「%s」。", ref, cat), false
		}
		if it.Status != model.StatusOpen {
			return fmt.Sprintf("%s 已经是「%s」，没有改。", ref, model.StatusName(it.Status)), false
		}
		due, hasTime := NewDue(it, c, now)
		it.DueAt, it.DueHasTime = &due, hasTime
		return "✓ " + ref + " 截止改到 " + model.FormatDue(due, hasTime, now), true
	}
	return "没看懂这条指令。" + Usage, false
}
