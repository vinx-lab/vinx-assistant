package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/redact"
)

// generalForm 是设置页「整理与规则」表单的原始文本，校验失败时原样回显。
type generalForm struct {
	TokenLimit, BatchTimes, DigestTime  string
	Images                              bool
	Prefixes, Medium, Deep, ActionWords string
}

func generalFormOf(st model.Settings) generalForm {
	return generalForm{
		TokenLimit:  strconv.FormatInt(st.AI.DailyTokenLimit, 10),
		BatchTimes:  strings.Join(st.Schedule.BatchTimes, " "),
		DigestTime:  st.Schedule.DigestTime,
		Images:      st.AI.Images,
		Prefixes:    FormatPrefixes(st.Rules.Prefixes),
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
	st.AI.DailyTokenLimit, st.AI.Images = limit, f.Images
	st.Schedule.BatchTimes, st.Schedule.DigestTime = times, digest
	st.Rules.Prefixes, st.Rules.ActionWords = prefixes, words
	st.Rules.MediumKeywords, st.Rules.DeepKeywords = ParseWords(f.Medium), ParseWords(f.Deep)
	return nil
}

type providerView struct {
	ID, Name, BaseURL, KeyTail string
}

type settingsData struct {
	Page
	General   generalForm
	Providers []providerView
	AI        model.AI
	Levels    []levelView
}

type levelView struct {
	Level      model.Level
	Name       string
	ProviderID string
	Model      string
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, st model.Settings, g generalForm, errMsg string) {
	d := settingsData{Page: s.page(r, "设置", "settings"), General: g, AI: st.AI}
	d.Error = errMsg
	for _, p := range st.AI.Providers {
		d.Providers = append(d.Providers, providerView{ID: p.ID, Name: p.Name, BaseURL: p.BaseURL, KeyTail: redact.Secret(p.APIKey)})
	}
	for _, l := range []struct {
		lv   model.Level
		name string
	}{{model.LevelLight, "轻量"}, {model.LevelMedium, "中等"}, {model.LevelDeep, "深度"}} {
		ref := st.AI.Ref(l.lv)
		d.Levels = append(d.Levels, levelView{Level: l.lv, Name: l.name, ProviderID: ref.ProviderID, Model: ref.Model})
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
		Deep: r.FormValue("deep_keywords"), ActionWords: r.FormValue("action_words")}
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

func (s *Server) settingsModels(w http.ResponseWriter, r *http.Request) {
	st, err := s.d.Store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	refs := map[model.Level]*model.ModelRef{model.LevelLight: &st.AI.Light, model.LevelMedium: &st.AI.Medium, model.LevelDeep: &st.AI.Deep}
	for lv, ref := range refs {
		pid, m := r.FormValue(string(lv)+"_provider"), strings.TrimSpace(r.FormValue(string(lv)+"_model"))
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

// providerSave 新增或修改服务商。修改时密钥留空表示不改。
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

// providerModels 拉取服务商的模型列表（POST：会带着密钥访问外部服务）。错误信息里的密钥打码。
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
	writeJSON(w, http.StatusOK, map[string][]string{"models": models})
}
