package web

import (
	"context"
	"net/http"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// usageDay 一天的用量。整理（批量/逐条，受每日上限约束）与指令（微信指令翻译，不计入上限）分开显示，口径与「今日」一致。
type usageDay struct {
	store.DayUsage
	Total, Organize, Command int64
}

type usageData struct {
	Page
	Today, TodayCommand, Limit int64 // Today 只含整理，与上限同口径
	Days                       []usageDay
	Max                        int64
	Top                        []model.Item
	Failed                     []model.Item
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := usageData{Page: s.subPage(r, "用量", "usage")}
	st, err := s.d.Store.LoadSettings(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.Limit = st.AI.DailyTokenLimit
	if d.Today, err = s.d.Store.TokensOn(ctx, clock.DayString(s.d.Clock.Now())); err != nil {
		s.fail(w, err)
		return
	}
	days, err := s.d.Store.UsageByDay(ctx, 30)
	if err != nil {
		s.fail(w, err)
		return
	}
	cmd, err := s.commandTokensByDay(ctx, 30)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.TodayCommand = cmd[clock.DayString(s.d.Clock.Now())]
	for _, u := range days {
		t := u.PromptTokens + u.CompletionTokens
		d.Days = append(d.Days, usageDay{DayUsage: u, Total: t, Command: cmd[u.Day], Organize: t - cmd[u.Day]})
		d.Max = max(d.Max, t)
	}
	if d.Top, err = s.d.Store.TopItemsByTokens(ctx, 10); err != nil {
		s.fail(w, err)
		return
	}
	if d.Failed, err = s.d.Store.FailedItems(ctx, 20); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "usage", d)
}

// commandTokensByDay 最近 limit 天里每天指令翻译用的 token（level=command）。
func (s *Server) commandTokensByDay(ctx context.Context, limit int) (map[string]int64, error) {
	rows, err := s.d.Store.DB().QueryContext(ctx, `SELECT day, SUM(prompt_tokens + completion_tokens) FROM llm_usage WHERE level = ? GROUP BY day ORDER BY day DESC LIMIT ?`,
		store.UsageLevelCommand, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var day string
		var n int64
		if err := rows.Scan(&day, &n); err != nil {
			return nil, err
		}
		out[day] = n
	}
	return out, rows.Err()
}
