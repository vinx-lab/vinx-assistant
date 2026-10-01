// Package model 是各包共用的纯数据类型，不依赖数据库和网络。
package model

import (
	"fmt"
	"strings"
	"time"
)

type Category string

const (
	CatInbox    Category = "inbox"
	CatResearch Category = "research"
	CatLater    Category = "later"
	CatTodo     Category = "todo"
	CatIdea     Category = "idea"
	CatArchive  Category = "archive"
)

// Categories 是看板标签页的顺序。
var Categories = []Category{CatTodo, CatResearch, CatLater, CatIdea, CatArchive, CatInbox}

var categoryNames = map[Category]string{
	CatInbox: "未整理", CatResearch: "待研究", CatLater: "稍后看",
	CatTodo: "待办", CatIdea: "点子", CatArchive: "资料",
}

func CategoryName(c Category) string {
	if n, ok := categoryNames[c]; ok {
		return n
	}
	return string(c)
}

func ValidCategory(c Category) bool { _, ok := categoryNames[c]; return ok }

type CategoryBy string

const (
	ByPrefix CategoryBy = "prefix"
	ByAI     CategoryBy = "ai"
	ByManual CategoryBy = "manual"
)

type Level string

const (
	LevelLight  Level = "light"
	LevelMedium Level = "medium"
	LevelDeep   Level = "deep"
)

func (l Level) Rank() int {
	switch l {
	case LevelLight:
		return 1
	case LevelMedium:
		return 2
	case LevelDeep:
		return 3
	}
	return 0
}

type Priority string

const (
	PriorityNone   Priority = ""
	PriorityHigh   Priority = "high"
	PriorityMedium Priority = "medium"
	PriorityLow    Priority = "low"
)

const (
	StatusNew       = "new"
	StatusDoing     = "doing"
	StatusDone      = "done"
	StatusDropped   = "dropped"
	StatusRead      = "read"
	StatusOpen      = "open"
	StatusCancelled = "cancelled"
	StatusKept      = "kept"
)

var validStatus = map[Category][]string{
	CatInbox:    {StatusNew},
	CatResearch: {StatusNew, StatusDoing, StatusDone, StatusDropped},
	CatLater:    {StatusNew, StatusRead},
	CatTodo:     {StatusOpen, StatusDone, StatusCancelled},
	CatIdea:     {StatusKept},
	CatArchive:  {StatusKept},
}

// DefaultStatus 是条目进入某个分类时的初始状态。
func DefaultStatus(c Category) string { return validStatus[c][0] }

func ValidStatus(c Category, s string) bool {
	for _, v := range validStatus[c] {
		if v == s {
			return true
		}
	}
	return false
}

// IsOpen 表示条目还没处理完（看板默认只显示这些）。
func IsOpen(c Category, s string) bool {
	switch c {
	case CatResearch:
		return s == StatusNew || s == StatusDoing
	case CatLater, CatInbox:
		return s == StatusNew
	case CatTodo:
		return s == StatusOpen
	}
	return false
}

type Item struct {
	ID              int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	MsgID           string
	RawText         string
	URL             string
	LinkTitle       string
	LinkDesc        string
	Category        Category
	CategoryBy      CategoryBy
	Level           Level
	Status          string
	Title           string
	Summary         string
	Detail          string
	Priority        Priority
	DueAt           *time.Time
	DueHasTime      bool
	ProcessedLevel  Level
	ProcessError    string
	ProcessAttempts int
	TokensUsed      int64
	RawJSON         string
	Tags            []string
}

// DisplayTitle 依次取标题、网页标题、原文首行前 30 个字，都没有时用编号。
func (it *Item) DisplayTitle() string {
	for _, s := range []string{it.Title, it.LinkTitle} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	if t := strings.TrimSpace(it.RawText); t != "" {
		first, _, _ := strings.Cut(t, "\n")
		return TruncateRunes(first, 30)
	}
	return fmt.Sprintf("#%d", it.ID)
}

// TruncateRunes 按字符截断，截断时补「…」。
func TruncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

type Attachment struct {
	ID        int64
	ItemID    int64
	Kind      string // image voice file video
	RelPath   string // 相对 <data>/media
	FileName  string
	Size      int64
	MD5       string
	State     string // ok pending failed
	Attempts  int
	LastError string
	MediaJSON string // 重试下载用的 ilink.Item JSON
	CreatedAt time.Time
}
