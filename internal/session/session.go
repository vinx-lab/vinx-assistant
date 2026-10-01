// Package session 管微信登录态：凭证、连接状态、最近一次 context_token。都存在 kv 表里，
// 这样 login 子命令和网页扫码写进去的新凭证，正在运行的 serve 下一轮就能用上。
//
// 凭证失效（-14）的处理与上游 session-guard 一致：暂停所有收发 1 小时，到点自动重试，
// 不立即要求重新扫码；连续暂停的次数记在 StaleCount 里，网页据此提示是否该重新扫码。
package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
	"github.com/vinx-lab/vinx-assistant/internal/ilink"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

const (
	StatusOK     = "ok"
	StatusPaused = "paused"
	StatusNoCred = "no_cred"

	// PauseDuration 对齐上游 SESSION_PAUSE_DURATION_MS。
	PauseDuration = time.Hour
	// 上游登录时最多上送最近 10 个 bot_token（local_token_list）。
	maxTokenHistory = 10

	keyCred        = "ilink.cred"
	keyPausedUntil = "ilink.paused_until"
	keyStaleCount  = "ilink.stale_count"
	keyTokens      = "ilink.token_history"
	keyContext     = "ilink.context_token"
	keyContextAt   = "ilink.context_token_at"
)

var (
	ErrNoCred = errors.New("session: 还没有登录微信")
	ErrPaused = errors.New("session: 微信凭证失效（-14），暂停中，到点自动重试")
)

type Session struct {
	st        *store.Store
	clk       clock.Clock
	NewClient func(ilink.Cred) *ilink.Client

	mu     sync.Mutex
	client *ilink.Client
	token  string
}

func New(st *store.Store, hc *http.Client, clk clock.Clock) *Session {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Session{st: st, clk: clk, NewClient: func(c ilink.Cred) *ilink.Client { return ilink.New(c, hc) }}
}

func (s *Session) Cred(ctx context.Context) (ilink.Cred, bool, error) {
	raw, ok, err := s.st.GetKV(ctx, keyCred)
	if err != nil || !ok {
		return ilink.Cred{}, false, err
	}
	var c ilink.Cred
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return ilink.Cred{}, false, err
	}
	return c, c.BotToken != "", nil
}

// SaveCred 保存新凭证，清除暂停状态，并把 token 记进历史（扫码时上送）。
func (s *Session) SaveCred(ctx context.Context, c ilink.Cred) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	hist := append([]string{c.BotToken}, s.TokenHistory(ctx)...)
	uniq := hist[:0]
	seen := map[string]bool{}
	for _, t := range hist {
		if t != "" && !seen[t] {
			seen[t] = true
			uniq = append(uniq, t)
		}
	}
	if len(uniq) > maxTokenHistory {
		uniq = uniq[:maxTokenHistory]
	}
	hb, _ := json.Marshal(uniq)
	for k, v := range map[string]string{keyCred: string(b), keyTokens: string(hb), keyPausedUntil: "0", keyStaleCount: "0"} {
		if err := s.st.SetKV(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

// TokenHistory 是最近用过的 bot_token，新的在前，最多 10 个。
func (s *Session) TokenHistory(ctx context.Context) []string {
	raw, ok, _ := s.st.GetKV(ctx, keyTokens)
	var list []string
	if ok {
		json.Unmarshal([]byte(raw), &list)
	}
	return list
}

func (s *Session) kvInt(ctx context.Context, key string) int64 {
	v, _, _ := s.st.GetKV(ctx, key)
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// PausedUntil 返回暂停截止时间；没有暂停时为零值。
func (s *Session) PausedUntil(ctx context.Context) time.Time {
	n := s.kvInt(ctx, keyPausedUntil)
	if n == 0 || s.clk.Now().Unix() >= n {
		return time.Time{}
	}
	return time.Unix(n, 0).In(clock.Zone)
}

// StaleCount 是连续遇到 -14 的次数；成功收到一次 getupdates 就清零。
func (s *Session) StaleCount(ctx context.Context) int { return int(s.kvInt(ctx, keyStaleCount)) }

// Pause 在收到 -14 时调用：暂停 1 小时，计数加一。
func (s *Session) Pause(ctx context.Context) error {
	until := s.clk.Now().Add(PauseDuration).Unix()
	if err := s.st.SetKV(ctx, keyPausedUntil, strconv.FormatInt(until, 10)); err != nil {
		return err
	}
	return s.st.SetKV(ctx, keyStaleCount, strconv.Itoa(s.StaleCount(ctx)+1))
}

// MarkHealthy 在 getupdates 成功后调用，清零 -14 计数。
func (s *Session) MarkHealthy(ctx context.Context) error {
	if s.StaleCount(ctx) == 0 {
		return nil
	}
	return s.st.SetKV(ctx, keyStaleCount, "0")
}

func (s *Session) Status(ctx context.Context) string {
	if _, ok, _ := s.Cred(ctx); !ok {
		return StatusNoCred
	}
	if !s.PausedUntil(ctx).IsZero() {
		return StatusPaused
	}
	return StatusOK
}

// Client 返回当前凭证对应的客户端；凭证换了就重建。暂停期间返回 ErrPaused（收发都停，与上游一致）。
func (s *Session) Client(ctx context.Context) (*ilink.Client, error) {
	c, ok, err := s.Cred(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNoCred
	}
	if !s.PausedUntil(ctx).IsZero() {
		return nil, ErrPaused
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil || s.token != c.BotToken {
		s.client, s.token = s.NewClient(c), c.BotToken
	}
	return s.client, nil
}

func (s *Session) RememberContext(ctx context.Context, token string, at time.Time) error {
	if err := s.st.SetKV(ctx, keyContext, token); err != nil {
		return err
	}
	return s.st.SetKV(ctx, keyContextAt, strconv.FormatInt(at.Unix(), 10))
}

func (s *Session) Context(ctx context.Context) (string, time.Time, bool, error) {
	tok, ok, err := s.st.GetKV(ctx, keyContext)
	if err != nil || !ok || tok == "" {
		return "", time.Time{}, false, err
	}
	var at time.Time
	if n := s.kvInt(ctx, keyContextAt); n > 0 {
		at = time.Unix(n, 0).In(clock.Zone)
	}
	return tok, at, true, nil
}
