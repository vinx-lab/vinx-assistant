package model

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{" 发票 ", "#报销", "＃财务"}, []string{"发票", "报销", "财务"}},
		{[]string{"Go", "go", "GO "}, []string{"Go"}},
		{[]string{"", "  ", "#"}, nil},
		{[]string{"a  b\tc"}, []string{"a b c"}},
		{[]string{"1", "2", "3", "4", "5", "6"}, []string{"1", "2", "3", "4", "5"}},
		{[]string{strings.Repeat("长", 25)}, []string{strings.Repeat("长", 20)}},
	}
	for _, c := range cases {
		if got := NormalizeTags(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("NormalizeTags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
