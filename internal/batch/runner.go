package batch

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/enrich"
	"github.com/vinx-lab/vinx-assistant/internal/llm"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

const (
	MaxAttempts        = 3
	LightBatchSize     = 15
	MediumContentRunes = 6000
	DeepContentRunes   = 20000

	lightTextRunes  = 2000
	singleTextRunes = 4000
	maxImages       = 3
	maxImageBytes   = 5 << 20
	maxPromptTags   = 100
	leaseKey        = "batch.lease"
	// 整理期间每 leaseRenewEvery 续期一次，租约只需比几次心跳长；进程崩溃后最多等 leaseTTL 就能再整理。
	leaseTTL        = 10 * time.Minute
	leaseRenewEvery = 2 * time.Minute
)

// errLeaseLost 是心跳发现租约已被别的进程接管时取消整理的原因。
var errLeaseLost = errors.New("整理租约被其他进程接管，本次整理中止")

type Report struct {
	Processed int
	Failed    int
	Skipped   int
	Queued    int
	Waiting   int
	Tokens    int64
	Notes     []string
	Busy      bool
}

func (rep Report) String() string {
	if rep.Busy {
		return "已有整理在进行，本次跳过"
	}
	s := fmt.Sprintf("整理完成：处理 %d 条，失败 %d 条，无需 AI %d 条，超出今日 token 上限留到下次 %d 条，档位不可用未处理 %d 条，消耗 %d token",
		rep.Processed, rep.Failed, rep.Skipped, rep.Queued, rep.Waiting, rep.Tokens)
	for _, n := range rep.Notes {
		s += "\n- " + n
	}
	return s
}

type Runner struct {
	Store    *store.Store
	Clock    clock.Clock
	Fetcher  *enrich.Fetcher
	MediaDir string
	NewLLM   func(p model.Provider) llm.Chatter // 测试注入；nil 时用 llm.New
	Log      *slog.Logger

	mu         sync.Mutex
	running    atomic.Bool
	renewEvery time.Duration // 测试用；0 表示 leaseRenewEvery
}

func (r *Runner) Running() bool { return r.running.Load() }

func (r *Runner) log() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}

func (r *Runner) now() time.Time {
	if r.Clock == nil {
		return clock.Real{}.Now()
	}
	return r.Clock.Now()
}

func (r *Runner) chatterFor(p model.Provider) llm.Chatter {
	if r.NewLLM != nil {
		return r.NewLLM(p)
	}
	return llm.New(p.BaseURL, p.APIKey, nil)
}

// Run 跑一次整理。已有整理在进行（本进程或另一个进程）时返回 Report{Busy: true}。
// ctx 被取消时停止后续调用、不给任何条目记失败，返回已完成部分的 Report 和 ctx 的错误。
func (r *Runner) Run(ctx context.Context) (Report, error) {
	if !r.mu.TryLock() {
		return Report{Busy: true}, nil
	}
	defer r.mu.Unlock()
	now := r.now()
	owner := newOwner()
	ok, err := r.Store.TryLease(ctx, leaseKey, owner, now, leaseTTL)
	if err != nil {
		return Report{}, err
	}
	if !ok {
		return Report{Busy: true}, nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stopHeartbeat := r.heartbeat(ctx, owner, cancel)
	defer func() {
		stopHeartbeat() // 先停心跳，再释放，避免释放后又被续上
		if err := r.Store.ReleaseLease(context.WithoutCancel(ctx), leaseKey, owner); err != nil {
			r.log().Error("释放整理租约失败", "err", err)
		}
	}()
	r.running.Store(true)
	defer r.running.Store(false)

	settings, err := r.Store.LoadSettings(ctx)
	if err != nil {
		return Report{}, err
	}
	pending, err := r.Store.PendingForBatch(ctx, MaxAttempts)
	if err != nil {
		return Report{}, err
	}
	day := clock.DayString(now)
	used, err := r.Store.TokensOn(ctx, day)
	if err != nil {
		return Report{}, err
	}
	tagCounts, err := r.Store.AllTags(ctx)
	if err != nil {
		return Report{}, err
	}
	tags := make([]string, 0, min(len(tagCounts), maxPromptTags))
	for _, tc := range tagCounts {
		if len(tags) == maxPromptTags {
			break
		}
		tags = append(tags, tc.Name)
	}

	ru := &run{
		r: r, ctx: ctx, wctx: context.WithoutCancel(ctx), now: now, day: day, settings: settings, tags: tags,
		budget:   &Budget{Limit: settings.AI.DailyTokenLimit, Used: used},
		rep:      &Report{},
		stopped:  map[model.Level]bool{},
		chatters: map[model.Level]levelChatter{},
	}
	var light, single []*model.Item
	atts := map[int64][]model.Attachment{}
	for i := range pending {
		if ru.cancelled() {
			break
		}
		it := &pending[i]
		imgs := ru.imageAtts(it)
		if strings.TrimSpace(it.RawText) == "" && it.URL == "" && len(imgs) == 0 {
			ru.skip(it)
			continue
		}
		atts[it.ID] = imgs
		if it.Level == model.LevelLight && len(imgs) == 0 {
			light = append(light, it)
		} else {
			single = append(single, it)
		}
	}
	ru.lightAll(light)
	for _, it := range single {
		if ru.cancelled() {
			break
		}
		ru.single(it, atts[it.ID])
	}
	if ctx.Err() != nil {
		err := context.Cause(ctx)
		ru.rep.Notes = append(ru.rep.Notes, "整理被中途取消，没处理到的条目下次再整理")
		r.log().Warn("AI 整理被取消", "cause", err, "report", ru.rep.String())
		return *ru.rep, err
	}
	r.log().Info("AI 整理结束", "report", ru.rep.String())
	return *ru.rep, nil
}

func newOwner() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// heartbeat 定期续期租约；发现租约已不属于本次整理时取消整理。返回的函数停止心跳并等它退出。
func (r *Runner) heartbeat(ctx context.Context, owner string, cancel context.CancelCauseFunc) (stop func()) {
	every := r.renewEvery
	if every <= 0 {
		every = leaseRenewEvery
	}
	hbCtx, hbCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
			}
			ok, err := r.Store.RenewLease(hbCtx, leaseKey, owner, r.now(), leaseTTL)
			switch {
			case hbCtx.Err() != nil:
				return
			case err != nil:
				r.log().Error("续期整理租约失败", "err", err)
			case !ok:
				r.log().Error("整理租约已被接管，中止本次整理")
				cancel(errLeaseLost)
				return
			}
		}
	}()
	return func() {
		hbCancel()
		<-done
	}
}

type levelChatter struct {
	ch    llm.Chatter
	prov  model.Provider
	model string
}

type run struct {
	r        *Runner
	ctx      context.Context // 控制是否继续发起新的抓取和调用
	wctx     context.Context // 写库用：调用已完成、token 已花掉时，即使被取消也要把结果和用量记下来
	now      time.Time
	day      string
	settings model.Settings
	tags     []string
	budget   *Budget
	rep      *Report
	stopped  map[model.Level]bool
	chatters map[model.Level]levelChatter
}

func (ru *run) cancelled() bool { return ru.ctx.Err() != nil }

// fits 判断一次请求放不放得进剩余预算。Estimate 对中文可能偏低，比较时留 1.5 倍余量。
func (ru *run) fits(est int64) bool { return ru.budget.Fits(est + est/2) }

func (ru *run) stop(level model.Level, note string) {
	if ru.stopped[level] {
		return
	}
	ru.stopped[level] = true
	ru.rep.Notes = append(ru.rep.Notes, note)
}

func (ru *run) chatter(level model.Level) (levelChatter, bool) {
	if ru.stopped[level] {
		return levelChatter{}, false
	}
	if c, ok := ru.chatters[level]; ok {
		return c, true
	}
	ref := ru.settings.AI.Ref(level)
	p, ok := ru.settings.AI.Provider(ref.ProviderID)
	if !ok || ref.Model == "" || p.BaseURL == "" {
		ru.stop(level, levelName(level)+"档还没有配置服务商和模型，相关条目这次没有处理")
		return levelChatter{}, false
	}
	c := levelChatter{ch: ru.r.chatterFor(p), prov: p, model: ref.Model}
	ru.chatters[level] = c
	return c, true
}

func (ru *run) request(level model.Level, items []promptItem, images []string, modelName string) llm.Request {
	return buildRequest(level, ru.now, ru.tags, items, images, modelName)
}

func pitemsOf(items []*model.Item, maxText int) []promptItem {
	out := make([]promptItem, len(items))
	for i, it := range items {
		out[i] = toPromptItem(it, maxText)
	}
	return out
}

func (ru *run) lightEstimate(items []*model.Item) int64 {
	return Estimate(ru.request(model.LevelLight, pitemsOf(items, lightTextRunes), nil, ""))
}

// lightAll 把轻量条目按「最多 15 条、不超剩余预算」装包发出。单条都超预算的留到下次。
func (ru *run) lightAll(items []*model.Item) {
	if len(items) == 0 || ru.cancelled() {
		return
	}
	if _, ok := ru.chatter(model.LevelLight); !ok {
		ru.rep.Waiting += len(items)
		return
	}
	var chunk []*model.Item
	for i, it := range items {
		cand := append(append([]*model.Item(nil), chunk...), it)
		if len(cand) > LightBatchSize || !ru.fits(ru.lightEstimate(cand)) {
			if len(chunk) > 0 {
				ru.call(model.LevelLight, chunk, pitemsOf(chunk, lightTextRunes), nil)
				chunk = nil
				if ru.cancelled() {
					return
				}
				if ru.stopped[model.LevelLight] {
					ru.rep.Waiting += len(items) - i
					return
				}
			}
			cand = []*model.Item{it}
			if !ru.fits(ru.lightEstimate(cand)) {
				ru.rep.Queued++
				continue
			}
		}
		chunk = cand
	}
	if len(chunk) > 0 {
		ru.call(model.LevelLight, chunk, pitemsOf(chunk, lightTextRunes), nil)
	}
}

// single 逐条处理中等、深度和带图片的条目。
func (ru *run) single(it *model.Item, atts []model.Attachment) {
	level := it.Level
	if _, ok := ru.chatter(level); !ok {
		ru.rep.Waiting++
		return
	}
	pi := toPromptItem(it, singleTextRunes)
	if it.URL != "" && level != model.LevelLight && ru.r.Fetcher != nil {
		limit := MediumContentRunes
		if level == model.LevelDeep {
			limit = DeepContentRunes
		}
		if c, err := ru.r.Fetcher.Content(ru.ctx, it.URL, limit); err != nil {
			pi.ContentNote = "正文抓取失败：" + err.Error()
		} else {
			pi.Content = c
		}
	}
	if ru.cancelled() {
		return
	}
	images := ru.loadImages(atts)
	if len(images) == 0 && strings.TrimSpace(it.RawText) == "" && it.URL == "" {
		// 附件记录在，但图片文件读不到：没有可交给 AI 的内容。
		ru.skip(it)
		return
	}
	pitems := []promptItem{pi}
	if !ru.fits(Estimate(ru.request(level, pitems, images, ""))) {
		ru.rep.Queued++
		return
	}
	ru.call(level, []*model.Item{it}, pitems, images)
}

// call 发一次请求（格式不对时重试 1 次），记用量，把结果写回条目。
func (ru *run) call(level model.Level, items []*model.Item, pitems []promptItem, images []string) {
	lc, ok := ru.chatter(level)
	if !ok {
		ru.rep.Waiting += len(items)
		return
	}
	req := ru.request(level, pitems, images, lc.model)
	est := Estimate(req)
	shares := map[int64]int64{}
	var (
		results  map[int64]Result
		errs     map[int64]error
		lastErr  error
		noBudget bool // 格式不对、但剩余预算不够重试
	)
	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := lc.ch.Chat(ru.ctx, req)
		if err != nil {
			ru.callFailed(level, items, pitems, images, shares, err)
			return
		}
		ru.record(level, lc, resp.Usage, est, items, shares)
		if resp.FinishReason == "length" {
			ru.r.log().Warn("AI 输出被截断（到了 max_tokens 上限）", "level", level, "attempt", attempt, "items", len(items))
		}
		results, errs, lastErr = parseItems(resp.Content, items, level != model.LevelLight)
		if lastErr == nil {
			break
		}
		if resp.FinishReason == "length" {
			lastErr = fmt.Errorf("%w（输出被截断）", lastErr)
		}
		ru.r.log().Warn("AI 返回的格式不对", "level", level, "attempt", attempt, "err", lastErr)
		if attempt == 2 || ru.cancelled() {
			break
		}
		if !ru.fits(est) {
			noBudget = true
			break
		}
	}
	for _, it := range items {
		if res, ok := results[it.ID]; ok {
			ru.applyOne(it, res, level, shares[it.ID])
			continue
		}
		if ru.cancelled() || noBudget {
			// 取消或预算不够导致没能重试：不记失败，只记 token；预算不够的留到下次。
			ru.saveTokens([]*model.Item{it}, shares)
			if noBudget {
				ru.rep.Queued++
			}
			continue
		}
		err := errs[it.ID]
		if err == nil {
			err = lastErr
		}
		ru.fail(it, err, shares[it.ID])
	}
}

// errKind 是调用出错时的处理方式。
type errKind int

const (
	errCancelled errKind = iota // 整理被取消：不停用档位、不记失败
	errProvider                 // 服务商不可用（密钥、余额、限流、5xx、网络）：停用整档，不记失败
	errRequest                  // 这次请求本身被拒（内容审核、图片不支持、请求过大）：只怪这几条
	errTimeout                  // 单次请求超时：只怪这几条
)

func (ru *run) classify(err error) errKind {
	if ru.cancelled() || errors.Is(err, context.Canceled) {
		return errCancelled
	}
	var he *llm.HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case 400, 413, 422:
			return errRequest
		}
		return errProvider
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errTimeout
	}
	return errProvider
}

// callFailed 处理 Chat 返回的错误。已花掉的 token（格式重试前那次）照样记到条目上。
//   - 400/413/422（如 DeepSeek 对敏感内容的 Content Exists Risk、对不支持图片的模型发图）：
//     多条一包时拆成逐条重发，找出是哪一条；单条时给这一条记一次失败，3 次后退出队列。
//   - 单条请求超时：记一次失败，不停用整档。多条一包超时仍按服务商不可用处理，避免拆开后每条再等一轮超时。
//   - 其余（401/402/403/404/429/5xx、网络错误）：停用整档、不记失败，下次再试。
func (ru *run) callFailed(level model.Level, items []*model.Item, pitems []promptItem, images []string, shares map[int64]int64, err error) {
	kind := ru.classify(err)
	if kind == errTimeout && len(items) > 1 {
		kind = errProvider
	}
	switch kind {
	case errCancelled:
		ru.saveTokens(items, shares)
	case errRequest, errTimeout:
		if len(items) == 1 {
			ru.r.log().Warn("AI 拒绝或超时，这一条记一次失败", "level", level, "item", items[0].ID, "err", err)
			ru.fail(items[0], err, shares[items[0].ID])
			return
		}
		ru.r.log().Warn("AI 拒绝了整包请求，拆成逐条重发", "level", level, "items", len(items), "err", err)
		ru.saveTokens(items, shares)
		for i, it := range items {
			if ru.cancelled() {
				return
			}
			one := []promptItem{pitems[i]}
			if !ru.fits(Estimate(ru.request(level, one, images, ""))) {
				ru.rep.Queued++
				continue
			}
			ru.call(level, []*model.Item{it}, one, images)
		}
	default:
		ru.stop(level, fmt.Sprintf("%s档调用失败，本次不再调用：%v", levelName(level), err))
		ru.rep.Waiting += len(items)
		ru.saveTokens(items, shares)
	}
}

// record 记用量。服务商没返回用量时按估算记，避免预算失效；token 按条目平摊进 shares。
func (ru *run) record(level model.Level, lc levelChatter, u llm.Usage, est int64, items []*model.Item, shares map[int64]int64) {
	if u.Total() == 0 {
		u.PromptTokens = est
	}
	total := u.Total()
	ru.budget.Spend(total)
	ru.rep.Tokens += total
	var itemID int64
	if len(items) == 1 {
		itemID = items[0].ID
	}
	if err := ru.r.Store.AddUsage(ru.wctx, store.Usage{Day: ru.day, Level: string(level), Provider: lc.prov.Name, Model: lc.model,
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, ItemID: itemID}); err != nil {
		ru.r.log().Error("记录 AI 用量失败", "err", err)
	}
	n := int64(len(items))
	for i, it := range items {
		share := total / n
		if i == 0 {
			share += total % n
		}
		shares[it.ID] += share
	}
}

// modify 在事务里读出条目的最新版本再改，不用批次开始时读出的旧副本覆盖用户在此期间的修改。
func (ru *run) modify(id int64, fn func(cur *model.Item) error) bool {
	if _, err := ru.r.Store.ModifyItem(ru.wctx, id, fn); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			ru.r.log().Warn("条目在整理期间被删除", "item", id)
		} else {
			ru.r.log().Error("保存整理结果失败", "item", id, "err", err)
		}
		return false
	}
	return true
}

// applyOne 写回 AI 结果：在最新行上 apply（用户在调用期间改成手动分类或改了状态的，apply 不再改分类）。
func (ru *run) applyOne(snap *model.Item, res Result, level model.Level, tokens int64) {
	ok := ru.modify(snap.ID, func(cur *model.Item) error {
		apply(cur, res, level)
		cur.TokensUsed += tokens
		return nil
	})
	if !ok {
		ru.rep.Failed++
		return
	}
	if len(res.Tags) > 0 {
		if err := ru.r.Store.AddTags(ru.wctx, snap.ID, res.Tags); err != nil {
			ru.r.log().Warn("保存标签失败", "item", snap.ID, "err", err)
		}
	}
	for _, w := range res.Warnings {
		ru.r.log().Info("AI 结果有字段被忽略", "item", snap.ID, "warning", w)
	}
	ru.rep.Processed++
}

// fail 给条目记一次失败。写库失败（条目已被删除等）时不计入报告的失败数，modify 已记日志。
func (ru *run) fail(snap *model.Item, err error, tokens int64) {
	ok := ru.modify(snap.ID, func(cur *model.Item) error {
		cur.ProcessAttempts++
		cur.ProcessError = model.TruncateRunes(err.Error(), 300)
		cur.TokensUsed += tokens
		return nil
	})
	if !ok {
		ru.r.log().Warn("记录整理失败没写进去，不计入失败数", "item", snap.ID, "err", err)
		return
	}
	ru.rep.Failed++
}

// saveTokens 只把已花掉的 token 记到条目上（调用中途出错或被取消时）。
func (ru *run) saveTokens(items []*model.Item, shares map[int64]int64) {
	for _, it := range items {
		if n := shares[it.ID]; n > 0 {
			ru.modify(it.ID, func(cur *model.Item) error {
				cur.TokensUsed += n
				return nil
			})
		}
	}
}

// skip 处理没有内容可交给 AI 的条目：未整理的归到资料，直接标记为已处理。
func (ru *run) skip(snap *model.Item) {
	ok := ru.modify(snap.ID, func(cur *model.Item) error {
		if cur.Category == model.CatInbox && cur.CategoryBy == model.ByAI && cur.Status == model.DefaultStatus(cur.Category) {
			cur.Category, cur.Status = model.CatArchive, model.DefaultStatus(model.CatArchive)
		}
		if cur.Level.Rank() > cur.ProcessedLevel.Rank() {
			cur.ProcessedLevel = cur.Level
		}
		cur.ProcessError = ""
		return nil
	})
	if ok {
		ru.rep.Skipped++
	}
}

func (ru *run) imageAtts(it *model.Item) []model.Attachment {
	if !ru.settings.AI.Images {
		return nil
	}
	atts, err := ru.r.Store.ListAttachments(ru.ctx, it.ID)
	if err != nil {
		ru.r.log().Error("读取附件失败", "item", it.ID, "err", err)
		return nil
	}
	var out []model.Attachment
	for _, a := range atts {
		if a.Kind == "image" && a.State == "ok" && a.RelPath != "" {
			out = append(out, a)
			if len(out) == maxImages {
				break
			}
		}
	}
	return out
}

var imageMIME = map[string]string{".png": "image/png", ".gif": "image/gif", ".webp": "image/webp"}

func (ru *run) loadImages(atts []model.Attachment) []string {
	var out []string
	for _, a := range atts {
		path := filepath.Join(ru.r.MediaDir, filepath.FromSlash(a.RelPath))
		info, err := os.Stat(path)
		if err != nil || info.Size() > maxImageBytes {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		mime := imageMIME[strings.ToLower(filepath.Ext(path))]
		if mime == "" {
			mime = "image/jpeg"
		}
		out = append(out, "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(data))
	}
	return out
}
