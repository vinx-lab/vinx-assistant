package web

import (
	"net/http"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type searchData struct {
	Page
	Q, Cat, Status, From, To string
	Categories               []model.Category
	Statuses                 []string
	Items                    []model.Item
	Searched                 bool
}

// parseSearch 把表单变成查询条件；日期按上海时间，「到」那天包含在内（store 的 To 不含，所以加一天）。
func parseSearch(q, cat, status, from, to string) (store.SearchQuery, error) {
	sq := store.SearchQuery{Q: q, Status: status}
	if model.ValidCategory(model.Category(cat)) {
		sq.Category = model.Category(cat)
	}
	if from != "" {
		t, err := time.ParseInLocation("2006-01-02", from, clock.Zone)
		if err != nil {
			return sq, err
		}
		sq.From = t
	}
	if to != "" {
		t, err := time.ParseInLocation("2006-01-02", to, clock.Zone)
		if err != nil {
			return sq, err
		}
		sq.To = t.AddDate(0, 0, 1)
	}
	return sq, nil
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	d := searchData{Page: s.page(r, "搜索", "search"), Q: v.Get("q"), Cat: v.Get("cat"), Status: v.Get("status"),
		From: v.Get("from"), To: v.Get("to"), Categories: model.Categories, Statuses: statusOrder}
	if d.Q == "" && d.Cat == "" && d.Status == "" && d.From == "" && d.To == "" {
		s.render(w, http.StatusOK, "search", d)
		return
	}
	sq, err := parseSearch(d.Q, d.Cat, d.Status, d.From, d.To)
	if err != nil {
		d.Error = "日期格式不对，应为 YYYY-MM-DD"
		s.render(w, http.StatusBadRequest, "search", d)
		return
	}
	if d.Items, err = s.d.Store.Search(r.Context(), sq); err != nil {
		s.fail(w, err)
		return
	}
	d.Searched = true
	s.render(w, http.StatusOK, "search", d)
}
