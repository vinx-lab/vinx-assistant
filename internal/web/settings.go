package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vinx-lab/vinx-assistant/internal/batch"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/redact"
)

// generalForm 是设置页「整理与规则」表单的原始文本，校验失败时原样回显。
type generalForm struct {
	TokenLimit, BatchTimes, DigestTime  string
	Images                              bool
	Prefixes, LabelRules, Medium, Deep, ActionWords string
}

func generalFormOf(st model.Settings) generalForm {
	return generalForm{
		TokenLimit:  strconv.FormatInt(st.AI.DailyTokenLimit, 10),
		BatchTimes:  strings.Join(st.Schedule.BatchTimes, " "),
		DigestTime:  st.Schedule.DigestTime,
		Images:      st.AI.Images,
		Prefixes:    FormatPrefixes(st.Rules.Prefixes),
		LabelRules:  FormatLabelRules(st.Rules.LabelRules),
		Medium:      strings.Join(st.Rules.MediumKeywords, "\n"),
		Deep:        strings.Join(st.Rules.DeepKeywords, "\n"),
		ActionWords: FormatActionWords(st.Rules.ActionWords),
	}
}

// applyGeneral 校验表单并写进 st；出错时 st 不变。
func applyGeneral(st *model.Settings, f generalForm) error {
	limit, err := strconv.ParseInt(strings.TrimSpace(f.TokenLimit), 10, 64)
	if err != nil || limit < 0 {
		return errors.New("每日 token 上限应为不小于 0 的整数（0 表示不限）")
	}
	times, err := ParseTimes(f.BatchTimes)
	if err != nil {
		return errors.New("整理时间：" + err.Error())
	}
	digest, err := ParseClock(f.DigestTime)
	if err != nil {
		return errors.New("每日摘要时间：" + err.Error())
	}
	prefixes, err := ParsePrefixes(f.Prefixes)
	if err != nil {
		return err
	}
	words, err := ParseActionWords(f.ActionWords)
	if err != nil {
		return err
	}
	labelRules, err := ParseLabelRules(f.LabelRules)
	if err != nil {
		return err
	}
	st.AI.DailyTokenLimit, st.AI.Images = limit, f.Images
	st.Schedule.BatchTimes, st.Schedule.DigestTime = times, digest
	st.Rules.Prefixes, st.Rules.ActionWords, st.Rules.LabelRules = prefixes, words, labelRules
	st.Rules.MediumKeywords, st.Rules.DeepKeywords = ParseWords(f.Medium), ParseWords(f.Deep)
	return nil
}

type providerView struct {
	ID, Name, BaseURL, KeyTail string
	Models                     []string // 上次拉取到的模型 ID，供三档下拉使用
}

type settingsData struct {
	Page
	General   generalForm
	Providers []providerView
	AI        model.AI
	Levels    []levelView
	// LabelRules 是标签关键词的文本（每行「关键词=标签」），Prompt 是当前提示词说明（空则填默认），PromptPreview 是最终发给 AI 的完整提示词。
	LabelRules    string
	Prompt        string
	PromptPreview string
}

type levelView struct {
	Level      model.Level
	Name       string
	ProviderID string
	Model      string
	Options    []string // 模型下拉的选项：所选服务商已保存的模型，当前值不在其中时也列上
}

// modelOptions 是某档模型下拉的选项：服务商已保存的模型；当前值不在列表里时放在最前面，保证能原样显示和保存。
func modelOptions(models []string, cur string) []string {
	if cur == "" || slices.Contains(models, cur) {
		return models
	}
	return append([]string{cur}, models...)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, st model.Settings, g generalForm, errMsg string) {
	d := settingsData{Page: s.page(r, "设置", "settings"), General: g, AI: st.AI}
	d.Error = errMsg
	for _, p := range st.AI.Providers {
		d.Providers = append(d.Providers, providerView{ID: p.ID, Name: p.Name, BaseURL: p.BaseURL, KeyTail: redact.Secret(p.APIKey), Models: p.Models})
	}
	for _, l := range []struct {
		lv   model.Level
		name string
	}{{model.LevelLight, "轻量"}, {model.LevelMedium, "中等"}, {model.LevelDeep, "深度"}} {
		ref := st.AI.Ref(l.lv)
		p, _ := st.AI.Provider(ref.ProviderID)
		d.Levels = append(d.Levels, levelView{Level: l.lv, Name: l.name, ProviderID: ref.ProviderID, Model: ref.Model, Options: modelOptions(p.Models, ref.Model)})
	}
	d.LabelRules = g.LabelRules
	d.Prompt = st.Prompt
	if strings.TrimSpace(d.Prompt) == "" {
		d.Prompt = batch.DefaultPrompt
	}
	if topics, labels, err := batch.PromptTags(r.Context(), s.d.Store, st); err == nil {
		d.PromptPreview = batch.PreviewPrompt(st, s.d.Clock.Now(), topics, labels, model.LevelLight)
	}
	d.AI.Providers = nil // 模板里只用 Providers（已打码），不把密钥放进模板数据
	s.render(w, status, "settings", d)
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderSettings(w, r, http.StatusOK, st, generalFormOf(st), "")
}

func (s *Server) settingsGeneral(w http.ResponseWriter, r *http.Request) {
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	f := generalForm{TokenLimit: r.FormValue("token_limit"), BatchTimes: r.FormValue("batch_times"), DigestTime: r.FormValue("digest_time"),
		Images: r.FormValue("images") == "1", Prefixes: r.FormValue("prefixes"), Medium: r.FormValue("medium_keywords"),
		LabelRules: r.FormValue("label_rules"), Deep: r.FormValue("deep_keywords"), ActionWords: r.FormValue("action_words")}
	if err := applyGeneral(&st, f); err != nil {
		s.renderSettings(w, r, http.StatusBadRequest, st, f, err.Error())
		return
	}
	if err := s.d.Store.SaveSettings(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/settings?msg=saved")
}

// maxPromptRunes 是提示词说明的长度上限，防止误贴大段文字撑爆每次请求。
const maxPromptRunes = 4000

// settingsPrompt 保存整理提示词的说明部分；reset=1 或留空恢复默认。
func (s *Server) settingsPrompt(w http.ResponseWriter, r *http.Request) {
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	prompt := strings.TrimSpace(strings.ReplaceAll(r.FormValue("prompt"), "\r\n", "\n"))
	if r.FormValue("reset") == "1" || prompt == strings.TrimSpace(batch.DefaultPrompt) {
		prompt = ""
	}
	if utf8.RuneCountInString(prompt) > maxPromptRunes {
		saved := st
		st.Prompt = prompt // 只用于回显，不保存
		s.renderSettings(w, r, http.StatusBadRequest, st, generalFormOf(saved), "提示词太长（上限 "+strconv.Itoa(maxPromptRunes)+" 字）")
		return
	}
	st.Prompt = prompt
	if err := s.d.Store.SaveSettings(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/settings?msg=saved")
}

func (s *Server) settingsModels(w http.ResponseWriter, r *http.Request) {
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	refs := map[model.Level]*model.ModelRef{model.LevelLight: &st.AI.Light, model.LevelMedium: &st.AI.Medium, model.LevelDeep: &st.AI.Deep}
	for lv, ref := range refs {
		pid, m := r.FormValue(string(lv)+"_provider"), strings.TrimSpace(r.FormValue(string(lv)+"_model"))
		// 「手动输入」填了就以它为准（模型列表为空或想用列表外的模型时）
		if manual := strings.TrimSpace(r.FormValue(string(lv) + "_model_manual")); manual != "" {
			m = manual
		}
		if pid != "" {
			if _, ok := st.AI.Provider(pid); !ok {
				s.renderSettings(w, r, http.StatusBadRequest, st, generalFormOf(st), "选择的服务商不存在")
				return
			}
		}
		*ref = model.ModelRef{ProviderID: pid, Model: m}
	}
	if err := s.d.Store.SaveSettings(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/settings?msg=saved")
}

func newProviderID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "p" + hex.EncodeToString(b)
}

func validBaseURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// providerSave 新增或修改服务商。修改时密钥留空表示不改；但地址变了时必须重填密钥。
func (s *Server) providerSave(w http.ResponseWriter, r *http.Request) {
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.FormValue("id")
	name, base, key := strings.TrimSpace(r.FormValue("name")), strings.TrimSpace(r.FormValue("base_url")), strings.TrimSpace(r.FormValue("api_key"))
	if name == "" || !validBaseURL(base) {
		s.renderSettings(w, r, http.StatusBadRequest, st, generalFormOf(st), "服务商需要名称和 http(s) 开头的 API 地址")
		return
	}
	if id == "" {
		st.AI.Providers = append(st.AI.Providers, model.Provider{ID: newProviderID(), Name: name, BaseURL: base, APIKey: key})
	} else {
		found := false
		for i := range st.AI.Providers {
			if st.AI.Providers[i].ID == id {
				p := &st.AI.Providers[i]
				// 改了地址却沿用旧密钥，等于把密钥交给新地址（网站没有密码，别人可借此把密钥发到自己的服务器）。
				if key == "" && p.APIKey != "" && base != p.BaseURL {
					s.renderSettings(w, r, http.StatusBadRequest, st, generalFormOf(st), "API 地址变了，请重新填写密钥")
					return
				}
				if base != p.BaseURL {
					p.Models = nil // 模型列表属于旧地址
				}
				p.Name, p.BaseURL = name, base
				if key != "" {
					p.APIKey = key
				}
				found = true
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
	}
	if err := s.d.Store.SaveSettings(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/settings?msg=saved")
}

// providerDelete 删除服务商，并清空引用它的档位。
func (s *Server) providerDelete(w http.ResponseWriter, r *http.Request) {
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	kept := st.AI.Providers[:0]
	for _, p := range st.AI.Providers {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	st.AI.Providers = kept
	for _, ref := range []*model.ModelRef{&st.AI.Light, &st.AI.Medium, &st.AI.Deep} {
		if ref.ProviderID == id {
			*ref = model.ModelRef{}
		}
	}
	if err := s.d.Store.SaveSettings(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/settings?msg=deleted")
}

// providerModels 拉取服务商的模型列表（POST：会带着密钥访问外部服务）并保存到服务商上，供三档下拉使用。错误信息里的密钥打码。
func (s *Server) providerModels(w http.ResponseWriter, r *http.Request) {
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	p, ok := st.AI.Provider(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "服务商不存在"})
		return
	}
	if s.d.ListModels == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "未配置模型列表功能"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	models, err := s.d.ListModels(ctx, p)
	if err != nil {
		msg := err.Error()
		if p.APIKey != "" {
			msg = strings.ReplaceAll(msg, p.APIKey, redact.Secret(p.APIKey))
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": msg})
		return
	}
	// 拉取最多 20 秒，期间设置可能被改过：重新读一次再写，只更新这一个服务商；它被删了或换了地址就不存
	if err := s.saveProviderModels(r.Context(), p, models); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"models": models})
}

func (s *Server) saveProviderModels(ctx context.Context, fetched model.Provider, models []string) error {
	st, err := s.d.Store.LoadSettings(ctx)
	if err != nil {
		return err
	}
	for i := range st.AI.Providers {
		if p := &st.AI.Providers[i]; p.ID == fetched.ID && p.BaseURL == fetched.BaseURL {
			p.Models = slices.Clone(models)
			return s.d.Store.SaveSettings(ctx, st)
		}
	}
	return nil
}
