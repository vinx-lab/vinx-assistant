package model

import "testing"

func TestDefaultAndValidStatus(t *testing.T) {
	cases := []struct {
		cat Category
		def string
		ok  []string
		bad []string
	}{
		{CatInbox, StatusNew, []string{"new"}, []string{"done"}},
		{CatResearch, StatusNew, []string{"new", "doing", "done", "dropped"}, []string{"open"}},
		{CatLater, StatusNew, []string{"new", "read"}, []string{"done"}},
		{CatTodo, StatusOpen, []string{"open", "done", "cancelled"}, []string{"new"}},
		{CatIdea, StatusKept, []string{"kept"}, []string{"new"}},
		{CatArchive, StatusKept, []string{"kept"}, []string{"open"}},
	}
	for _, c := range cases {
		if got := DefaultStatus(c.cat); got != c.def {
			t.Errorf("DefaultStatus(%s) = %s, want %s", c.cat, got, c.def)
		}
		for _, s := range c.ok {
			if !ValidStatus(c.cat, s) {
				t.Errorf("ValidStatus(%s,%s) = false", c.cat, s)
			}
		}
		for _, s := range c.bad {
			if ValidStatus(c.cat, s) {
				t.Errorf("ValidStatus(%s,%s) = true", c.cat, s)
			}
		}
	}
}

func TestLevelRank(t *testing.T) {
	if !(Level("").Rank() < LevelLight.Rank() && LevelLight.Rank() < LevelMedium.Rank() && LevelMedium.Rank() < LevelDeep.Rank()) {
		t.Fatal("rank order broken")
	}
}

func TestDisplayTitle(t *testing.T) {
	cases := []struct {
		it   Item
		want string
	}{
		{Item{ID: 1, Title: "标题", LinkTitle: "网页"}, "标题"},
		{Item{ID: 2, LinkTitle: "网页"}, "网页"},
		{Item{ID: 3, RawText: "一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十多出来\n第二行"}, "一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十…"},
		{Item{ID: 4}, "#4"},
	}
	for _, c := range cases {
		if got := c.it.DisplayTitle(); got != c.want {
			t.Errorf("DisplayTitle(#%d) = %q, want %q", c.it.ID, got, c.want)
		}
	}
}

func TestDefaultSettings(t *testing.T) {
	s := DefaultSettings()
	if len(s.Schedule.BatchTimes) != 2 || s.Schedule.DigestTime != "09:00" {
		t.Fatalf("schedule = %+v", s.Schedule)
	}
	if s.AI.Images || s.AI.DailyTokenLimit != 200000 {
		t.Fatalf("ai = %+v", s.AI)
	}
	found := false
	for _, p := range s.Rules.Prefixes {
		if p.Prefix == "待研究" && p.Category == CatResearch {
			found = true
		}
	}
	if !found {
		t.Fatal("missing 待研究 prefix")
	}
}
