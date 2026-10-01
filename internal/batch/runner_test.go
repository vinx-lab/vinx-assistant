package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/enrich"
	"github.com/vinx-lab/vinx-assistant/internal/llm"
	"github.com/vinx-lab/vinx-assistant/internal/llm/llmtest"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type tenv struct {
	n   int
	st  *store.Store
	clk *clock.Fake
	llm *llmtest.Server
	r   *Runner
}

func newTEnv(t *testing.T, mod func(*model.Settings)) *tenv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := clock.NewFake(clock.At(2026, 10, 1, 9, 0))
	st.SetClock(clk)
	srv := llmtest.New()
	t.Cleanup(srv.Close)
	s := model.DefaultSettings()
	s.AI.Providers = []model.Provider{{ID: "p", Name: "测试服务商", BaseURL: srv.URL, APIKey: "sk-test-key-123456"}}
	s.AI.Light = model.ModelRef{ProviderID: "p", Model: "light-model"}
	s.AI.Medium = model.ModelRef{ProviderID: "p", Model: "medium-model"}
	s.AI.Deep = model.ModelRef{ProviderID: "p", Model: "deep-model"}
	if mod != nil {
		mod(&s)
	}
	if err := st.SaveSettings(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: st, Clock: clk, Fetcher: enrich.NewFetcher(nil), MediaDir: t.TempDir()}
	return &tenv{st: st, clk: clk, llm: srv, r: r}
}

func (e *tenv) add(t *testing.T, it *model.Item) int64 {
	t.Helper()
	if it.MsgID == "" {
		e.n++
		it.MsgID = fmt.Sprintf("m%d", e.n)
	}
	id, err := e.st.InsertItem(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *tenv) get(t *testing.T, id int64) *model.Item {
	t.Helper()
	it, err := e.st.GetItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *tenv) run(t *testing.T) Report {
	t.Helper()
	rep, err := e.r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// lightReq 按整理时实际会用的设置和标签构造请求，用来估算 token。
func (e *tenv) lightReq(items []promptItem, images []string, modelName string) llm.Request {
	st, _ := e.st.LoadSettings(context.Background())
	topics, labels, _ := PromptTags(context.Background(), e.st, st)
	return buildRequest(st.Prompt, model.LevelLight, e.clk.Now(), topics, labels, items, images, modelName)
}

type obj = map[string]any

func TestLightBatchClassifiesAndTags(t *testing.T) {
	e := newTEnv(t, nil)
	id1 := e.add(t, &model.Item{RawText: "一篇讲 SQLite 的文章 https://example.com/sqlite", URL: "https://example.com/sqlite"})
	id2 := e.add(t, &model.Item{RawText: "月底前把发票交给财务"})
	id3 := e.add(t, &model.Item{RawText: "交房租", Category: model.CatTodo, CategoryBy: model.ByPrefix})
	id4 := e.add(t, &model.Item{RawText: "做个收集箱", Category: model.CatIdea, CategoryBy: model.ByManual})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{
		{"id": id1, "category": "later", "tags": []string{"SQLite", "数据库"}, "title": "SQLite 文章", "summary": "讲 SQLite 的文章", "due": "", "priority": "low"},
		{"id": id2, "category": "todo", "tags": []string{"发票"}, "title": "交发票", "summary": "月底前交发票", "due": "2026-10-31", "priority": "high"},
		{"id": id3, "category": "idea", "tags": []string{"房租"}, "title": "交房租", "summary": "每月房租", "due": "2026-10-05", "priority": "medium"},
		{"id": id4, "category": "research", "tags": []string{}, "title": "收集箱", "summary": "做个收集箱", "due": "", "priority": ""},
		{"id": 99, "category": "todo", "title": "不属于本批"},
	}}))

	rep := e.run(t)
	if rep.Processed != 4 || rep.Failed != 0 || rep.Tokens != 150 {
		t.Fatalf("report = %+v", rep)
	}
	reqs := e.llm.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	r := reqs[0]
	if r.Model != "light-model" || r.Auth != "Bearer sk-test-key-123456" || strings.Contains(r.Body, "response_format") {
		t.Fatalf("request = %+v", r)
	}
	for _, want := range []string{"2026-10-01 09:00", `"fixed_category":"todo"`, `"fixed_category":"idea"`, "月底前把发票交给财务"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	it1 := e.get(t, id1)
	if it1.Category != model.CatLater || it1.Status != model.StatusNew || it1.Title != "SQLite 文章" || !reflect.DeepEqual(it1.Topics, []string{"SQLite", "数据库"}) || it1.ProcessedLevel != model.LevelLight {
		t.Fatalf("it1 = %+v", it1)
	}
	it2 := e.get(t, id2)
	if it2.Category != model.CatTodo || it2.Status != model.StatusOpen || it2.Priority != model.PriorityHigh || it2.DueAt == nil || it2.DueHasTime || it2.DueAt.Format("2006-01-02") != "2026-10-31" {
		t.Fatalf("it2 = %+v", it2)
	}
	if it3 := e.get(t, id3); it3.Category != model.CatTodo || it3.DueAt == nil {
		t.Fatalf("prefix category changed: %+v", it3)
	}
	if it4 := e.get(t, id4); it4.Category != model.CatIdea || it4.Status != model.StatusKept {
		t.Fatalf("manual category changed: %+v", it4)
	}
	if n, _ := e.st.TokensOn(context.Background(), "2026-10-01"); n != 150 {
		t.Fatalf("usage = %d", n)
	}
	var sum int64
	for _, id := range []int64{id1, id2, id3, id4} {
		sum += e.get(t, id).TokensUsed
	}
	if sum != 150 {
		t.Fatalf("tokens_used sum = %d", sum)
	}
	if again := e.run(t); again.Processed != 0 || len(e.llm.Requests()) != 1 {
		t.Fatalf("second run reprocessed: %+v", again)
	}
}

func TestBadJSONRetriesThenFails(t *testing.T) {
	e := newTEnv(t, nil)
	id := e.add(t, &model.Item{RawText: "随便一条"})
	bad := llmtest.Reply{Content: "抱歉，我无法处理"}
	for round := 1; round <= 3; round++ {
		e.llm.Enqueue(bad, bad)
		rep := e.run(t)
		if rep.Failed != 1 {
			t.Fatalf("round %d report = %+v", round, rep)
		}
		it := e.get(t, id)
		if it.ProcessAttempts != round || !strings.Contains(it.ProcessError, "JSON") {
			t.Fatalf("round %d item = %+v", round, it)
		}
	}
	if len(e.llm.Requests()) != 6 {
		t.Fatalf("requests = %d, want 6 (每轮重试 1 次)", len(e.llm.Requests()))
	}
	if rep := e.run(t); rep.Failed != 0 || len(e.llm.Requests()) != 6 {
		t.Fatalf("item retried after max attempts: %+v", rep)
	}
	if n, _ := e.st.TokensOn(context.Background(), "2026-10-01"); n == 0 {
		t.Fatal("calls without usage must still count toward budget")
	}
}

func TestMissingItemFailsOnlyThatItem(t *testing.T) {
	e := newTEnv(t, nil)
	id1 := e.add(t, &model.Item{RawText: "第一条"})
	id2 := e.add(t, &model.Item{RawText: "第二条"})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": id1, "category": "idea", "title": "第一条"}}}))
	rep := e.run(t)
	if rep.Processed != 1 || rep.Failed != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if it := e.get(t, id1); it.Category != model.CatIdea || it.ProcessAttempts != 0 {
		t.Fatalf("it1 = %+v", it)
	}
	if it := e.get(t, id2); it.ProcessAttempts != 1 || !strings.Contains(it.ProcessError, "没有返回") || it.Category != model.CatInbox {
		t.Fatalf("it2 = %+v", it)
	}
}

func TestDueHandling(t *testing.T) {
	e := newTEnv(t, nil)
	userDue := clock.At(2026, 10, 20, 15, 0)
	id1 := e.add(t, &model.Item{RawText: "周五下午六点前回复"})
	id2 := e.add(t, &model.Item{RawText: "月底交发票"})
	id3 := e.add(t, &model.Item{RawText: "下周三开会"})
	id4 := e.add(t, &model.Item{RawText: "已设截止", DueAt: &userDue, DueHasTime: true})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{
		{"id": id1, "category": "todo", "due": "2026-10-02 18:00"},
		{"id": id2, "category": "todo", "due": "2025-10-31"},
		{"id": id3, "category": "todo", "due": "下周三"},
		{"id": id4, "category": "todo", "due": "2026-11-01"},
	}}))
	if rep := e.run(t); rep.Processed != 4 {
		t.Fatalf("report = %+v", rep)
	}
	if it := e.get(t, id1); it.DueAt == nil || !it.DueHasTime || it.DueAt.Format(timeLayout) != "2026-10-02 18:00" {
		t.Fatalf("it1 due = %v", it.DueAt)
	}
	for _, id := range []int64{id2, id3} {
		if it := e.get(t, id); it.DueAt != nil {
			t.Fatalf("item %d got bad due %v", id, it.DueAt)
		}
	}
	if it := e.get(t, id4); !it.DueAt.Equal(userDue) || !it.DueHasTime {
		t.Fatalf("user due overwritten: %v", it.DueAt)
	}
}

func TestTokenLimitSkipsBigItemOnly(t *testing.T) {
	e := newTEnv(t, nil)
	big := e.add(t, &model.Item{RawText: strings.Repeat("很长的内容", 1000)})
	small := e.add(t, &model.Item{RawText: "短"})
	smallItem := e.get(t, small)
	est := Estimate(e.lightReq([]promptItem{toPromptItem(smallItem, lightTextRunes)}, nil, ""))
	s, _ := e.st.LoadSettings(context.Background())
	s.AI.DailyTokenLimit = est*3/2 + 10 // 预算比较时估算值留 1.5 倍余量
	e.st.SaveSettings(context.Background(), s)
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": small, "category": "idea", "title": "短"}}}))

	rep := e.run(t)
	if rep.Processed != 1 || rep.Queued != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if it := e.get(t, big); it.ProcessedLevel != "" || it.ProcessAttempts != 0 {
		t.Fatalf("big item touched: %+v", it)
	}
	if !strings.Contains(rep.String(), "留到下次 1 条") {
		t.Fatalf("report text = %s", rep.String())
	}
}

func TestTokenLimitAlreadyUsedUp(t *testing.T) {
	e := newTEnv(t, func(s *model.Settings) { s.AI.DailyTokenLimit = 1000 })
	e.st.AddUsage(context.Background(), store.Usage{Day: "2026-10-01", Level: "light", Model: "m", PromptTokens: 1000})
	e.add(t, &model.Item{RawText: "a"})
	e.add(t, &model.Item{RawText: "b"})
	rep := e.run(t)
	if rep.Queued != 2 || len(e.llm.Requests()) != 0 {
		t.Fatalf("report = %+v requests=%d", rep, len(e.llm.Requests()))
	}
}

func TestMediumFetchesGitHubReadme(t *testing.T) {
	e := newTEnv(t, nil)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/vinx-lab/demo/readme" {
			w.Write([]byte("# Demo\n这是 README 内容"))
			return
		}
		http.NotFound(w, r)
	}))
	defer gh.Close()
	e.r.Fetcher.GitHubAPI = gh.URL
	id := e.add(t, &model.Item{RawText: "https://github.com/vinx-lab/demo 研究一下", URL: "https://github.com/vinx-lab/demo", Level: model.LevelMedium})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": id, "category": "research", "tags": []string{"Go"}, "title": "Demo", "summary": "一个示例", "detail": "这是一个示例项目。值得一看。"}}}))

	rep := e.run(t)
	if rep.Processed != 1 {
		t.Fatalf("report = %+v", rep)
	}
	r := e.llm.Requests()[0]
	if r.Model != "medium-model" || !strings.Contains(r.Text, "这是 README 内容") || !strings.Contains(r.Text, "值不值得") {
		t.Fatalf("request = %+v", r)
	}
	it := e.get(t, id)
	if it.Detail != "这是一个示例项目。值得一看。" || it.ProcessedLevel != model.LevelMedium || it.Category != model.CatResearch {
		t.Fatalf("item = %+v", it)
	}
	var itemID int64
	e.st.DB().QueryRow(`SELECT item_id FROM llm_usage`).Scan(&itemID)
	if itemID != id {
		t.Fatalf("usage item_id = %d", itemID)
	}
}

func TestDeepWithoutDetailFails(t *testing.T) {
	e := newTEnv(t, nil)
	id := e.add(t, &model.Item{RawText: "深入研究 htmx", Level: model.LevelDeep})
	noDetail := llmtest.JSON(obj{"items": []obj{{"id": id, "category": "research", "title": "htmx"}}})
	e.llm.Enqueue(noDetail, noDetail)
	if rep := e.run(t); rep.Failed != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if it := e.get(t, id); !strings.Contains(it.ProcessError, "detail") || it.ProcessedLevel != "" {
		t.Fatalf("item = %+v", it)
	}
	if e.llm.Requests()[0].Model != "deep-model" {
		t.Fatal("deep level must use deep model")
	}
}

func TestUnconfiguredLevelWaits(t *testing.T) {
	e := newTEnv(t, func(s *model.Settings) { s.AI.Medium = model.ModelRef{} })
	id := e.add(t, &model.Item{RawText: "研究一下这个", Level: model.LevelMedium})
	rep := e.run(t)
	if rep.Waiting != 1 || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "中等档") {
		t.Fatalf("report = %+v", rep)
	}
	if it := e.get(t, id); it.ProcessAttempts != 0 || it.ProcessError != "" {
		t.Fatalf("item = %+v", it)
	}
	if len(e.llm.Requests()) != 0 {
		t.Fatal("must not call unconfigured level")
	}
}

func TestProviderErrorStopsLevelWithoutCountingAttempts(t *testing.T) {
	e := newTEnv(t, nil)
	id1 := e.add(t, &model.Item{RawText: "a"})
	id2 := e.add(t, &model.Item{RawText: "b"})
	e.llm.Enqueue(llmtest.Reply{Status: 500, Raw: "boom"})
	rep := e.run(t)
	if rep.Waiting != 2 || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "轻量档调用失败") {
		t.Fatalf("report = %+v", rep)
	}
	for _, id := range []int64{id1, id2} {
		if it := e.get(t, id); it.ProcessAttempts != 0 {
			t.Fatalf("attempts counted on provider error: %+v", it)
		}
	}
	if strings.Contains(rep.String(), "sk-test-key-123456") {
		t.Fatal("api key leaked in report")
	}
}

func TestNoContentSkipped(t *testing.T) {
	e := newTEnv(t, nil)
	id := e.add(t, &model.Item{RawText: ""})
	rep := e.run(t)
	if rep.Skipped != 1 || len(e.llm.Requests()) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if it := e.get(t, id); it.Category != model.CatArchive || it.Status != model.StatusKept || it.ProcessedLevel != model.LevelLight {
		t.Fatalf("item = %+v", it)
	}
}

func TestImagesSentWhenEnabled(t *testing.T) {
	e := newTEnv(t, func(s *model.Settings) { s.AI.Images = true })
	id := e.add(t, &model.Item{RawText: ""})
	rel := "2026/10/1-0.jpg"
	os.MkdirAll(filepath.Join(e.r.MediaDir, "2026/10"), 0o700)
	os.WriteFile(filepath.Join(e.r.MediaDir, rel), []byte("\xff\xd8\xff fake"), 0o600)
	e.st.InsertAttachment(context.Background(), &model.Attachment{ItemID: id, Kind: "image", State: "ok", RelPath: rel})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": id, "category": "archive", "title": "截图"}}}))

	if rep := e.run(t); rep.Processed != 1 {
		t.Fatalf("report = %+v", rep)
	}
	r := e.llm.Requests()[0]
	if r.Model != "light-model" || !strings.Contains(r.Body, "data:image/jpeg;base64,") || !strings.Contains(r.Body, `"detail":"low"`) {
		t.Fatalf("request body = %.300s", r.Body)
	}
}

func TestBusyWhenLeaseHeld(t *testing.T) {
	e := newTEnv(t, nil)
	e.add(t, &model.Item{RawText: "a"})
	e.st.TryLease(context.Background(), "batch.lease", "other", e.clk.Now(), time.Hour)
	rep := e.run(t)
	if !rep.Busy || len(e.llm.Requests()) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

// hookChatter 在把请求交给真正的客户端之前先执行 before，用来模拟「调用进行中」发生的事。
type hookChatter struct {
	inner  llm.Chatter
	before func(ctx context.Context)
}

func (h hookChatter) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	if h.before != nil {
		h.before(ctx)
	}
	return h.inner.Chat(ctx, req)
}

func (e *tenv) hook(before func(ctx context.Context)) {
	e.r.NewLLM = func(p model.Provider) llm.Chatter {
		return hookChatter{inner: llm.New(p.BaseURL, p.APIKey, nil), before: before}
	}
}

func TestBudgetKeepsMargin(t *testing.T) {
	e := newTEnv(t, nil)
	id := e.add(t, &model.Item{RawText: "短"})
	est := Estimate(e.lightReq([]promptItem{toPromptItem(e.get(t, id), lightTextRunes)}, nil, ""))
	s, _ := e.st.LoadSettings(context.Background())
	s.AI.DailyTokenLimit = est + 10 // 够裸估算，但不够 1.5 倍余量
	e.st.SaveSettings(context.Background(), s)
	rep := e.run(t)
	if rep.Queued != 1 || len(e.llm.Requests()) != 0 {
		t.Fatalf("report = %+v requests=%d", rep, len(e.llm.Requests()))
	}
}

// 批次读出条目之后、写回之前用户改了条目，写回时不能用旧副本覆盖用户的修改。
func TestUserEditDuringCallNotOverwritten(t *testing.T) {
	e := newTEnv(t, nil)
	ctx := context.Background()
	// a：未整理，调用期间用户手动归到待办并标记完成。
	a := e.add(t, &model.Item{RawText: "买打印纸"})
	// b：已按轻量整理成待办，用户把深度调到中等后，在调用期间把它标记完成。
	b := e.add(t, &model.Item{RawText: "研究 SQLite WAL", Category: model.CatTodo, Status: model.StatusOpen, ProcessedLevel: model.LevelLight, Level: model.LevelMedium})
	if err := e.st.AddTags(ctx, a, store.TagKindLabel, []string{"待办"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	e.hook(func(context.Context) {
		calls++
		switch calls {
		case 1:
			e.st.ModifyItem(ctx, a, func(it *model.Item) error {
				it.Category, it.CategoryBy, it.Status = model.CatTodo, model.ByManual, model.StatusDone
				return nil
			})
		case 2:
			e.st.ModifyItem(ctx, b, func(it *model.Item) error {
				it.Status = model.StatusDone
				return nil
			})
		}
	})
	e.llm.Enqueue(
		llmtest.JSON(obj{"items": []obj{{"id": a, "category": "idea", "tags": []string{"办公"}, "title": "买纸", "priority": "low"}}}),
		llmtest.JSON(obj{"items": []obj{{"id": b, "category": "research", "title": "SQLite WAL", "detail": "WAL 模式介绍。值得看。"}}}),
	)
	if rep := e.run(t); rep.Processed != 2 {
		t.Fatalf("report = %+v", rep)
	}
	ia := e.get(t, a)
	if ia.Category != model.CatTodo || ia.CategoryBy != model.ByManual || ia.Status != model.StatusDone {
		t.Fatalf("user edit on a overwritten: %+v", ia)
	}
	if ia.Title != "买纸" || ia.ProcessedLevel != model.LevelLight || ia.TokensUsed != 150 || !reflect.DeepEqual(ia.Topics, []string{"办公"}) || !reflect.DeepEqual(ia.Labels, []string{"待办"}) {
		t.Fatalf("AI fields on a not applied: %+v", ia)
	}
	ib := e.get(t, b)
	if ib.Category != model.CatTodo || ib.Status != model.StatusDone {
		t.Fatalf("user edit on b overwritten: %+v", ib)
	}
	if ib.Detail != "WAL 模式介绍。值得看。" || ib.ProcessedLevel != model.LevelMedium {
		t.Fatalf("AI fields on b not applied: %+v", ib)
	}
}

// 一次整理进行中，同进程再调 Run 返回 Busy；Running 反映状态；租约 TTL 足够长，结束后释放。
func TestConcurrentRunIsBusy(t *testing.T) {
	e := newTEnv(t, nil)
	id := e.add(t, &model.Item{RawText: "a"})
	entered := make(chan struct{})
	release := make(chan struct{})
	e.hook(func(context.Context) {
		close(entered)
		<-release
	})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": id, "category": "idea", "title": "a"}}}))
	if e.r.Running() {
		t.Fatal("running before Run")
	}
	var (
		wg   sync.WaitGroup
		rep1 Report
		err1 error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		rep1, err1 = e.r.Run(context.Background())
	}()
	<-entered
	if !e.r.Running() {
		t.Error("Running() = false during run")
	}
	rep2, err := e.r.Run(context.Background())
	if err != nil || !rep2.Busy {
		t.Errorf("second run = %+v, %v", rep2, err)
	}
	if v, ok, _ := e.st.GetKV(context.Background(), leaseKey); !ok || !strings.HasPrefix(v, fmt.Sprint(e.clk.Now().Add(leaseTTL).Unix())+":") {
		t.Errorf("lease value = %q ok=%v", v, ok)
	}
	close(release)
	wg.Wait()
	if err1 != nil || rep1.Processed != 1 || rep1.Busy {
		t.Fatalf("first run = %+v, %v", rep1, err1)
	}
	if e.r.Running() {
		t.Fatal("Running() = true after run")
	}
	if _, ok, _ := e.st.GetKV(context.Background(), leaseKey); ok {
		t.Fatal("lease not released")
	}
}

// 批次被取消时不记失败、不停用档位，也不算服务商出错。
func TestCancelledRunRecordsNoFailure(t *testing.T) {
	e := newTEnv(t, nil)
	id1 := e.add(t, &model.Item{RawText: "a"})
	id2 := e.add(t, &model.Item{RawText: "b", Level: model.LevelMedium})
	ctx, cancel := context.WithCancel(context.Background())
	e.hook(func(context.Context) { cancel() })
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": id1, "category": "idea"}}}))
	rep, err := e.r.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if rep.Failed != 0 || rep.Processed != 0 || strings.Contains(rep.String(), "调用失败") {
		t.Fatalf("report = %+v", rep)
	}
	for _, id := range []int64{id1, id2} {
		if it := e.get(t, id); it.ProcessAttempts != 0 || it.ProcessError != "" || it.ProcessedLevel != "" {
			t.Fatalf("item touched on cancel: %+v", it)
		}
	}
	if len(e.llm.Requests()) > 1 {
		t.Fatalf("kept calling after cancel: %d", len(e.llm.Requests()))
	}
	if _, ok, _ := e.st.GetKV(context.Background(), leaseKey); ok {
		t.Fatal("lease not released after cancel")
	}
}

// 已整理成待办并标了完成的条目，调高深度后再整理：AI 不改分类和状态，只补 detail。
func TestDoneTodoKeepsCategoryOnDeeperRun(t *testing.T) {
	e := newTEnv(t, nil)
	id := e.add(t, &model.Item{RawText: "研究 SQLite WAL", Category: model.CatTodo, Status: model.StatusDone, ProcessedLevel: model.LevelLight, Level: model.LevelMedium})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": id, "category": "research", "title": "WAL", "detail": "WAL 介绍。值得看。"}}}))
	if rep := e.run(t); rep.Processed != 1 {
		t.Fatalf("report = %+v", rep)
	}
	it := e.get(t, id)
	if it.Category != model.CatTodo || it.Status != model.StatusDone || it.Detail != "WAL 介绍。值得看。" || it.ProcessedLevel != model.LevelMedium {
		t.Fatalf("item = %+v", it)
	}
}

// 附件记录正常但图片文件丢了、原文和链接都为空：跳过，不发空请求。
func TestMissingImageFileSkipped(t *testing.T) {
	e := newTEnv(t, func(s *model.Settings) { s.AI.Images = true })
	id := e.add(t, &model.Item{RawText: ""})
	e.st.InsertAttachment(context.Background(), &model.Attachment{ItemID: id, Kind: "image", State: "ok", RelPath: "2026/10/gone.jpg"})
	rep := e.run(t)
	if rep.Skipped != 1 || len(e.llm.Requests()) != 0 {
		t.Fatalf("report = %+v requests=%d", rep, len(e.llm.Requests()))
	}
	if it := e.get(t, id); it.Category != model.CatArchive || it.ProcessedLevel != model.LevelLight || it.ProcessAttempts != 0 {
		t.Fatalf("item = %+v", it)
	}
}

// 格式不对、但预算不够重试：留到下次，不计失败次数。
func TestRetrySkippedForBudgetQueues(t *testing.T) {
	e := newTEnv(t, nil)
	id := e.add(t, &model.Item{RawText: "随便一条"})
	est := Estimate(e.lightReq([]promptItem{toPromptItem(e.get(t, id), lightTextRunes)}, nil, ""))
	s, _ := e.st.LoadSettings(context.Background())
	s.AI.DailyTokenLimit = est*3/2 + 10 // 第一次放得下；按估算记账后剩余不够再试一次
	e.st.SaveSettings(context.Background(), s)
	e.llm.Enqueue(llmtest.Reply{Content: "抱歉，我无法处理"})
	rep := e.run(t)
	if rep.Queued != 1 || rep.Failed != 0 || len(e.llm.Requests()) != 1 {
		t.Fatalf("report = %+v requests=%d", rep, len(e.llm.Requests()))
	}
	if it := e.get(t, id); it.ProcessAttempts != 0 || it.ProcessError != "" || it.TokensUsed != est {
		t.Fatalf("item = %+v", it)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 整理期间心跳按当前时间续期租约，结束后释放。
func TestLeaseHeartbeatRenews(t *testing.T) {
	e := newTEnv(t, nil)
	e.r.renewEvery = 5 * time.Millisecond
	id := e.add(t, &model.Item{RawText: "a"})
	ctx := context.Background()
	start := e.clk.Now()
	e.hook(func(context.Context) {
		e.clk.Advance(5 * time.Minute)
		want := fmt.Sprint(start.Add(5*time.Minute+leaseTTL).Unix()) + ":"
		waitFor(t, func() bool {
			v, _, _ := e.st.GetKV(ctx, leaseKey)
			return strings.HasPrefix(v, want)
		})
	})
	e.llm.Enqueue(llmtest.JSON(obj{"items": []obj{{"id": id, "category": "idea", "title": "a"}}}))
	if rep := e.run(t); rep.Processed != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if _, ok, _ := e.st.GetKV(ctx, leaseKey); ok {
		t.Fatal("lease not released")
	}
}

// 租约被别的进程接管（例如本进程卡太久过期了）：心跳发现后取消本次整理，且不释放别人的租约。
func TestLeaseLostCancelsRun(t *testing.T) {
	e := newTEnv(t, nil)
	e.r.renewEvery = 5 * time.Millisecond
	e.add(t, &model.Item{RawText: "a"})
	ctx := context.Background()
	e.hook(func(runCtx context.Context) {
		e.st.SetKV(ctx, leaseKey, "9999999999:other")
		select {
		case <-runCtx.Done():
		case <-time.After(5 * time.Second):
			t.Error("run not cancelled after lease lost")
		}
	})
	rep, err := e.r.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "租约") {
		t.Fatalf("err = %v rep = %+v", err, rep)
	}
	if v, _, _ := e.st.GetKV(ctx, leaseKey); v != "9999999999:other" {
		t.Fatalf("other's lease touched: %q", v)
	}
}

// DeepSeek 对敏感内容返回 400 Content Exists Risk：整包被拒时拆成逐条重发，只给出问题的那条记失败。
func TestBadRequestSplitsBatch(t *testing.T) {
	e := newTEnv(t, nil)
	id1 := e.add(t, &model.Item{RawText: "敏感内容"})
	id2 := e.add(t, &model.Item{RawText: "第二条"})
	id3 := e.add(t, &model.Item{RawText: "第三条"})
	risk := llmtest.Reply{Status: 400, Raw: `{"error":{"message":"Content Exists Risk"}}`}
	e.llm.Enqueue(risk, risk,
		llmtest.JSON(obj{"items": []obj{{"id": id2, "category": "idea", "title": "二"}}}),
		llmtest.JSON(obj{"items": []obj{{"id": id3, "category": "later", "title": "三"}}}))
	rep := e.run(t)
	if rep.Processed != 2 || rep.Failed != 1 || rep.Waiting != 0 || len(rep.Notes) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if n := len(e.llm.Requests()); n != 4 {
		t.Fatalf("requests = %d, want 4 (1 包 + 3 条)", n)
	}
	if it := e.get(t, id1); it.ProcessAttempts != 1 || !strings.Contains(it.ProcessError, "Content Exists Risk") || it.Category != model.CatInbox {
		t.Fatalf("it1 = %+v", it)
	}
	if it := e.get(t, id2); it.Category != model.CatIdea || it.ProcessAttempts != 0 {
		t.Fatalf("it2 = %+v", it)
	}
	if it := e.get(t, id3); it.Category != model.CatLater {
		t.Fatalf("it3 = %+v", it)
	}
}

// 单条 400：记一次失败，不停用档位，同档后面的条目照常处理；3 次后退出队列。
func TestBadRequestSingleCountsAttempt(t *testing.T) {
	e := newTEnv(t, nil)
	id1 := e.add(t, &model.Item{RawText: "带图研究", Level: model.LevelMedium})
	id2 := e.add(t, &model.Item{RawText: "另一条研究", Level: model.LevelMedium})
	bad := llmtest.Reply{Status: 400, Raw: "image_url is not supported"}
	e.llm.Enqueue(bad, llmtest.JSON(obj{"items": []obj{{"id": id2, "category": "research", "title": "研究", "detail": "## 要点"}}}))
	rep := e.run(t)
	if rep.Failed != 1 || rep.Processed != 1 || len(rep.Notes) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if it := e.get(t, id1); it.ProcessAttempts != 1 || !strings.Contains(it.ProcessError, "HTTP 400") {
		t.Fatalf("it1 = %+v", it)
	}
	e.llm.Enqueue(bad, bad)
	e.run(t)
	e.run(t)
	if it := e.get(t, id1); it.ProcessAttempts != 3 {
		t.Fatalf("attempts = %d", it.ProcessAttempts)
	}
	before := len(e.llm.Requests())
	if rep := e.run(t); rep.Failed != 0 || len(e.llm.Requests()) != before {
		t.Fatalf("item retried after max attempts: %+v", rep)
	}
}

// 401/402/403/404/429/5xx 是服务商层面的问题：停用整档、不记失败。
func TestProviderStatusesStopLevel(t *testing.T) {
	for _, status := range []int{401, 402, 403, 404, 429, 503} {
		e := newTEnv(t, nil)
		id := e.add(t, &model.Item{RawText: "a", Level: model.LevelMedium})
		e.add(t, &model.Item{RawText: "b", Level: model.LevelMedium})
		e.llm.Enqueue(llmtest.Reply{Status: status, Raw: "nope"})
		rep := e.run(t)
		if rep.Waiting != 2 || rep.Failed != 0 || len(e.llm.Requests()) != 1 {
			t.Fatalf("status %d: report = %+v", status, rep)
		}
		if it := e.get(t, id); it.ProcessAttempts != 0 {
			t.Fatalf("status %d: attempts counted", status)
		}
	}
}

// slowLLM 返回一个超时很短的真实客户端，指向一个永远不回的服务。
func slowLLM(t *testing.T) func(model.Provider) llm.Chatter {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) }) // 先于 srv.Close 执行
	return func(p model.Provider) llm.Chatter {
		c := llm.New(srv.URL, p.APIKey, nil)
		c.Timeout = 50 * time.Millisecond
		return c
	}
}

// 单条请求超时（不是整理被取消）：记一次失败，不停用整档。
func TestSingleTimeoutCountsAttempt(t *testing.T) {
	e := newTEnv(t, nil)
	id1 := e.add(t, &model.Item{RawText: "a", Level: model.LevelMedium})
	id2 := e.add(t, &model.Item{RawText: "b", Level: model.LevelMedium})
	e.r.NewLLM = slowLLM(t)
	rep := e.run(t)
	if rep.Failed != 2 || rep.Waiting != 0 || len(rep.Notes) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	for _, id := range []int64{id1, id2} {
		if it := e.get(t, id); it.ProcessAttempts != 1 || it.ProcessError == "" {
			t.Fatalf("item = %+v", it)
		}
	}
}

// 多条一包超时仍停用整档、不记失败，避免拆开后每条再等一轮超时。
func TestBatchTimeoutStopsLevel(t *testing.T) {
	e := newTEnv(t, nil)
	id := e.add(t, &model.Item{RawText: "a"})
	e.add(t, &model.Item{RawText: "b"})
	e.r.NewLLM = slowLLM(t)
	rep := e.run(t)
	if rep.Waiting != 2 || rep.Failed != 0 || len(rep.Notes) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if it := e.get(t, id); it.ProcessAttempts != 0 {
		t.Fatalf("item = %+v", it)
	}
}

func TestTruncatedOutputRetriesThenFails(t *testing.T) {
	e := newTEnv(t, nil)
	id := e.add(t, &model.Item{RawText: "一条"})
	cut := llmtest.Reply{Content: `{"items":[{"id":1,"categ`, FinishReason: "length", PromptTokens: 10, CompletionTokens: 5}
	e.llm.Enqueue(cut, cut)
	if rep := e.run(t); rep.Failed != 1 || len(e.llm.Requests()) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if it := e.get(t, id); it.ProcessAttempts != 1 || !strings.Contains(it.ProcessError, "截断") {
		t.Fatalf("item = %+v", it)
	}
}

// 条目在调用期间被删掉：写不回失败，不计入失败数。
func TestFailOnDeletedItemNotCounted(t *testing.T) {
	e := newTEnv(t, nil)
	id := e.add(t, &model.Item{RawText: "会被删掉"})
	e.hook(func(ctx context.Context) {
		if _, err := e.st.DB().Exec(`DELETE FROM items WHERE id = ?`, id); err != nil {
			t.Error(err)
		}
	})
	bad := llmtest.Reply{Content: "不是 JSON"}
	e.llm.Enqueue(bad, bad)
	if rep := e.run(t); rep.Failed != 0 || rep.Processed != 0 {
		t.Fatalf("report = %+v", rep)
	}
}
