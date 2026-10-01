package ingest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/auth"
	"github.com/vinx-lab/vinx-assistant/internal/ilink/ilinktest"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

func TestSigninCode(t *testing.T) {
	cases := []struct {
		name    string
		from    string
		text    string
		code    string // 网页上有效的验证码；空表示没有
		matched bool
	}{
		{"登录 前缀", ilinktest.OwnerID, " 登录 123456 ", "123456", true},
		{"全角空格", ilinktest.OwnerID, "登录\u3000123456", "123456", true},
		{"不带空格", ilinktest.OwnerID, "登录123456", "123456", true},
		{"纯数字不算", ilinktest.OwnerID, "123456", "123456", false},
		{"对不上", ilinktest.OwnerID, "登录 654321", "123456", false},
		{"没有验证码", ilinktest.OwnerID, "登录 123456", "", false},
		{"不是验证码格式", ilinktest.OwnerID, "验证码 123456", "123456", false},
		{"陌生人", "stranger@im.wechat", "登录 123456", "123456", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			ctx := context.Background()
			now := e.clk.Now()
			if c.code != "" {
				if err := e.st.CreateLoginCode(ctx, auth.CodeHash(c.code), "browser", store.LoginClient{IP: "192.0.2.7", UA: "Chrome / Windows"}, now, now.Add(2*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			e.handle(t, ilinktest.TextMsg(1, c.from, c.text))
			state, _ := e.st.PollLoginCode(ctx, "browser", now)
			_, itemErr := e.st.GetItem(ctx, 1)
			sent := e.srv.Sent()
			if c.matched {
				if state != store.LoginCodeConfirmed {
					t.Fatalf("应确认，state=%v", state)
				}
				if itemErr == nil {
					t.Fatal("验证码消息不应入库")
				}
				if len(sent) != 1 || sent[0].Text != "✓ 已登录网页：Chrome / Windows，192.0.2.7，09:00。不是你本人？回复「退出网页登录」" {
					t.Fatalf("回复 %+v", sent)
				}
				if seen, _ := e.st.Seen(ctx, "1"); !seen {
					t.Fatal("应记入 seen_msgs")
				}
				// 重复投递：不再回复，也不入库
				e.handle(t, ilinktest.TextMsg(1, c.from, c.text))
				if len(e.srv.Sent()) != 1 {
					t.Fatal("重复投递又回复了")
				}
				if _, err := e.st.GetItem(ctx, 1); err == nil {
					t.Fatal("重复投递入库了")
				}
				return
			}
			if state == store.LoginCodeConfirmed {
				t.Fatal("不应确认")
			}
			if c.from == ilinktest.OwnerID && itemErr != nil {
				t.Fatal("对不上时应照常入库")
			}
			for _, s := range sent {
				if strings.HasPrefix(s.Text, SigninReplyPrefix) {
					t.Fatal("不应回复已登录")
				}
			}
		})
	}
}

// 过期、已用过的验证码当普通消息处理。
func TestSigninCodeExpiredOrUsed(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	now := e.clk.Now()
	e.st.CreateLoginCode(ctx, auth.CodeHash("111111"), "b1", store.LoginClient{}, now, now.Add(2*time.Minute))
	e.st.CreateLoginCode(ctx, auth.CodeHash("222222"), "b2", store.LoginClient{}, now, now.Add(2*time.Minute))
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "登录 222222"))
	e.handle(t, ilinktest.TextMsg(2, ilinktest.OwnerID, "登录 222222")) // 已用过
	e.clk.Advance(2 * time.Minute)
	e.handle(t, ilinktest.TextMsg(3, ilinktest.OwnerID, "登录 111111")) // 已过期
	if _, err := e.st.GetItemByMsgID(ctx, "1"); err == nil {
		t.Fatal("第一条应被当作验证码")
	}
	for _, id := range []string{"2", "3"} {
		if _, err := e.st.GetItemByMsgID(ctx, id); err != nil {
			t.Fatalf("消息 %s 应入库：%v", id, err)
		}
	}
}

// 微信里「退出网页登录」：吊销全部网页会话和验证码，回复，不入库。
func TestSignoutWord(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	now := e.clk.Now()
	e.st.CreateSession(ctx, "s1", now, now.Add(time.Hour))
	e.st.CreateSession(ctx, "s2", now, now.Add(time.Hour))
	e.st.CreateLoginCode(ctx, auth.CodeHash("333333"), "b", store.LoginClient{}, now, now.Add(2*time.Minute))
	e.st.ConfirmLoginCode(ctx, auth.CodeHash("333333"), now) // 已确认还没换会话的也要作废

	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, " 退出网页登录 "))
	if n, _ := e.st.CountSessions(ctx); n != 0 {
		t.Fatalf("sessions = %d", n)
	}
	if st, _ := e.st.PollLoginCode(ctx, "b", now); st != store.LoginCodeGone {
		t.Fatal("验证码应作废")
	}
	if s := e.srv.Sent(); len(s) != 1 || s[0].Text != SignoutReply {
		t.Fatalf("回复 %+v", s)
	}
	if _, err := e.st.GetItem(ctx, 1); err == nil {
		t.Fatal("不应入库")
	}
	// 陌生人发的不算
	e.st.CreateSession(ctx, "s3", now, now.Add(time.Hour))
	e.handle(t, ilinktest.TextMsg(2, "stranger@im.wechat", "退出网页登录"))
	if n, _ := e.st.CountSessions(ctx); n != 1 {
		t.Fatal("陌生人不能退出网页登录")
	}
}
