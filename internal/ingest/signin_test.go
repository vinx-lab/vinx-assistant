package ingest

import (
	"context"
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
		{"纯数字", ilinktest.OwnerID, " 123456 ", "123456", true},
		{"登录 前缀", ilinktest.OwnerID, "登录 123456", "123456", true},
		{"对不上", ilinktest.OwnerID, "654321", "123456", false},
		{"没有验证码", ilinktest.OwnerID, "123456", "", false},
		{"不是验证码格式", ilinktest.OwnerID, "验证码 123456", "123456", false},
		{"陌生人", "stranger@im.wechat", "123456", "123456", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			ctx := context.Background()
			now := e.clk.Now()
			if c.code != "" {
				if err := e.st.CreateLoginCode(ctx, auth.CodeHash(c.code), "browser", now, now.Add(2*time.Minute)); err != nil {
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
				if len(sent) != 1 || sent[0].Text != SigninReply {
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
				if s.Text == SigninReply {
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
	e.st.CreateLoginCode(ctx, auth.CodeHash("111111"), "b1", now, now.Add(2*time.Minute))
	e.st.CreateLoginCode(ctx, auth.CodeHash("222222"), "b2", now, now.Add(2*time.Minute))
	e.handle(t, ilinktest.TextMsg(1, ilinktest.OwnerID, "222222"))
	e.handle(t, ilinktest.TextMsg(2, ilinktest.OwnerID, "222222")) // 已用过
	e.clk.Advance(2 * time.Minute)
	e.handle(t, ilinktest.TextMsg(3, ilinktest.OwnerID, "111111")) // 已过期
	if _, err := e.st.GetItemByMsgID(ctx, "1"); err == nil {
		t.Fatal("第一条应被当作验证码")
	}
	for _, id := range []string{"2", "3"} {
		if _, err := e.st.GetItemByMsgID(ctx, id); err != nil {
			t.Fatalf("消息 %s 应入库：%v", id, err)
		}
	}
}
