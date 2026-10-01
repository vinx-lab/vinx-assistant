package clock

import (
	"testing"
	"time"
)

func TestFakeAdvanceAndZone(t *testing.T) {
	f := NewFake(time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC))
	if got := f.Now().Hour(); got != 8 {
		t.Fatalf("hour in CST = %d, want 8", got)
	}
	f.Advance(90 * time.Minute)
	if got := f.Now().Format("15:04"); got != "10:00" {
		t.Fatalf("after advance = %s, want 10:00", got)
	}
	f.Set(At(2026, 12, 31, 23, 59))
	if got := DayString(f.Now()); got != "2026-12-31" {
		t.Fatalf("DayString = %s", got)
	}
}

func TestDayStringUsesShanghai(t *testing.T) {
	// UTC 2026-10-01 17:00 = 上海 2026-10-02 01:00
	if got := DayString(time.Date(2026, 10, 1, 17, 0, 0, 0, time.UTC)); got != "2026-10-02" {
		t.Fatalf("DayString = %s, want 2026-10-02", got)
	}
}

func TestRealIsInZone(t *testing.T) {
	if (Real{}).Now().Location() != Zone {
		t.Fatal("Real.Now must be in Zone")
	}
}
