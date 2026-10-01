package command

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
)

var words = model.DefaultSettings().Rules.ActionWords

func TestParse(t *testing.T) {
	now := clock.At(2026, 10, 1, 9, 0)
	cases := []struct {
		text, ref string
		want      Cmd
	}{
		{"完成 12", "", Cmd{Op: OpDone, ID: 12}},
		{"搞定#12", "", Cmd{Op: OpDone, ID: 12}},
		{"完成 #12。", "", Cmd{Op: OpDone, ID: 12}},
		{"完成", `{"title":"⏰ 到期提醒\n#12 交发票"}`, Cmd{Op: OpDone, ID: 12}},
		{"完成", `{"title":"#12 交发票 #15 交材料 #12"}`, Cmd{Op: OpDone, Candidates: []int64{12, 15}}},
		{"删除 3", "", Cmd{Op: OpCancel, ID: 3}},
		{"取消12", "", Cmd{Op: OpCancel, ID: 12}},
		{"列表", "", Cmd{Op: OpList}},
		{"撤销！", "", Cmd{Op: OpUndo}},
		{"推迟 15 明天", "", Cmd{Op: OpPostpone, ID: 15, Shift: Shift{Days: 1, Absolute: true}}},
		{"推迟 #15 后天", "", Cmd{Op: OpPostpone, ID: 15, Shift: Shift{Days: 2, Absolute: true}}},
		{"推迟 15 3天", "", Cmd{Op: OpPostpone, ID: 15, Shift: Shift{Days: 3}}},
		{"推迟 15 2小时", "", Cmd{Op: OpPostpone, ID: 15, Shift: Shift{Hours: 2}}},
		{"推迟 15 下周", "", Cmd{Op: OpPostpone, ID: 15, Shift: Shift{Days: 7}}},
		{"推迟 2天", "提醒 #9", Cmd{Op: OpPostpone, ID: 9, Shift: Shift{Days: 2}}},
		{"改到 15 10-08 15:00", "", Cmd{Op: OpReschedule, ID: 15, When: clock.At(2026, 10, 8, 15, 0), HasTime: true}},
		{"改到 15 10-08", "", Cmd{Op: OpReschedule, ID: 15, When: clock.At(2026, 10, 8, 0, 0)}},
		{"改到 15 2026-11-02 9:30", "", Cmd{Op: OpReschedule, ID: 15, When: clock.At(2026, 11, 2, 9, 30), HasTime: true}},
		{"改到 15 明天 9：30", "", Cmd{Op: OpReschedule, ID: 15, When: clock.At(2026, 10, 2, 9, 30), HasTime: true}},
		{"改到 15 10月8日", "", Cmd{Op: OpReschedule, ID: 15, When: clock.At(2026, 10, 8, 0, 0)}},
		{"改到 15 01-05", "", Cmd{Op: OpReschedule, ID: 15, When: clock.At(2027, 1, 5, 0, 0)}},  // 早于今天 → 明年
		{"改到 15 10-01", "", Cmd{Op: OpReschedule, ID: 15, When: clock.At(2026, 10, 1, 0, 0)}}, // 今天不算早于今天
		{"改到 10-08", "#7 交发票", Cmd{Op: OpReschedule, ID: 7, When: clock.At(2026, 10, 8, 0, 0)}},
	}
	for _, c := range cases {
		got, err := Parse(c.text, c.ref, words, now)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.text, err)
			continue
		}
		// time.Time 用 Equal 比较，其余字段用 DeepEqual
		sameWhen := got.When.Equal(c.want.When)
		got.When, c.want.When = time.Time{}, time.Time{}
		if !sameWhen || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q) = %+v, want %+v", c.text, got, c.want)
		}
	}
}

func TestParseYearRollover(t *testing.T) {
	cases := []struct {
		now  time.Time
		in   string
		want time.Time
	}{
		{clock.At(2026, 12, 31, 20, 0), "改到 1 12-31", clock.At(2026, 12, 31, 0, 0)},
		{clock.At(2027, 1, 2, 9, 0), "改到 1 12-31", clock.At(2027, 12, 31, 0, 0)},
		{clock.At(2026, 12, 20, 9, 0), "改到 1 01-05", clock.At(2027, 1, 5, 0, 0)},
	}
	for _, c := range cases {
		got, err := Parse(c.in, "", words, c.now)
		if err != nil || !got.When.Equal(c.want) {
			t.Errorf("now=%v %q → %v err=%v, want %v", c.now, c.in, got.When, err, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	now := clock.At(2026, 10, 1, 9, 0)
	unrecognized := []string{"完成发票那个", "推迟 15", "推迟 15 改天", "列表 全部", "完成", "完成 12 13"}
	for _, s := range unrecognized {
		if _, err := Parse(s, "", words, now); !errors.Is(err, ErrUnrecognized) {
			t.Errorf("Parse(%q) err = %v, want ErrUnrecognized", s, err)
		}
	}
	if _, err := Parse("好的", "#12", words, now); !errors.Is(err, ErrNoActionWord) {
		t.Errorf("no action word err = %v", err)
	}
	for _, s := range []string{"改到 12 02-30", "改到 12 2026-13-01", "改到 12 10-08 25:00", "改到 12 10-08 9:75"} {
		var de *DateError
		if _, err := Parse(s, "", words, now); !errors.As(err, &de) {
			t.Errorf("Parse(%q) err = %v, want DateError", s, err)
		}
	}
}

func TestRefIDs(t *testing.T) {
	if got := RefIDs(`#3 a #10 b #3`); !reflect.DeepEqual(got, []int64{3, 10}) {
		t.Fatalf("RefIDs = %v", got)
	}
	if got := RefIDs(""); got != nil {
		t.Fatalf("empty = %v", got)
	}
}
