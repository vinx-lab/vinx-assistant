package web

import (
	"net/http"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type usageDay struct {
	store.DayUsage
	Total int64
}

type usageData struct {
	Page
	Today, Limit int64
	Days         []usageDay
	Max          int64
	Top          []model.Item
	Failed       []model.Item
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := usageData{Page: s.page(r, "用量", "usage")}
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
	for _, u := range days {
		t := u.PromptTokens + u.CompletionTokens
		d.Days = append(d.Days, usageDay{DayUsage: u, Total: t})
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
