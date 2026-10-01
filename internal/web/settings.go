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
	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/redact"
)

// generalForm 是设置页「整理与规则」表单的原始文本，校验失败时原样回显。
type generalForm struct {
	TokenLimit, BatchTimes, DigestTime              string
	Images                                          bool
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

// applyGeneral 校验表单并写进 st；出错时 st 不变。返回出错的小节（rules / keywords），供回显时定位。
func applyGeneral(st *model.Settings, f generalForm) (string, error) {
	limit, err := strconv.ParseInt(strings.TrimSpace(f.TokenLimit), 10, 64)
	if err != nil || limit < 0 {
		return "rules", errors.New("每日 token 上限应为不小于 0 的整数（0 表示不限）")
	}
	times, err := ParseTimes(f.BatchTimes)
	if err != nil {
		return "rules", errors.New("整理时间：" + err.Error())
	}
	digest, err := ParseClock(f.DigestTime)
	if err != nil {
		return "rules", errors.New("每日摘要时间：" + err.Error())
	}
	prefixes, err := ParsePrefixes(f.Prefixes)
	if err != nil {
		return "keywords", err
	}
	words, err := ParseActionWords(f.ActionWords)
	if err != nil {
		return "keywords", err
	}
	labelRules, err := ParseLabelRules(f.LabelRules)
	if err != nil {
		return "keywords", err
	}
	st.AI.DailyTokenLimit, st.AI.Images = limit, f.Images
	st.Schedule.BatchTimes, st.Schedule.DigestTime = times, digest
	st.Rules.Prefixes, st.Rules.ActionWords, st.Rules.LabelRules = prefixes, words, labelRules
	st.Rules.MediumKeywords, st.Rules.DeepKeywords = ParseWords(f.Medium), ParseWords(f.Deep)
	return "", nil
}

// settingSection 是设置页的一个小节。微信登录、用量是独立页面，也挂在设置的小节导航里。
type settingSection struct {
	Key, Name, Href, Icon string
}

// settingGroup 是小节导航里的一组。
type settingGroup struct {
	Name     string
	Sections []settingSection
}

var settingGroups = []settingGroup{
	{"AI", []settingSection{
		{"providers", "服务商", "/settings/providers", "key"},
		{"models", "模型", "/settings/models", "cpu"},
		{"prompt", "提示词", "/settings/prompt", "text"},
	}},
	{"整理", []settingSection{
		{"rules", "时间与额度", "/settings/rules", "clockc"},
		{"keywords", "关键词", "/settings/keywords", "tag"},
	}},
	{"账号与数据", []settingSection{
		{"login", "微信登录", "/login", "wechat"},
		{"usage", "用量", "/usage", "chart"},
	}},
}

// sectionHref 是设置小节的地址；未知小节回到设置首页。
func sectionHref(key string) string {
	for _, g := range settingGroups {
		for _, sec := range g.Sections {
			if sec.Key == key {
				return sec.Href
			}
		}
	}
	return "/settings"
}

// settingsPageSections 是由 settings 模板渲染的小节（不含独立页面 login、usage）。
var settingsPageSections = map[string]string{"providers": "服务商", "models": "模型", "rules": "时间与额度", "keywords": "关键词", "prompt": "提示词"}

// settingsIndex 是手机上设置首页分组列表右侧显示的当前值，键是小节 Key。
type settingsIndex map[string]string

type providerView struct {
	ID, Name, BaseURL, KeyTail string
	Models                     []string // 上次拉取到的模型 ID，供三档下拉使用
}

type settingsData struct {
	Page
	Section string // 当前小节：providers models rules keywords prompt
	// IsIndex 表示访问的是 /settings：电脑上显示服务商小节，手机上显示分组列表（Index 是各项的当前值）
	IsIndex   bool
	Index     settingsIndex
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

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, section string, st model.Settings, g generalForm, errMsg string) {
	isIndex := r.Method == http.MethodGet && r.URL.Path == "/settings"
	pg := s.page(r, settingsPageSections[section], "settings")
	pg.Sub, pg.Up = section, "/settings"
	if isIndex {
		pg.Up = ""
	}
	d := settingsData{Page: pg, Section: section, IsIndex: isIndex, General: g, AI: st.AI}
	d.Error = errMsg
	if isIndex {
		d.Index = s.settingsIndexOf(r.Context(), st, pg.Top.WeChat)
	}
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
	if section == "prompt" { // 预览要查库里的标签，只在提示词小节生成
		if topics, labels, err := batch.PromptTags(r.Context(), s.d.Store, st); err == nil {
			d.PromptPreview = batch.PreviewPrompt(st, s.d.Clock.Now(), topics, labels, model.LevelLight)
		}
	}
	d.AI.Providers = nil // 模板里只用 Providers（已打码），不把密钥放进模板数据
	s.render(w, status, "settings", d)
}

// settingsIndexOf 是手机设置首页每一项右侧的当前值。
func (s *Server) settingsIndexOf(ctx context.Context, st model.Settings, wechat string) settingsIndex {
	ix := settingsIndex{"providers": "未设置", "models": "未设置", "prompt": "默认", "rules": strings.Join(st.Schedule.BatchTimes, " ")}
	if n := len(st.AI.Providers); n == 1 {
		ix["providers"] = st.AI.Providers[0].Name
	} else if n > 1 {
		ix["providers"] = strconv.Itoa(n) + " 个"
	}
	if m := st.AI.Light.Model; m != "" {
		ix["models"] = m
	}
	if strings.TrimSpace(st.Prompt) != "" {
		ix["prompt"] = "自定义"
	}
	if ix["rules"] == "" {
		ix["rules"] = "未设置"
	}
	ix["keywords"] = strconv.Itoa(len(st.Rules.Prefixes)+len(st.Rules.LabelRules)) + " 条"
	ix["login"] = map[string]string{"ok": "正常", "paused": "暂停中"}[wechat]
	if ix["login"] == "" {
		ix["login"] = "未登录"
	}
	if n, err := s.d.Store.TokensOn(ctx, clock.DayString(s.d.Clock.Now())); err == nil {
		ix["usage"] = "今日 " + strconv.FormatInt(n, 10)
	}
	return ix
}

// settingsPage 显示一个设置小节：/settings 在电脑上是服务商小节、手机上是分组列表，/settings/{section} 是单个小节。
func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	section := r.PathValue("section")
	if section == "" {
		section = "providers"
	}
	if _, ok := settingsPageSections[section]; !ok {
		http.NotFound(w, r)
		return
	}
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderSettings(w, r, http.StatusOK, section, st, generalFormOf(st), "")
}

// settingsGeneral 保存「整理与规则」或「关键词」小节。两个小节共用这个地址，用隐藏字段 section 区分，
// 只取本小节的字段，其余沿用已保存的值；没有 section 时（旧表单）取全部字段。
func (s *Server) settingsGeneral(w http.ResponseWriter, r *http.Request) {
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	section := r.FormValue("section")
	f := generalFormOf(st)
	if section == "" || section == "rules" {
		f.TokenLimit, f.BatchTimes, f.DigestTime, f.Images = r.FormValue("token_limit"), r.FormValue("batch_times"), r.FormValue("digest_time"), r.FormValue("images") == "1"
	}
	if section == "" || section == "keywords" {
		f.Prefixes, f.LabelRules, f.Medium, f.Deep, f.ActionWords = r.FormValue("prefixes"), r.FormValue("label_rules"), r.FormValue("medium_keywords"), r.FormValue("deep_keywords"), r.FormValue("action_words")
	}
	if bad, err := applyGeneral(&st, f); err != nil {
		s.renderSettings(w, r, http.StatusBadRequest, bad, st, f, err.Error())
		return
	}
	if err := s.d.Store.SaveSettings(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	if section == "" {
		section = "rules"
	}
	s.redirect(w, r, withMsg(sectionHref(section), "saved"))
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
		s.renderSettings(w, r, http.StatusBadRequest, "prompt", st, generalFormOf(saved), "提示词太长（上限 "+strconv.Itoa(maxPromptRunes)+" 字）")
		return
	}
	st.Prompt = prompt
	if err := s.d.Store.SaveSettings(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	s.redirect(w, r, "/settings/prompt?msg=saved")
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
				s.renderSettings(w, r, http.StatusBadRequest, "models", st, generalFormOf(st), "选择的服务商不存在")
				return
			}
		}
		*ref = model.ModelRef{ProviderID: pid, Model: m}
	}
	if err := s.d.Store.SaveSettings(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	s.redirect(w, r, "/settings/models?msg=saved")
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
		s.renderSettings(w, r, http.StatusBadRequest, "providers", st, generalFormOf(st), "服务商需要名称和 http(s) 开头的 API 地址")
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
					s.renderSettings(w, r, http.StatusBadRequest, "providers", st, generalFormOf(st), "API 地址变了，请重新填写密钥")
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
	s.redirect(w, r, "/settings/providers?msg=saved")
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
	s.redirect(w, r, "/settings/providers?msg=deleted")
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
