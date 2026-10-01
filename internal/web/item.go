package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type editForm struct {
	Category, Tags, DueDate, DueTime, Priority string
}

func splitTags(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ' ' || r == '\t' || r == '\n'
	})
}

// applyEdit 把详情页表单写进条目，返回规范化后的标签。出错时不修改条目。
// 不允许手动把已整理的条目改回「未整理」：批处理会一直重新处理它（计划 2 评审结论）。
func applyEdit(it *model.Item, f editForm) ([]string, error) {
	cat := model.Category(f.Category)
	if !model.ValidCategory(cat) {
		return nil, badInput{"分类不对"}
	}
	if cat == model.CatInbox && it.Category != model.CatInbox {
		return nil, badInput{"不能手动改回未整理"}
	}
	prio := model.Priority(f.Priority)
	if _, ok := priorityNames[prio]; !ok {
		return nil, badInput{"优先级不对"}
	}
	var due *time.Time
	hasTime := false
	switch {
	case f.DueDate == "" && f.DueTime != "":
		return nil, badInput{"填了时刻就要填日期"}
	case f.DueDate != "":
		d, err := time.ParseInLocation("2006-01-02", f.DueDate, clock.Zone)
		if err != nil {
			return nil, badInput{"日期格式不对"}
		}
		if f.DueTime != "" {
			hm, err := time.ParseInLocation("15:04", f.DueTime, clock.Zone)
			if err != nil {
				return nil, badInput{"时刻格式不对"}
			}
			d = d.Add(time.Duration(hm.Hour())*time.Hour + time.Duration(hm.Minute())*time.Minute)
			hasTime = true
		}
		due = &d // 只有日期时就是当天 00:00（全局约定）
	}
	if cat != it.Category {
		it.Category, it.CategoryBy = cat, model.ByManual
		if !model.ValidStatus(cat, it.Status) {
			it.Status = model.DefaultStatus(cat)
		}
	}
	it.Priority, it.DueAt, it.DueHasTime = prio, due, hasTime
	return model.NormalizeTags(splitTags(f.Tags)), nil
}

type itemData struct {
	Page
	Item       *model.Item
	Atts       []model.Attachment
	Categories []model.Category
	Priorities []model.Priority
	Form       editForm
}

func formOf(it *model.Item) editForm {
	f := editForm{Category: string(it.Category), Tags: strings.Join(it.Tags, "、"), Priority: string(it.Priority)}
	if it.DueAt != nil {
		f.DueDate = it.DueAt.In(clock.Zone).Format("2006-01-02")
		if it.DueHasTime {
			f.DueTime = it.DueAt.In(clock.Zone).Format("15:04")
		}
	}
	return f
}

// editCategories 是分类下拉的选项：「未整理」只在条目本来就是未整理时出现（保持不变用）。
func editCategories(cur model.Category) []model.Category {
	var out []model.Category
	for _, c := range model.Categories {
		if c != model.CatInbox || cur == model.CatInbox {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) renderItem(w http.ResponseWriter, r *http.Request, status int, it *model.Item, f editForm, errMsg string) {
	atts, err := s.d.Store.ListAttachments(r.Context(), it.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	d := itemData{Page: s.page(r, it.DisplayTitle(), "board"), Item: it, Atts: atts, Form: f,
		Categories: editCategories(it.Category),
		Priorities: []model.Priority{model.PriorityNone, model.PriorityHigh, model.PriorityMedium, model.PriorityLow}}
	d.Error = errMsg
	s.render(w, status, "item", d)
}

func (s *Server) itemPage(w http.ResponseWriter, r *http.Request) {
	it, ok := s.loadItem(w, r)
	if !ok {
		return
	}
	s.renderItem(w, r, http.StatusOK, it, formOf(it), "")
}

func (s *Server) itemSave(w http.ResponseWriter, r *http.Request) {
	id, ok := s.itemID(w, r)
	if !ok {
		return
	}
	f := editForm{Category: r.FormValue("category"), Tags: r.FormValue("tags"), DueDate: r.FormValue("due_date"),
		DueTime: r.FormValue("due_time"), Priority: r.FormValue("priority")}
	var tags []string
	_, err := s.d.Store.ModifyItem(r.Context(), id, func(it *model.Item) error {
		var err error
		tags, err = applyEdit(it, f)
		return err
	})
	var bad badInput
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.As(err, &bad):
		orig, ok := s.loadItem(w, r)
		if ok {
			s.renderItem(w, r, http.StatusBadRequest, orig, f, bad.msg)
		}
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	if err := s.d.Store.SetTags(r.Context(), id, tags); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, withMsg(r.URL.Path, "saved"))
}
