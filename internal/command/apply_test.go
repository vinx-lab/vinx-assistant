package command

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func tptr(t time.Time) *time.Time { return &t }

func TestNewDue(t *testing.T) {
	now := clock.At(2026, 10, 1, 9, 0)
	cases := []struct {
		name     string
		due      *time.Time
		hasTime  bool
		shift    Shift
		want     time.Time
		wantTime bool
	}{
		{"明天保留时刻", tptr(clock.At(2026, 9, 28, 15, 0)), true, Shift{Days: 1, Absolute: true}, clock.At(2026, 10, 2, 15, 0), true},
		{"明天日期型", tptr(clock.At(2026, 10, 1, 0, 0)), false, Shift{Days: 1, Absolute: true}, clock.At(2026, 10, 2, 0, 0), false},
		{"无截止 3天从今天算", nil, false, Shift{Days: 3}, clock.At(2026, 10, 4, 0, 0), false},
		{"无截止 2小时从现在算", nil, false, Shift{Hours: 2}, clock.At(2026, 10, 1, 11, 0), true},
		{"未来截止加天", tptr(clock.At(2026, 10, 5, 10, 0)), true, Shift{Days: 2}, clock.At(2026, 10, 7, 10, 0), true},
		{"逾期截止加天从今天算", tptr(clock.At(2026, 9, 20, 10, 0)), true, Shift{Days: 2}, clock.At(2026, 10, 3, 10, 0), true},
		{"下周", tptr(clock.At(2026, 10, 2, 0, 0)), false, Shift{Days: 7}, clock.At(2026, 10, 9, 0, 0), false},
		{"未来截止加小时", tptr(clock.At(2026, 10, 1, 15, 0)), true, Shift{Hours: 1}, clock.At(2026, 10, 1, 16, 0), true},
	}
	for _, c := range cases {
		it := &model.Item{DueAt: c.due, DueHasTime: c.hasTime}
		got, ht := NewDue(it, Cmd{Op: OpPostpone, Shift: c.shift}, now)
		if !got.Equal(c.want) || ht != c.wantTime {
			t.Errorf("%s: got %v %v, want %v %v", c.name, got, ht, c.want, c.wantTime)
		}
	}
}

func TestApply(t *testing.T) {
	now := clock.At(2026, 10, 1, 9, 0)
	cases := []struct {
		cat        model.Category
		status     string
		cmd        Cmd
		wantStatus string
		changed    bool
		replyHas   string
	}{
		{model.CatTodo, model.StatusOpen, Cmd{Op: OpDone}, model.StatusDone, true, "✓ 已完成 #1"},
		{model.CatTodo, model.StatusDone, Cmd{Op: OpDone}, model.StatusDone, false, "已经是"},
		{model.CatResearch, model.StatusDoing, Cmd{Op: OpDone}, model.StatusDone, true, "✓ 已完成"},
		{model.CatLater, model.StatusNew, Cmd{Op: OpDone}, model.StatusRead, true, "✓ 已完成"},
		{model.CatIdea, model.StatusKept, Cmd{Op: OpDone}, model.StatusKept, false, "不需要标完成"},
		{model.CatTodo, model.StatusOpen, Cmd{Op: OpCancel}, model.StatusCancelled, true, "✓ 已取消"},
		{model.CatResearch, model.StatusNew, Cmd{Op: OpCancel}, model.StatusDropped, true, "✓ 已取消"},
		{model.CatLater, model.StatusNew, Cmd{Op: OpCancel}, model.StatusNew, false, "不能取消"},
		{model.CatResearch, model.StatusNew, Cmd{Op: OpPostpone, Shift: Shift{Days: 1, Absolute: true}}, model.StatusNew, false, "只有待办"},
		{model.CatTodo, model.StatusDone, Cmd{Op: OpPostpone, Shift: Shift{Days: 1, Absolute: true}}, model.StatusDone, false, "已经是「完成」"},
		{model.CatTodo, model.StatusOpen, Cmd{Op: OpReschedule, When: clock.At(2026, 10, 8, 15, 0), HasTime: true}, model.StatusOpen, true, "10-08 周四 15:00"},
	}
	for i, c := range cases {
		it := &model.Item{ID: 1, RawText: "交发票", Category: c.cat, Status: c.status}
		reply, changed := Apply(it, c.cmd, now)
		if it.Status != c.wantStatus || changed != c.changed || !strings.Contains(reply, c.replyHas) {
			t.Errorf("case %d: status=%s changed=%v reply=%q", i, it.Status, changed, reply)
		}
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	due := clock.At(2026, 10, 8, 15, 0)
	it := &model.Item{Status: model.StatusOpen, DueAt: &due, DueHasTime: true}
	b, _ := json.Marshal(SnapshotOf(it))
	if string(b) != `{"status":"open","due_at":`+jsonInt(due.Unix())+`,"due_has_time":true}` {
		t.Fatalf("json = %s", b)
	}
	it.Status, it.DueAt, it.DueHasTime = model.StatusDone, nil, false
	var s Snapshot
	json.Unmarshal(b, &s)
	s.ApplyTo(it)
	if it.Status != model.StatusOpen || it.DueAt == nil || !it.DueAt.Equal(due) || !it.DueHasTime {
		t.Fatalf("restored = %+v", it)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }
