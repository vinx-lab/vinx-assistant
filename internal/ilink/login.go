package ilink

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrQRExpired     = errors.New("ilink: 二维码已过期，请重新获取")
	ErrVerifyBlocked = errors.New("ilink: 验证码错误次数过多，请稍后再试")
	ErrAlreadyBound  = errors.New("ilink: 这个 Bot 已绑定在另一份凭证上")
)

type QR struct {
	Code    string // 轮询状态用
	Content string // 生成二维码图片的内容（一个链接）
}

type Login struct {
	BaseURL   string
	HTTP      *http.Client
	PollDelay time.Duration // 两次状态查询之间的间隔，防止服务端立即返回 wait 时空转
	Timeout   time.Duration // 整个扫码过程的上限
}

func NewLogin(hc *http.Client) *Login {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Login{BaseURL: DefaultBaseURL, HTTP: hc, PollDelay: 500 * time.Millisecond, Timeout: 8 * time.Minute}
}

func (l *Login) Start(ctx context.Context, oldTokens []string) (QR, error) {
	if oldTokens == nil {
		oldTokens = []string{}
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	var r struct {
		QRCode  string `json:"qrcode"`
		Content string `json:"qrcode_img_content"`
		Ret     int    `json:"ret"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := doJSON(ctx, l.HTTP, http.MethodPost, strings.TrimRight(l.BaseURL, "/")+"/ilink/bot/get_bot_qrcode?bot_type=3", "",
		map[string]any{"local_token_list": oldTokens}, &r); err != nil {
		return QR{}, err
	}
	if err := codeErr("get_bot_qrcode", r.Ret, r.ErrCode, r.ErrMsg); err != nil {
		return QR{}, err
	}
	if r.QRCode == "" {
		return QR{}, errors.New("ilink: 没有拿到二维码")
	}
	return QR{Code: r.QRCode, Content: r.Content}, nil
}

// Wait 轮询扫码状态直到确认。verify 在手机上要求输入数字时调用；onState 收到每个非 wait 状态。
func (l *Login) Wait(ctx context.Context, qr QR, verify func(context.Context) (string, error), onState func(string)) (Cred, error) {
	if l.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.Timeout)
		defer cancel()
	}
	base := strings.TrimRight(l.BaseURL, "/")
	code := ""
	for {
		if err := ctx.Err(); err != nil {
			return Cred{}, err
		}
		q := url.Values{"qrcode": {qr.Code}}
		if code != "" {
			q.Set("verify_code", code)
		}
		var st struct {
			Status       string `json:"status"`
			RedirectHost string `json:"redirect_host"`
			BotToken     string `json:"bot_token"`
			BotID        string `json:"ilink_bot_id"`
			UserID       string `json:"ilink_user_id"`
			BaseURL      string `json:"baseurl"`
		}
		pctx, cancel := context.WithTimeout(ctx, defaultLongPoll)
		err := doJSON(pctx, l.HTTP, http.MethodGet, base+"/ilink/bot/get_qrcode_status?"+q.Encode(), "", nil, &st)
		cancel()
		if err != nil { // 请求失败按 wait 处理（与上游一致）
			if ctx.Err() != nil {
				return Cred{}, ctx.Err()
			}
			sleep(ctx, 2*time.Second)
			continue
		}
		if st.Status != "" && st.Status != "wait" && onState != nil {
			onState(st.Status)
		}
		switch st.Status {
		case "scaned":
			code = ""
		case "need_verifycode":
			if verify == nil {
				return Cred{}, errors.New("ilink: 需要输入手机上显示的验证码，但没有提供输入方式")
			}
			if code, err = verify(ctx); err != nil {
				return Cred{}, err
			}
			continue
		case "scaned_but_redirect":
			if st.RedirectHost != "" {
				base = "https://" + st.RedirectHost
			}
		case "expired":
			return Cred{}, ErrQRExpired
		case "verify_code_blocked":
			return Cred{}, ErrVerifyBlocked
		case "binded_redirect":
			return Cred{}, ErrAlreadyBound
		case "confirmed":
			c := Cred{BotToken: st.BotToken, BotID: st.BotID, UserID: st.UserID, BaseURL: st.BaseURL}
			if c.BaseURL == "" {
				c.BaseURL = base
			}
			if c.BotToken == "" {
				return Cred{}, errors.New("ilink: 确认登录但没有返回 bot_token")
			}
			return c, nil
		}
		sleep(ctx, l.PollDelay)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
