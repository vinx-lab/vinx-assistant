package ilink

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	DefaultBaseURL    = "https://ilinkai.weixin.qq.com"
	DefaultCDNBaseURL = "https://novac2c.cdn.weixin.qq.com/c2c"
	ChannelVersion    = "2.4.9" // 对齐的上游版本，见 UPSTREAM.md；同步上游时一起改
	AppID             = "bot"   // 沿用官方插件的值，独立客户端是否获准接入官方未答复（上游 issue #265）
	BotAgent          = "VinxAssistant/0.1"

	defaultLongPoll = 35 * time.Second // 上游 DEFAULT_LONG_POLL_TIMEOUT_MS
	sendTimeout     = 15 * time.Second // 上游 DEFAULT_API_TIMEOUT_MS
	downloadTimeout = 120 * time.Second
	maxMediaBytes   = 256 << 20
)

var ErrSessionExpired = errors.New("ilink: 登录凭证失效（-14），需要重新扫码")

type APIError struct {
	Op      string
	Ret     int
	ErrCode int
	Msg     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ilink %s: ret=%d errcode=%d %s", e.Op, e.Ret, e.ErrCode, e.Msg)
}

func codeErr(op string, ret, errcode int, msg string) error {
	code := ret
	if code == 0 {
		code = errcode
	}
	switch code {
	case 0:
		return nil
	case -14:
		return ErrSessionExpired
	}
	return &APIError{Op: op, Ret: ret, ErrCode: errcode, Msg: msg}
}

type Client struct {
	BaseURL    string
	CDNBaseURL string
	token      string
	http       *http.Client
	pollMs     atomic.Int64 // 下一次长轮询超时，按服务端的 longpolling_timeout_ms 调整
}

func New(cred Cred, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	base := cred.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	c := &Client{BaseURL: strings.TrimRight(base, "/"), CDNBaseURL: DefaultCDNBaseURL, token: cred.BotToken, http: hc}
	c.pollMs.Store(defaultLongPoll.Milliseconds())
	return c
}

// PollTimeout 是下一次长轮询的超时。
func (c *Client) PollTimeout() time.Duration {
	return time.Duration(c.pollMs.Load()) * time.Millisecond
}

func clientVersion() string {
	parts := strings.Split(ChannelVersion, ".")
	var v int
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		v = v<<8 | n
	}
	return strconv.Itoa(v)
}

func setHeaders(req *http.Request, token string, post bool) {
	req.Header.Set("iLink-App-Id", AppID)
	req.Header.Set("iLink-App-ClientVersion", clientVersion())
	if post {
		var b [4]byte
		rand.Read(b[:])
		uin := strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[:])), 10)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("AuthorizationType", "ilink_bot_token")
		req.Header.Set("X-WECHAT-UIN", base64.StdEncoding.EncodeToString([]byte(uin)))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func doJSON(ctx context.Context, hc *http.Client, method, u, token string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	setHeaders(req, token, body != nil)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ilink: HTTP %d", resp.StatusCode)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	return json.Unmarshal(data, out)
}

func baseInfo() map[string]string {
	return map[string]string{"channel_version": ChannelVersion, "bot_agent": BotAgent}
}

// GetUpdates 长轮询收消息。服务端无消息时会挂起直到超时，这时返回空结果、游标不变（与上游一致）。
func (c *Client) GetUpdates(ctx context.Context, buf string) (*Updates, error) {
	pctx, cancel := context.WithTimeout(ctx, c.PollTimeout())
	defer cancel()
	var env struct {
		Updates
		Msgs []json.RawMessage `json:"msgs"`
	}
	err := doJSON(pctx, c.http, http.MethodPost, c.BaseURL+"/ilink/bot/getupdates", c.token,
		map[string]any{"get_updates_buf": buf, "base_info": baseInfo()}, &env)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return &Updates{Buf: buf}, nil
		}
		return nil, err
	}
	u := env.Updates
	u.Msgs = nil
	for _, raw := range env.Msgs {
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil {
			u.Undecodable = append(u.Undecodable, append(json.RawMessage(nil), raw...))
			continue
		}
		u.Msgs = append(u.Msgs, m)
	}
	if u.LongPollTimeoutMs > 0 {
		c.pollMs.Store(int64(u.LongPollTimeoutMs))
	}
	if err := codeErr("getupdates", u.Ret, u.ErrCode, u.ErrMsg); err != nil {
		return nil, err
	}
	return &u, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// SendText 给用户发一条文字，返回服务端的 message_id（之后用户引用这条消息时，引用里的 svr_id 就是它；
// 服务端没返回时为空串）。必须带最近一条入站消息的 context_token，否则会被拒（ret=-2）。
func (c *Client) SendText(ctx context.Context, to, contextToken, text string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	msg := map[string]any{
		"from_user_id":  "",
		"to_user_id":    to,
		"client_id":     "vinx-" + randHex(8),
		"message_type":  2,
		"message_state": 2,
		"item_list":     []any{map[string]any{"type": 1, "text_item": map[string]string{"text": text}}},
	}
	if contextToken != "" {
		msg["context_token"] = contextToken
	}
	var st struct {
		MessageID ID     `json:"message_id"`
		Ret       int    `json:"ret"`
		ErrCode   int    `json:"errcode"`
		ErrMsg    string `json:"errmsg"`
	}
	if err := doJSON(ctx, c.http, http.MethodPost, c.BaseURL+"/ilink/bot/sendmessage", c.token,
		map[string]any{"msg": msg, "base_info": baseInfo()}, &st); err != nil {
		return "", err
	}
	if err := codeErr("sendmessage", st.Ret, st.ErrCode, st.ErrMsg); err != nil {
		return "", err
	}
	return st.MessageID.String(), nil
}

func (c *Client) notify(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	var st Updates
	if err := doJSON(ctx, c.http, http.MethodPost, c.BaseURL+path, c.token, map[string]any{"base_info": baseInfo()}, &st); err != nil {
		return err
	}
	return codeErr(path, st.Ret, st.ErrCode, st.ErrMsg)
}

func (c *Client) NotifyStart(ctx context.Context) error {
	return c.notify(ctx, "/ilink/bot/msg/notifystart")
}
func (c *Client) NotifyStop(ctx context.Context) error {
	return c.notify(ctx, "/ilink/bot/msg/notifystop")
}

// Download 下载并解密一项媒体。
func (c *Client) Download(ctx context.Context, it Item) ([]byte, error) {
	m := it.Media()
	if m == nil {
		return nil, errors.New("ilink: 这一项没有媒体")
	}
	u := m.FullURL
	if u == "" && m.EncryptQueryParam != "" {
		u = c.CDNBaseURL + "/download?encrypted_query_param=" + url.QueryEscape(m.EncryptQueryParam)
	}
	if u == "" {
		return nil, errors.New("ilink: 媒体没有下载地址")
	}
	key, err := it.MediaKey() // 先检查密钥：语音、文件、视频缺密钥时不下载（与上游一致）
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ilink: 下载媒体 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMediaBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMediaBytes {
		return nil, fmt.Errorf("ilink: 媒体超过 %d MB", maxMediaBytes>>20)
	}
	if key == nil {
		return data, nil
	}
	return DecryptECB(data, key)
}
