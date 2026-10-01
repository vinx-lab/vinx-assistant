package model

import (
	"testing"
	"time"
)

func TestFormatDue(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, cst)
	cases := []struct {
		t       time.Time
		hasTime bool
		want    string
	}{
		{time.Date(2026, 10, 8, 15, 0, 0, 0, cst), true, "10-08 周四 15:00"},
		{time.Date(2026, 10, 8, 0, 0, 0, 0, cst), false, "10-08 周四"},
		{time.Date(2027, 1, 2, 0, 0, 0, 0, cst), false, "2027-01-02 周六"},
	}
	for _, c := range cases {
		if got := FormatDue(c.t, c.hasTime, now); got != c.want {
			t.Errorf("FormatDue(%v,%v) = %q, want %q", c.t, c.hasTime, got, c.want)
		}
	}
}

func TestStatusName(t *testing.T) {
	if StatusName(StatusDone) != "完成" || StatusName(StatusOpen) != "待办" || StatusName("x") != "x" {
		t.Fatal("status names wrong")
	}
}
