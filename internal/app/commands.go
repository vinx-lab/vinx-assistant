package app

import (
	"context"
	"net/http"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/command"
	"github.com/vinx-lab/vinx-assistant/internal/llm"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/remind"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// commandTranslator 每次调用时读设置，用轻量档的服务商和模型；没配置时返回 nil（指令只走固定格式，
// 看不懂的回用法提示）。指令兜底是用户在等结果的实时调用，用量记为 command，不计入每日整理限额。
func commandTranslator(st *store.Store, hc *http.Client, clk clock.Clock) func(context.Context) (command.Translator, error) {
	return func(ctx context.Context) (command.Translator, error) {
		set, err := st.LoadSettings(ctx)
		if err != nil {
			return nil, err
		}
		ref := set.AI.Ref(model.LevelLight)
		p, ok := set.AI.Provider(ref.ProviderID)
		if !ok || ref.Model == "" || p.BaseURL == "" {
			return nil, nil
		}
		return &command.AITranslator{
			Chat:   &command.LLMChatter{Client: llm.New(p.BaseURL, p.APIKey, hc), Model: ref.Model},
			Record: command.UsageRecorder(st, clk, p.Name, ref.Model),
		}, nil
	}
}

// wireCommandsAndReminders 装配微信指令（含 AI 兜底的后台 worker）与提醒，并接到收件服务上。
// 依赖 a.Store、a.Clock、a.Session、a.Notifier、a.Ingest、a.Log 已就绪。worker 由 serve 启动。
func (a *App) wireCommandsAndReminders(hc *http.Client) {
	a.Remind = &remind.Service{Store: a.Store, Notifier: a.Notifier, Clock: a.Clock, Log: a.Log, Session: a.Session}
	a.Commands = &command.Handler{
		Store: a.Store, Clock: a.Clock, Log: a.Log,
		Translator: commandTranslator(a.Store, hc, a.Clock),
		Reply:      a.Ingest.Reply,
		SaveAsItem: a.Ingest.SaveTextAsItem,
	}
	a.Ingest.Commands = a.Commands
	a.Ingest.Deferred = a.Remind
	a.Tickers = append(a.Tickers, a.Remind)
}
