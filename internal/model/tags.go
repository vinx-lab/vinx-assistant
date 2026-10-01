package model

import "strings"

const (
	MaxTags     = 5
	MaxTagRunes = 20
)

// NormalizeTags 清理标签：去空白和开头的 #，截到 20 个字，忽略大小写去重，最多 5 个。
func NormalizeTags(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimLeft(strings.TrimSpace(t), "#＃")
		t = strings.Join(strings.Fields(t), " ")
		if t == "" {
			continue
		}
		if r := []rune(t); len(r) > MaxTagRunes {
			t = string(r[:MaxTagRunes])
		}
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
		if len(out) == MaxTags {
			break
		}
	}
	return out
}
