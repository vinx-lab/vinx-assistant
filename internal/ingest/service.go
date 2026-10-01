package ingest

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/enrich"
	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/notify"
	"github.com/vinx-lab/vinx-assistant/internal/redact"
	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

const maxAttachmentAttempts = 5

type CommandInput struct {
	Text    string
	RefText string // 被引用消息的文字：引用自带的原文，或按 svr_id 查到的我们发过的消息、主人发过的条目；查不到为空
	HasRef  bool   // 这条消息引用了别的消息（RefText 可能为空）
	MsgID   string
}

type CommandHandler interface {
	Handle(ctx context.Context, in CommandInput) (reply string, handled bool, err error)
}

type Deferred interface {
	TakeDeferred(ctx context.Context) (text string, commit func(ctx context.Context, sent bool) error, err error)
}

type Enricher interface {
	Enrich(ctx context.Context, itemID int64, url string)
}

type Deps struct {
	Store    *store.Store
	Session  *session.Session
	Notifier notify.Notifier
	Clock    clock.Clock
	MediaDir string
	Log      *slog.Logger
	Commands CommandHandler
	Deferred Deferred
	Enricher Enricher
}

type Service struct {
	Deps
	acker *Acker
	bg    sync.WaitGroup
}

func New(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	return &Service{Deps: d, acker: NewAcker(5*time.Second, 30*time.Second)}
}

// Wait 等后台轻处理结束。
func (s *Service) Wait() { s.bg.Wait() }

type flat struct {
	text     string
	ref      *ilink.RefMessage
	hasImage bool
	hasOther bool
	media    []ilink.Item
}

func flatten(m ilink.Message) flat {
	var f flat
	var parts []string
	for _, it := range m.Items {
		if it.HasRef() {
			f.ref = it.RefMsg
		}
		switch it.Type {
		case ilink.TypeText:
			if it.Text != nil {
				parts = append(parts, it.Text.Text)
			}
		case ilink.TypeVoice:
			if it.Voice != nil && it.Voice.Text != "" {
				parts = append(parts, it.Voice.Text)
			}
			f.hasOther = true
			f.media = append(f.media, it)
		case ilink.TypeImage:
			f.hasImage = true
			f.media = append(f.media, it)
		case ilink.TypeFile:
			f.hasOther = true
			f.media = append(f.media, it)
		case ilink.TypeVideo:
			f.hasOther = true
			f.media = append(f.media, it)
		}
	}
	f.text = strings.TrimSpace(strings.Join(parts, "\n"))
	if f.text == "" {
		for _, it := range f.media {
			if it.Type == ilink.TypeFile && it.File != nil && it.File.FileName != "" {
				parts = append(parts, "[文件] "+it.File.FileName)
			}
		}
		f.text = strings.Join(parts, "\n")
	}
	return f
}

// Handle 处理一条入站消息。返回错误只表示数据库等内部故障；协议层面的「不处理」返回 nil。
func (s *Service) Handle(ctx context.Context, m ilink.Message) error {
	if m.MessageType != 1 {
		return nil
	}
	cred, _, err := s.Session.Cred(ctx)
	if err != nil {
		return err
	}
	if cred.UserID == "" {
		s.Log.Warn("凭证缺少 ilink_user_id，拒收消息，请重新登录")
		return nil
	}
	if m.FromUserID != cred.UserID {
		s.Log.Warn("忽略非主人的消息", "msg_id", m.ID())
		return nil
	}
	id := m.ID()
	if seen, err := s.Store.Seen(ctx, id); err != nil || seen {
		return err
	}
	now := s.Clock.Now()
	if m.ContextToken != "" {
		if err := s.Session.RememberContext(ctx, m.ContextToken, now); err != nil {
			return err
		}
	}
	s.noteDrift(ctx, m)
	f := flatten(m)
	settings, err := s.Store.LoadSettings(ctx)
	if err != nil {
		return err
	}

	if s.Commands != nil && IsCommand(f.text, f.ref != nil, settings.Rules.ActionWords) {
		in := CommandInput{Text: f.text, HasRef: f.ref != nil, RefText: s.resolveRef(ctx, f.ref), MsgID: id}
		reply, handled, err := s.Commands.Handle(ctx, in)
		if err != nil {
			s.Log.Error("指令执行失败", "msg_id", id, "err", err)
			reply, handled = "指令执行失败，请稍后再试", true
		}
		if handled {
			if reply != "" {
				s.Reply(ctx, reply)
			}
			return s.Store.MarkSeen(ctx, id, now)
		}
	}

	p := Classify(Input{Text: f.text, HasImage: f.hasImage, HasOther: f.hasOther}, settings.Rules, settings.AI.Images)
	created := m.CreatedAt()
	if created.IsZero() {
		created = now
	}
	it := &model.Item{
		CreatedAt:  created.In(clock.Zone),
		MsgID:      id,
		RawText:    p.Text,
		URL:        enrich.ExtractURL(p.Text),
		Category:   p.Category,
		CategoryBy: p.CategoryBy,
		Level:      p.Level,
		Status:     model.DefaultStatus(p.Category),
		RawJSON:    string(redact.JSON(m.Raw)),
	}
	if _, err := s.Store.InsertItem(ctx, it); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			// 重放路径：条目已在，补一次标签（尽力而为，AddTags 幂等合并）
			if len(p.Labels) > 0 {
				old, gerr := s.Store.GetItemByMsgID(ctx, id)
				switch {
				case gerr == nil:
					s.addLabels(ctx, old.ID, p.Labels)
				case !errors.Is(gerr, store.ErrNotFound):
					return gerr
				}
			}
			return s.Store.MarkSeen(ctx, id, now)
		}
		return err
	}
	for i, mi := range f.media {
		s.saveAttachment(ctx, it.ID, i, mi, now)
	}
	s.acker.Add(now, ackLabel(it, f))
	if it.URL != "" && s.Enricher != nil {
		s.bg.Add(1)
		go func(id int64, u string) {
			defer s.bg.Done()
			ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			s.Enricher.Enrich(ectx, id, u)
		}(it.ID, it.URL)
	}
	s.addLabels(ctx, it.ID, p.Labels)
	return s.Store.MarkSeen(ctx, id, now)
}

// addLabels 写关键词标签；标签是尽力而为，失败只记 WARN，不影响收件。
func (s *Service) addLabels(ctx context.Context, itemID int64, labels []string) {
	if len(labels) == 0 {
		return
	}
	if err := s.Store.AddTags(ctx, itemID, labels); err != nil {
		s.Log.Warn("关键词标签写入失败", "item_id", itemID, "err", err)
	}
}

// resolveRef 把引用还原成文字：先用引用自带的原文和摘要；只有 svr_id 时（新版微信），
// 查我们发过的消息，再查主人发过的条目。都查不到返回空串。
func (s *Service) resolveRef(ctx context.Context, ref *ilink.RefMessage) string {
	if ref == nil {
		return ""
	}
	if mi := ref.MessageItem; mi != nil {
		if mi.Text != nil && mi.Text.Text != "" {
			return mi.Text.Text
		}
		if mi.Voice != nil && mi.Voice.Text != "" {
			return mi.Voice.Text
		}
	}
	if ref.Title != "" {
		return ref.Title
	}
	// 实测新版引用只带 message_item{type:0,msg_id}，msg_id 即被引用消息的 message_id。
	if mi := ref.MessageItem; mi != nil && mi.MsgID != "" {
		if body, ok := s.lookupRef(ctx, mi.MsgID); ok {
			return body
		}
	}
	id := ref.SvrID.String()
	if id == "" {
		if ref.MessageItem != nil && ref.MessageItem.MsgID != "" {
			s.Log.Info("引用的消息查不到原文", "msg_id", ref.MessageItem.MsgID)
		}
		return ""
	}
	if body, ok := s.lookupRef(ctx, id); ok {
		return body
	}
	s.Log.Info("引用的消息查不到原文", "svr_id", id)
	return ""
}

// lookupRef 先查我们发过的消息，再查主人发过的条目。
func (s *Service) lookupRef(ctx context.Context, id string) (string, bool) {
	if body, ok, err := s.Store.SentBody(ctx, id); err == nil && ok {
		return body, true
	}
	if it, err := s.Store.GetItemByMsgID(ctx, id); err == nil {
		return it.RawText, true
	}
	return "", false
}

const keyDrift = "ilink.drift"

// noteDrift 记录协议之外的字段或消息类型（首次出现时打 WARN），提示去对照上游是否改了协议。
func (s *Service) noteDrift(ctx context.Context, m ilink.Message) {
	keys := ilink.Drift(m.Raw)
	if len(keys) == 0 {
		return
	}
	seen := map[string]int64{}
	if raw, ok, _ := s.Store.GetKV(ctx, keyDrift); ok {
		json.Unmarshal([]byte(raw), &seen)
	}
	changed := false
	for _, k := range keys {
		if _, ok := seen[k]; !ok {
			seen[k] = s.Clock.Now().Unix()
			changed = true
			s.Log.Warn("收到协议之外的字段，可能是上游协议有变化，请运行 make check-upstream 对照", "key", k, "msg_id", m.ID())
		}
	}
	if changed {
		b, _ := json.Marshal(seen)
		s.Store.SetKV(ctx, keyDrift, string(b))
	}
}

func ackLabel(it *model.Item, f flat) string {
	title := model.TruncateRunes(strings.TrimSpace(it.RawText), 20)
	if title == "" {
		switch {
		case f.hasImage:
			title = "图片"
		default:
			title = "附件"
		}
	}
	return model.CategoryName(it.Category) + "｜" + title
}

func (s *Service) saveAttachment(ctx context.Context, itemID int64, idx int, mi ilink.Item, now time.Time) {
	mj, _ := json.Marshal(mi)
	a := &model.Attachment{ItemID: itemID, Kind: mi.Kind(), State: "pending", MediaJSON: string(mj), CreatedAt: now}
	if mi.File != nil {
		a.FileName = sanitizeName(mi.File.FileName)
	}
	if _, err := s.Store.InsertAttachment(ctx, a); err != nil {
		s.Log.Error("附件入库失败", "item", itemID, "err", err)
		return
	}
	s.download(ctx, a, idx, mi, now)
}

// download 下载、校验、落盘并更新附件记录；失败只记录，留给 RetryAttachments。
func (s *Service) download(ctx context.Context, a *model.Attachment, idx int, mi ilink.Item, now time.Time) {
	skipped := false // 暂停或没凭证：没有真正尝试，不计次数
	attempts := a.Attempts
	a.Attempts++
	err := func() error {
		c, err := s.Session.Client(ctx)
		if err != nil {
			if errors.Is(err, session.ErrPaused) || errors.Is(err, session.ErrNoCred) {
				skipped = true
			}
			return err
		}
		data, err := c.Download(ctx, mi)
		if err != nil {
			return err
		}
		sum := md5.Sum(data)
		a.MD5 = hex.EncodeToString(sum[:])
		if mi.File != nil && mi.File.MD5 != "" && !strings.EqualFold(mi.File.MD5, a.MD5) {
			return errors.New("MD5 与消息里的不一致")
		}
		rel, err := saveMedia(s.MediaDir, now, a.ItemID, idx, a.Kind, a.FileName, data)
		if err != nil {
			return err
		}
		a.RelPath, a.Size, a.State, a.LastError = rel, int64(len(data)), "ok", ""
		return nil
	}()
	if err != nil && skipped {
		a.Attempts = attempts
		a.LastError = err.Error()
		s.Log.Info("附件下载推迟（暂停或无凭证）", "item", a.ItemID, "err", err)
	} else if err != nil {
		a.LastError = err.Error()
		if a.Attempts >= maxAttachmentAttempts || errors.Is(err, ilink.ErrNoMediaKey) {
			a.State = "failed" // 缺密钥重试也没用（上游直接跳过）
		}
		s.Log.Warn("附件下载失败", "item", a.ItemID, "attempt", a.Attempts, "err", err)
	}
	if err := s.Store.UpdateAttachment(ctx, a); err != nil {
		s.Log.Error("附件状态更新失败", "attachment", a.ID, "err", err)
	}
}

// RetryAttachments 重试下载失败的附件。附件序号取它在同一条目下的位置。
func (s *Service) RetryAttachments(ctx context.Context) {
	list, err := s.Store.RetryableAttachments(ctx, maxAttachmentAttempts)
	if err != nil {
		s.Log.Error("查询待重试附件失败", "err", err)
		return
	}
	for i := range list {
		a := &list[i]
		var mi ilink.Item
		if err := json.Unmarshal([]byte(a.MediaJSON), &mi); err != nil {
			a.State, a.LastError = "failed", "media_json 无法解析"
			s.Store.UpdateAttachment(ctx, a)
			continue
		}
		siblings, _ := s.Store.ListAttachments(ctx, a.ItemID)
		idx := 0
		for j, sib := range siblings {
			if sib.ID == a.ID {
				idx = j
			}
		}
		s.download(ctx, a, idx, mi, a.CreatedAt)
	}
}

// FlushAcks 到点就发合并回执。
func (s *Service) FlushAcks(ctx context.Context) {
	if text, ok := s.acker.Due(s.Clock.Now()); ok {
		s.Reply(ctx, text)
	}
}

// FlushAllAcks 忽略静默期，立即发出已积累的回执（关机时调用，避免丢掉）。
func (s *Service) FlushAllAcks(ctx context.Context) {
	if text, ok := s.acker.Flush(); ok {
		s.Reply(ctx, text)
	}
}

// Reply 立即发一条消息，并把待补发的提醒附在后面。
func (s *Service) Reply(ctx context.Context, text string) {
	var commit func(context.Context, bool) error
	if s.Deferred != nil {
		extra, c, err := s.Deferred.TakeDeferred(ctx)
		if err != nil {
			s.Log.Error("读取待补发提醒失败", "err", err)
		} else if extra != "" {
			text += "\n\n" + extra
			commit = c
		}
	}
	err := s.Notifier.Send(ctx, text)
	if err != nil {
		s.Log.Warn("回复发送失败", "err", err)
	}
	if commit != nil {
		if cerr := commit(ctx, err == nil); cerr != nil {
			s.Log.Error("更新补发状态失败", "err", cerr)
		}
	}
}
