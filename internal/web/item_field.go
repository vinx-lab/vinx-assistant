package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// itemView 是按字段保存后返回给前端的条目当前状态，前端据此局部更新显示。
type itemView struct {
	ID           int64    `json:"id"`
	Display      string   `json:"display"` // 显示的标题（不含编号）
	Title        string   `json:"title"`   // 手动标题；空表示由 AI 生成
	Category     string   `json:"category"`
	CategoryName string   `json:"category_name"`
	Status       string   `json:"status"`
	StatusName   string   `json:"status_name"`
	Priority     string   `json:"priority"`
	PriorityName string   `json:"priority_name"`
	Due          string   `json:"due"` // 显示文本，如「10-08 周四 15:00」；空表示没有截止时间
	DueDate      string   `json:"due_date"`
	DueTime      string   `json:"due_time"`
	Overdue      bool     `json:"overdue"`
	Labels       []string `json:"labels"`
	Topics       []string `json:"topics"`
}

func (s *Server) viewOf(it *model.Item) itemView {
	v := itemView{ID: it.ID, Display: it.DisplayTitle(), Title: it.Title, Category: string(it.Category), CategoryName: model.CategoryName(it.Category),
		Status: it.Status, StatusName: model.StatusName(it.Status), Priority: string(it.Priority), PriorityName: priorityNames[it.Priority],
		Labels: it.Labels, Topics: it.Topics, Overdue: isOverdue(*it, s.d.Clock.Now())}
	if it.DueAt != nil {
		now := s.d.Clock.Now()
		v.Due = model.FormatDue(*it.DueAt, it.DueHasTime, now)
		v.DueDate = it.DueAt.In(clock.Zone).Format("2006-01-02")
		if it.DueHasTime {
			v.DueTime = it.DueAt.In(clock.Zone).Format("15:04")
		}
	}
	if v.Labels == nil {
		v.Labels = []string{}
	}
	if v.Topics == nil {
		v.Topics = []string{}
	}
	return v
}

// itemField 是详情页原地编辑：一次只改一个字段（field=title|category|priority|due|labels|topics）。
// 只改这一个字段、在事务里读改写，不会覆盖别处同时改的其他字段，所以不需要整表保存的 updated_at 检查。
// 成功返回 {"item": 当前状态}；校验失败 400、条目不存在 404，都返回 {"error": "..."}。
func (s *Server) itemField(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "条目不存在"})
		return
	}
	ctx := r.Context()
	field := r.FormValue("field")
	switch field {
	case "labels", "topics":
		kind := store.TagKindLabel
		if field == "topics" {
			kind = store.TagKindTopic
		}
		if _, err = s.d.Store.GetItem(ctx, id); err == nil {
			err = s.d.Store.SetTags(ctx, id, kind, model.NormalizeTags(splitTags(r.FormValue(field))))
		}
	case "title", "category", "priority", "due":
		_, err = s.d.Store.ModifyItem(ctx, id, func(it *model.Item) error {
			switch field {
			case "title":
				setTitle(it, r.FormValue("title"))
			case "category":
				cat, err := parseCategory(it, r.FormValue("category"))
				if err != nil {
					return err
				}
				setCategory(it, cat)
			case "priority":
				prio, err := parsePriority(r.FormValue("priority"))
				if err != nil {
					return err
				}
				it.Priority = prio
			case "due":
				due, hasTime, err := parseDue(r.FormValue("due_date"), r.FormValue("due_time"))
				if err != nil {
					return err
				}
				it.DueAt, it.DueHasTime = due, hasTime
			}
			return nil
		})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不认识的字段"})
		return
	}
	var bad badInput
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "条目不存在"})
		return
	case errors.As(err, &bad):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": bad.msg})
		return
	case err != nil:
		s.d.Log.Error("按字段保存条目失败", "id", id, "field", field, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存失败，详见服务日志"})
		return
	}
	it, err := s.d.Store.GetItem(ctx, id)
	if err != nil {
		s.d.Log.Error("读取条目失败", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存后读取失败，请刷新"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]itemView{"item": s.viewOf(it)})
}
