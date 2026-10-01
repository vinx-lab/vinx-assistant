package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

// ListQuery 是看板的条目查询。
type ListQuery struct {
	Category model.Category
	Tag      string // 空 = 不按标签筛选
	Done     bool   // false = 默认视图（boardOpenCond）；true = 已处理（boardDoneCond，只对 HasDoneView 的分类有意义）
	Limit    int
}

// openCond 与 model.IsOpen 一致，写成 SQL 条件。
const openCond = `((category = 'research' AND status IN ('new','doing'))
	OR (category IN ('later','inbox') AND status = 'new')
	OR (category = 'todo' AND status = 'open'))`

// boardOpenCond 是看板默认视图：没处理完的，再加上点子和资料——它们是参考材料，状态总是 kept，没有「处理完」一说。
// 提醒和每日摘要仍按 model.IsOpen，不受影响。
const boardOpenCond = `(` + openCond + ` OR category IN ('idea','archive'))`

// boardDoneCond 是看板「已处理」视图：待办的完成/取消、研究的完成/放弃、稍后看的已读。
const boardDoneCond = `((category = 'todo' AND status IN ('done','cancelled'))
	OR (category = 'research' AND status IN ('done','dropped'))
	OR (category = 'later' AND status = 'read'))`

// HasDoneView 报告分类有没有「已处理」视图（点子、资料、未整理没有）。
func HasDoneView(c model.Category) bool {
	return c == model.CatTodo || c == model.CatResearch || c == model.CatLater
}

// ListItems 返回某个分类的条目（含标签）。待办按截止时间排在前面，其余按最新收到排。
func (s *Store) ListItems(ctx context.Context, q ListQuery) ([]model.Item, error) {
	if q.Limit <= 0 {
		q.Limit = 200
	}
	where := []string{"category = ?"}
	args := []any{q.Category}
	if q.Done {
		where = append(where, boardDoneCond)
	} else {
		where = append(where, boardOpenCond)
	}
	if q.Tag != "" {
		where = append(where, `id IN (SELECT x.item_id FROM item_tags x JOIN tags t ON t.id = x.tag_id WHERE t.name = ?)`)
		args = append(args, q.Tag)
	}
	order := "id DESC"
	if q.Category == model.CatTodo {
		order = "due_at IS NULL, due_at, id DESC"
	}
	args = append(args, q.Limit)
	items, err := s.queryItems(ctx, `SELECT `+itemCols+` FROM items WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return items, s.fillTags(ctx, items)
}

func (s *Store) fillTags(ctx context.Context, items []model.Item) error {
	for i := range items {
		tags, err := loadTags(ctx, s.db, items[i].ID)
		if err != nil {
			return err
		}
		items[i].Tags = tags
	}
	return nil
}

// BoardCounts 返回各分类在看板两个视图里的条目数：open 是默认视图（boardOpenCond），done 是已处理视图（boardDoneCond）。
func (s *Store) BoardCounts(ctx context.Context) (open, done map[model.Category]int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT category,
		COALESCE(SUM(CASE WHEN `+boardOpenCond+` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN `+boardDoneCond+` THEN 1 ELSE 0 END), 0)
		FROM items GROUP BY category`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	open, done = map[model.Category]int{}, map[model.Category]int{}
	for rows.Next() {
		var c string
		var o, d int
		if err := rows.Scan(&c, &o, &d); err != nil {
			return nil, nil, err
		}
		open[model.Category(c)], done[model.Category(c)] = o, d
	}
	return open, done, rows.Err()
}

// SearchQuery 是搜索页的条件；零值字段不参与筛选。
type SearchQuery struct {
	Q        string
	Category model.Category
	Status   string
	From, To time.Time // 按收到时间，To 不含
	Limit    int
}

// FTSPhrase 把用户输入变成 FTS5 的短语查询：整体用双引号包住，内部双引号翻倍，
// 这样 AND、OR、*、括号、冒号等都按字面匹配，不会被当成查询语法。
func FTSPhrase(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

// likePattern 转义 LIKE 的 % _ \，用 \ 作转义符。
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// Search 全文检索。trigram 分词要求至少 3 个字符；更短的（如两个字的「发票」）改用 items_fts 上的 LIKE。
func (s *Store) Search(ctx context.Context, q SearchQuery) ([]model.Item, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	var where []string
	var args []any
	text := strings.TrimSpace(q.Q)
	switch n := utf8.RuneCountInString(text); {
	case n >= 3:
		where = append(where, `id IN (SELECT rowid FROM items_fts WHERE items_fts MATCH ?)`)
		args = append(args, FTSPhrase(text))
	case n > 0:
		like := likePattern(text)
		where = append(where, `id IN (SELECT rowid FROM items_fts WHERE title LIKE ? ESCAPE '\' OR raw_text LIKE ? ESCAPE '\' OR summary LIKE ? ESCAPE '\' OR detail LIKE ? ESCAPE '\' OR tags LIKE ? ESCAPE '\' OR link_title LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like, like, like)
	}
	if q.Category != "" {
		where = append(where, "category = ?")
		args = append(args, q.Category)
	}
	if q.Status != "" {
		where = append(where, "status = ?")
		args = append(args, q.Status)
	}
	if !q.From.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, q.From.Unix())
	}
	if !q.To.IsZero() {
		where = append(where, "created_at < ?")
		args = append(args, q.To.Unix())
	}
	sqlq := `SELECT ` + itemCols + ` FROM items`
	if len(where) > 0 {
		sqlq += ` WHERE ` + strings.Join(where, " AND ")
	}
	args = append(args, q.Limit)
	items, err := s.queryItems(ctx, sqlq+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return items, s.fillTags(ctx, items)
}

// AttachmentByPath 按相对路径找附件（媒体下载时决定文件名和打开方式）。
func (s *Store) AttachmentByPath(ctx context.Context, rel string) (*model.Attachment, error) {
	list, err := s.queryAttachments(ctx, `SELECT `+attCols+` FROM attachments WHERE rel_path = ? LIMIT 1`, rel)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// FirstImages 返回每个条目第一张已下载的图片（看板缩略图用）。
func (s *Store) FirstImages(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	for _, id := range ids {
		var rel string
		err := s.db.QueryRowContext(ctx, `SELECT rel_path FROM attachments WHERE item_id = ? AND kind = 'image' AND state = 'ok' ORDER BY id LIMIT 1`, id).Scan(&rel)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = rel
	}
	return out, nil
}
