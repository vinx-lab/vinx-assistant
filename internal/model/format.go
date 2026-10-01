package model

import "time"

var weekdayNames = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

func WeekdayName(t time.Time) string { return weekdayNames[t.Weekday()] }

// FormatDue 显示截止时间：同年「10-08 周四 15:00」，跨年带年份；hasTime=false 时不带时刻。
func FormatDue(t time.Time, hasTime bool, now time.Time) string {
	t = t.In(now.Location())
	s := t.Format("01-02")
	if t.Year() != now.Year() {
		s = t.Format("2006-01-02")
	}
	s += " " + WeekdayName(t)
	if hasTime {
		s += " " + t.Format("15:04")
	}
	return s
}

var statusNames = map[string]string{
	StatusNew: "待看", StatusDoing: "研究中", StatusDone: "完成", StatusDropped: "放弃",
	StatusRead: "已看", StatusOpen: "待办", StatusCancelled: "已取消", StatusKept: "存档",
}

func StatusName(s string) string {
	if n, ok := statusNames[s]; ok {
		return n
	}
	return s
}
