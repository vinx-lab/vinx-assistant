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

	// 后处理队列：附件下载、关键词标签、抓网页标题都交给单个 worker 按入队顺序做，收件不等它们。
	// bg 计数「已入队、还没处理或丢弃」的任务，Wait 用它等队列清空。
	jobs   chan postJob
	bg     sync.WaitGroup
	ctx    context.Context // worker 用；Close 时取消，正在下载的附件随之中断
	cancel context.CancelFunc
	mu     sync.Mutex // 保护 closed，并让入队与 Close 互斥：Close 之后不会再有任务进队
	closed bool
	done   chan struct{} // worker 退出时关闭
	dlMu   sync.Mutex    // 串行化附件下载：worker 与 RetryAttachments 不会同时下同一个附件
}

// postJob 是一条消息入库后的后处理：下载附件、打标签、抓标题。
type postJob struct {
	itemID int64
	atts   []attJob
	labels []string
	url    string
	now    time.Time // 收到消息的时间，决定附件存放目录（与入库时 created_at 一致）
}

type attJob struct {
	a   *model.Attachment
	idx int
	mi  ilink.Item
}

const queueSize = 256

func New(d Deps) *Service { return newService(d, queueSize) }

func newService(d Deps, size int) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	s := &Service{Deps: d, acker: NewAcker(5*time.Second, 30*time.Second), jobs: make(chan postJob, size), done: make(chan struct{})}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	go s.worker()
	return s
}

// Wait 等已入队的后处理任务都处理完（或在 Close 后被丢弃）。
func (s *Service) Wait() { s.bg.Wait() }

// Close 停止接收后处理任务并让 worker 退出，返回时 worker 已退出。正在下载的附件被中断，
// 队列里没处理的任务直接丢弃：附件仍是 pending，之后由 RetryAttachments 补下；标签、标题不补。
// 可重复调用；之后 Handle 照常入库，只是不再做后处理。
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.done
		return
	}
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	<-s.done
}

// enqueue 把后处理任务交给 worker，从不阻塞：已关闭或队列满时放弃。
func (s *Service) enqueue(j postJob) {
	if len(j.atts) == 0 && len(j.labels) == 0 && (j.url == "" || s.Enricher == nil) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.Log.Info("后处理已停止，附件稍后由重试补下载", "item", j.itemID)
		return
	}
	s.bg.Add(1)
	select {
	case s.jobs <- j:
	default:
		s.bg.Done()
		s.Log.Warn("后处理队列已满，附件稍后由重试补下载", "item", j.itemID)
	}
}

func (s *Service) worker() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			// Close 时 closed 已置位，不会再有新任务进队；丢弃剩下的并计数完成。
			for {
				select {
				case <-s.jobs:
					s.bg.Done()
				default:
					return
				}
			}
		case j := <-s.jobs:
			s.process(j)
			s.bg.Done()
		}
	}
}

func (s *Service) process(j postJob) {
	ctx := s.ctx
	for _, aj := range j.atts {
		if ctx.Err() != nil {
			return // 关机：剩下的附件留给重试
		}
		s.downloadPending(ctx, aj.a, aj.idx, aj.mi, j.now)
	}
	if ctx.Err() != nil {
		return
	}
	s.addLabels(ctx, j.itemID, j.labels)
	if j.url != "" && s.Enricher != nil && ctx.Err() == nil {
		ectx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s.Enricher.Enrich(ectx, j.itemID, j.url)
		cancel()
	}
}

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
			return s.replay(ctx, id, f, p.Labels, now)
		}
		return err
	}
	job := postJob{itemID: it.ID, labels: p.Labels, url: it.URL, now: now}
	for i, mi := range f.media {
		if a := s.insertAttachment(ctx, it.ID, mi, now); a != nil {
			job.atts = append(job.atts, attJob{a: a, idx: i, mi: mi})
		}
	}
	s.acker.Add(now, ackLabel(it, f))
	err = s.Store.MarkSeen(ctx, id, now)
	s.enqueue(job) // 条目和附件记录已落库；即使 MarkSeen 失败也照常后处理，重放时不会重复
	return err
}

// SaveTextAsItem 按普通收件规则（前缀与关键词、标签、链接、回执）把一段文字存成条目。
// 用于后台 AI 判定「不是指令」的消息：那时消息已标记为已收，这里不再碰 seen 表；附件不处理
// （进入 AI 兜底的消息只有文字）。msg_id 已存在视为成功（重放或已存过）。
func (s *Service) SaveTextAsItem(ctx context.Context, msgID, text string) error {
	settings, err := s.Store.LoadSettings(ctx)
	if err != nil {
		return err
	}
	now := s.Clock.Now()
	p := Classify(Input{Text: text}, settings.Rules, settings.AI.Images)
	it := &model.Item{
		CreatedAt:  now.In(clock.Zone),
		MsgID:      msgID,
		RawText:    p.Text,
		URL:        enrich.ExtractURL(p.Text),
		Category:   p.Category,
		CategoryBy: p.CategoryBy,
		Level:      p.Level,
		Status:     model.DefaultStatus(p.Category),
	}
	if _, err := s.Store.InsertItem(ctx, it); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return nil
		}
		return err
	}
	s.acker.Add(now, ackLabel(it, flat{text: text}))
	s.enqueue(postJob{itemID: it.ID, labels: p.Labels, url: it.URL, now: now})
	return nil
}

// replay 处理重放（条目已在库里）：补插缺失的附件记录（按序号比对），补打标签，交给后台；
// 覆盖「条目入库后、附件记录插入前崩溃」的窗口。
func (s *Service) replay(ctx context.Context, msgID string, f flat, labels []string, now time.Time) error {
	if len(labels) > 0 || len(f.media) > 0 {
		old, err := s.Store.GetItemByMsgID(ctx, msgID)
		switch {
		case err == nil:
			job := postJob{itemID: old.ID, labels: labels, now: now}
			if len(f.media) > 0 {
				existing, err := s.Store.ListAttachments(ctx, old.ID)
				if err != nil {
					return err
				}
				for i := len(existing); i < len(f.media); i++ {
					if a := s.insertAttachment(ctx, old.ID, f.media[i], now); a != nil {
						job.atts = append(job.atts, attJob{a: a, idx: i, mi: f.media[i]})
					}
				}
			}
			s.enqueue(job)
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
	}
	return s.Store.MarkSeen(ctx, msgID, now)
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

// insertAttachment 插入一条 pending 附件记录（不下载）；失败只记日志，返回 nil。
func (s *Service) insertAttachment(ctx context.Context, itemID int64, mi ilink.Item, now time.Time) *model.Attachment {
	mj, _ := json.Marshal(mi)
	a := &model.Attachment{ItemID: itemID, Kind: mi.Kind(), State: "pending", MediaJSON: string(mj), CreatedAt: now}
	if mi.File != nil {
		a.FileName = sanitizeName(mi.File.FileName)
	}
	if _, err := s.Store.InsertAttachment(ctx, a); err != nil {
		s.Log.Error("附件入库失败", "item", itemID, "err", err)
		return nil
	}
	return a
}

// downloadPending 在下载锁内重新读取附件记录，仍是 pending 且没超次数才下载：
// worker 与 RetryAttachments 可能拿到同一个附件，后到的一方看到 ok/failed 就跳过。
func (s *Service) downloadPending(ctx context.Context, a *model.Attachment, idx int, mi ilink.Item, now time.Time) {
	s.dlMu.Lock()
	defer s.dlMu.Unlock()
	list, err := s.Store.ListAttachments(ctx, a.ItemID)
	if err != nil {
		if ctx.Err() == nil {
			s.Log.Error("读取附件记录失败", "attachment", a.ID, "err", err)
		}
		return
	}
	for i := range list {
		if list[i].ID == a.ID {
			cur := list[i]
			if cur.State != "pending" || cur.Attempts >= maxAttachmentAttempts {
				return
			}
			s.download(ctx, &cur, idx, mi, now)
			return
		}
	}
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
	if err != nil && ctx.Err() != nil {
		// 关机中断：不计次数、不写库（ctx 已取消写不进去），保持 pending 留给下次重试
		s.Log.Info("附件下载被中断，稍后重试", "item", a.ItemID)
		return
	}
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
		s.downloadPending(ctx, a, idx, mi, a.CreatedAt)
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
