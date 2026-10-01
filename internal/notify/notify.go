// Package notify 是推送渠道接口。第一期只有微信实现，以后加 ntfy 等渠道时实现同一个接口。
package notify

import (
	"context"
	"errors"
	"log/slog"

	"github.com/vinx-lab/vinx-assistant/internal/session"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

type Notifier interface {
	Send(ctx context.Context, text string) error
}

var ErrNoContext = errors.New("notify: 还没收到过微信消息，没有 context_token，无法主动发送")

type WeChat struct {
	sess *session.Session
	st   *store.Store
	Log  *slog.Logger // 可选；nil 时用 slog.Default()
}

func NewWeChat(sess *session.Session, st *store.Store) *WeChat { return &WeChat{sess: sess, st: st} }

// Send 用最近一次入站消息的 context_token 发给主人，成功后把 (message_id, 正文) 记进 sent_msgs，
// 供以后还原只带 svr_id 的引用。发送失败原样返回，由调用方决定是否补发；
// 发送已成功但记录失败只记日志、返回 nil，否则调用方会把已发出的消息当失败重发。
func (w *WeChat) Send(ctx context.Context, text string) error {
	c, err := w.sess.Client(ctx)
	if err != nil {
		return err
	}
	cred, _, err := w.sess.Cred(ctx)
	if err != nil {
		return err
	}
	tok, _, ok, err := w.sess.Context(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoContext
	}
	id, err := c.SendText(ctx, cred.UserID, tok, text)
	if err != nil {
		return err
	}
	if err := w.st.RecordSent(ctx, id, text); err != nil {
		log := w.Log
		if log == nil {
			log = slog.Default()
		}
		log.Warn("消息已发出，但记录 sent_msgs 失败", "msg_id", id, "err", err)
	}
	return nil
}
