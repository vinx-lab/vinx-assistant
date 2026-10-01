// Package command 处理微信里的待办指令：固定格式直接执行，解析不了时交给 AI 翻译成标准指令。
package command

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

type Op string

const (
	OpDone       Op = "done"
	OpPostpone   Op = "postpone"
	OpReschedule Op = "reschedule"
	OpCancel     Op = "cancel"
	OpList       Op = "list"
	OpUndo       Op = "undo"
	OpAsk        Op = "ask"  // 只出现在 AI 翻译结果里
	OpNone       Op = "none" // 只出现在 AI 翻译结果里：不是指令
)

type Shift struct {
	Days     int
	Hours    int
	Absolute bool // 明天/后天：目标日期 = 今天 + Days
}

type Cmd struct {
	Op         Op
	ID         int64
	Candidates []int64
	Shift      Shift
	When       time.Time
	HasTime    bool
}

var (
	ErrNoActionWord = errors.New("command: 不是以操作词开头")
	ErrUnrecognized = errors.New("command: 固定格式解析不了")
)

type DateError struct{ Input string }

func (e *DateError) Error() string { return "日期不对：" + e.Input }

var (
	refIDRe   = regexp.MustCompile(`#(\d+)`)
	leadIDRe  = regexp.MustCompile(`^(?:#\s*(\d+)|(\d+)(?:\s+|$))\s*(.*)$`)
	shiftRe   = regexp.MustCompile(`^(\d{1,3})\s*(天|日|小时|个小时|h|H)$`)
	whenRe    = regexp.MustCompile(`^(今天|明天|后天|(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})|(\d{1,2})[-/.](\d{1,2})|(\d{1,2})月(\d{1,2})[日号]?)(?:\s*(\d{1,2})[:：](\d{2}))?$`)
	trailPunc = "。.！!~～ "
)

// RefIDs 按出现顺序返回引用文本里的 #编号（去重）。
func RefIDs(refText string) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for _, m := range refIDRe.FindAllStringSubmatch(refText, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func matchWord(t string, words []model.ActionWord) (Op, string, bool) {
	sorted := append([]model.ActionWord(nil), words...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].Word) > len(sorted[j].Word) })
	for _, w := range sorted {
		if w.Word != "" && strings.HasPrefix(t, w.Word) {
			return Op(w.Op), t[len(w.Word):], true
		}
	}
	return "", "", false
}

// leadingID 取开头的编号。带 # 时后面可以紧跟内容；不带 # 时编号后必须是空白或结尾（「2天」不是编号）。
func leadingID(s string) (int64, string, bool) {
	m := leadIDRe.FindStringSubmatch(s)
	if m == nil {
		return 0, s, false
	}
	digits := m[1]
	if digits == "" {
		digits = m[2]
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 {
		return 0, s, false
	}
	return n, strings.TrimSpace(m[3]), true
}

func withRef(c Cmd, refText string) (Cmd, error) {
	ids := RefIDs(refText)
	switch len(ids) {
	case 0:
		return Cmd{}, ErrUnrecognized
	case 1:
		c.ID = ids[0]
	default:
		c.Candidates = ids
	}
	return c, nil
}

func Parse(text, refText string, words []model.ActionWord, now time.Time) (Cmd, error) {
	t := strings.TrimSpace(text)
	op, rest, ok := matchWord(t, words)
	if !ok {
		return Cmd{}, ErrNoActionWord
	}
	rest = strings.TrimSpace(strings.TrimRight(rest, trailPunc))
	c := Cmd{Op: op}
	switch op {
	case OpList, OpUndo:
		if rest != "" {
			return Cmd{}, ErrUnrecognized
		}
		return c, nil
	case OpDone, OpCancel:
		if rest == "" {
			return withRef(c, refText)
		}
		id, tail, ok := leadingID(rest)
		if !ok || tail != "" {
			return Cmd{}, ErrUnrecognized
		}
		c.ID = id
		return c, nil
	case OpPostpone, OpReschedule:
		spec := rest
		if id, tail, ok := leadingID(rest); ok {
			c.ID, spec = id, tail
		}
		if op == OpPostpone {
			sh, ok := parseShift(spec)
			if !ok {
				return Cmd{}, ErrUnrecognized
			}
			c.Shift = sh
		} else {
			when, hasTime, err := parseWhen(spec, now)
			if err != nil {
				return Cmd{}, err
			}
			c.When, c.HasTime = when, hasTime
		}
		if c.ID == 0 {
			return withRef(c, refText)
		}
		return c, nil
	}
	return Cmd{}, ErrUnrecognized
}

func parseShift(s string) (Shift, bool) {
	switch s {
	case "明天":
		return Shift{Days: 1, Absolute: true}, true
	case "后天":
		return Shift{Days: 2, Absolute: true}, true
	case "下周":
		return Shift{Days: 7}, true
	}
	m := shiftRe.FindStringSubmatch(s)
	if m == nil {
		return Shift{}, false
	}
	n, _ := strconv.Atoi(m[1])
	if n <= 0 {
		return Shift{}, false
	}
	if m[2] == "天" || m[2] == "日" {
		return Shift{Days: n}, true
	}
	return Shift{Hours: n}, true
}

func mkDate(y, m, d int, loc *time.Location) (time.Time, bool) {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc)
	return t, t.Year() == y && int(t.Month()) == m && t.Day() == d
}

// parseWhen 解析「改到」后面的时间。不匹配任何格式 → ErrUnrecognized；匹配但数值不合法 → *DateError。
func parseWhen(s string, now time.Time) (time.Time, bool, error) {
	m := whenRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false, ErrUnrecognized
	}
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	atoi := func(x string) int { n, _ := strconv.Atoi(x); return n }
	var day time.Time
	ok := true
	switch {
	case m[1] == "今天":
		day = today
	case m[1] == "明天":
		day = today.AddDate(0, 0, 1)
	case m[1] == "后天":
		day = today.AddDate(0, 0, 2)
	case m[2] != "":
		day, ok = mkDate(atoi(m[2]), atoi(m[3]), atoi(m[4]), loc)
	default:
		mon, d := atoi(m[5]), atoi(m[6])
		if m[7] != "" {
			mon, d = atoi(m[7]), atoi(m[8])
		}
		day, ok = mkDate(now.Year(), mon, d, loc)
		if ok && day.Before(today) {
			day, ok = mkDate(now.Year()+1, mon, d, loc)
		}
	}
	if !ok {
		return time.Time{}, false, &DateError{Input: s}
	}
	if m[9] == "" {
		return day, false, nil
	}
	hh, mm := atoi(m[9]), atoi(m[10])
	if hh > 23 || mm > 59 {
		return time.Time{}, false, &DateError{Input: s}
	}
	return day.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute), true, nil
}
