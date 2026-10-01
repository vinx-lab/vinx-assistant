package web

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestParseTimes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{"08:00\n20:00", []string{"08:00", "20:00"}, false},
		{"20:00, 8:00，8：00", []string{"08:00", "20:00"}, false},
		{"", nil, false},
		{"25:00", nil, true},
		{"8点", nil, true},
		{"12:60", nil, true},
	}
	for _, c := range cases {
		got, err := ParseTimes(c.in)
		if (err != nil) != c.err || (!c.err && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("ParseTimes(%q) = %v, %v", c.in, got, err)
		}
	}
}

func TestParsePrefixes(t *testing.T) {
	got, err := ParsePrefixes("待办=待办\n研究：=research\n\n 稍后看 = 稍后看 ")
	want := []model.PrefixRule{{Prefix: "待办", Category: model.CatTodo}, {Prefix: "研究", Category: model.CatResearch}, {Prefix: "稍后看", Category: model.CatLater}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v %v", got, err)
	}
	for _, bad := range []string{"待办", "待办=未整理", "待办=foo", "a b=todo", "待办=todo\n待办=idea", "=todo"} {
		if _, err := ParsePrefixes(bad); err == nil {
			t.Errorf("ParsePrefixes(%q) must fail", bad)
		}
	}
	if _, err := ParsePrefixes("x=todo\ny=bad"); err == nil || !strings.Contains(err.Error(), "第 2 行") {
		t.Errorf("error must name the line: %v", err)
	}
	round, _ := ParsePrefixes(FormatPrefixes(model.DefaultSettings().Rules.Prefixes))
	if !reflect.DeepEqual(round, model.DefaultSettings().Rules.Prefixes) {
		t.Errorf("format/parse round trip = %v", round)
	}
}

func TestParseActionWords(t *testing.T) {
	def := model.DefaultSettings().Rules.ActionWords
	got, err := ParseActionWords(FormatActionWords(def))
	if err != nil || !reflect.DeepEqual(got, def) {
		t.Fatalf("round trip: %v %v", got, err)
	}
	for _, bad := range []string{"", "完成", "完成=finish", "完成=done\n完成=undo"} {
		if _, err := ParseActionWords(bad); err == nil {
			t.Errorf("ParseActionWords(%q) must fail", bad)
		}
	}
}

func TestParseWords(t *testing.T) {
	if got := ParseWords("研究一下, 看看\n研究一下"); !reflect.DeepEqual(got, []string{"研究一下", "看看"}) {
		t.Fatalf("got %v", got)
	}
}
