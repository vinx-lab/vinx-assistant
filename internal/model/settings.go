package model

type PrefixRule struct {
	Prefix   string   `json:"prefix"`
	Category Category `json:"category"`
}

type ActionWord struct {
	Word string `json:"word"`
	Op   string `json:"op"` // done postpone reschedule cancel list undo
}

type Rules struct {
	Prefixes       []PrefixRule `json:"prefixes"`
	MediumKeywords []string     `json:"medium_keywords"`
	DeepKeywords   []string     `json:"deep_keywords"`
	ActionWords    []ActionWord `json:"action_words"`
}

type Schedule struct {
	BatchTimes []string `json:"batch_times"` // "HH:MM"
	DigestTime string   `json:"digest_time"`
}

type Provider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

type ModelRef struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

type AI struct {
	DailyTokenLimit int64      `json:"daily_token_limit"`
	Images          bool       `json:"images"`
	Providers       []Provider `json:"providers"`
	Light           ModelRef   `json:"light"`
	Medium          ModelRef   `json:"medium"`
	Deep            ModelRef   `json:"deep"`
}

func (a AI) Ref(l Level) ModelRef {
	switch l {
	case LevelMedium:
		return a.Medium
	case LevelDeep:
		return a.Deep
	}
	return a.Light
}

func (a AI) Provider(id string) (Provider, bool) {
	for _, p := range a.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

type Settings struct {
	Rules    Rules    `json:"rules"`
	Schedule Schedule `json:"schedule"`
	AI       AI       `json:"ai"`
}

func DefaultSettings() Settings {
	return Settings{
		Rules: Rules{
			Prefixes: []PrefixRule{
				{"待研究", CatResearch}, {"研究", CatResearch}, {"稍后看", CatLater},
				{"待办", CatTodo}, {"点子", CatIdea}, {"资料", CatArchive},
			},
			MediumKeywords: []string{"研究一下"},
			DeepKeywords:   []string{"深入研究"},
			ActionWords: []ActionWord{
				{"完成", "done"}, {"搞定", "done"}, {"推迟", "postpone"}, {"改到", "reschedule"},
				{"取消", "cancel"}, {"删除", "cancel"}, {"列表", "list"}, {"撤销", "undo"},
			},
		},
		Schedule: Schedule{BatchTimes: []string{"08:00", "20:00"}, DigestTime: "09:00"},
		AI:       AI{DailyTokenLimit: 200000},
	}
}
