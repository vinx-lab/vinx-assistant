// Package ilink 实现微信 ClawBot 的 iLink 协议（参考 Tencent/openclaw-weixin 2.4.9 的协议文档）。
// 只含协议，不含业务逻辑。
package ilink

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNoMediaKey：语音、文件、视频缺少 aes_key。上游遇到这种情况直接跳过，不当明文用。
var ErrNoMediaKey = errors.New("ilink: 媒体缺少 aes_key")

type ItemType int

const (
	TypeText  ItemType = 1
	TypeImage ItemType = 2
	TypeVoice ItemType = 3
	TypeFile  ItemType = 4
	TypeVideo ItemType = 5
)

type Cred struct {
	BotToken string    `json:"bot_token"`
	BotID    string    `json:"ilink_bot_id"`
	UserID   string    `json:"ilink_user_id"`
	BaseURL  string    `json:"baseurl"`
	LoginAt  time.Time `json:"login_at"`
}

type Updates struct {
	Ret     int       `json:"ret"`
	ErrCode int       `json:"errcode"`
	ErrMsg  string    `json:"errmsg"`
	Msgs    []Message `json:"msgs"`
	Buf     string    `json:"get_updates_buf"`
	// 服务端建议的下一次长轮询超时（毫秒），客户端照此调整（与上游 monitor 一致）
	LongPollTimeoutMs int `json:"longpolling_timeout_ms"`
}

type Message struct {
	Seq          int64           `json:"seq"`
	MessageID    json.Number     `json:"message_id"`
	FromUserID   string          `json:"from_user_id"`
	ToUserID     string          `json:"to_user_id"`
	CreateTimeMs int64           `json:"create_time_ms"`
	MessageType  int             `json:"message_type"` // 1 = 用户发来的
	ContextToken string          `json:"context_token"`
	Items        []Item          `json:"item_list"`
	Raw          json.RawMessage `json:"-"`
}

func (m *Message) UnmarshalJSON(b []byte) error {
	type plain Message
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*m = Message(p)
	m.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// ID 是去重用的消息编号。
func (m Message) ID() string {
	if s := m.MessageID.String(); s != "" && s != "0" {
		return s
	}
	return fmt.Sprintf("seq:%d:%d", m.Seq, m.CreateTimeMs)
}

func (m Message) CreatedAt() time.Time {
	if m.CreateTimeMs <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(m.CreateTimeMs)
}

type Media struct {
	EncryptQueryParam string `json:"encrypt_query_param"`
	AESKey            string `json:"aes_key"`
	FullURL           string `json:"full_url"`
}

type TextItem struct {
	Text string `json:"text"`
}

type ImageItem struct {
	AESKeyHex string `json:"aeskey"`
	Media     Media  `json:"media"`
	MidSize   int64  `json:"mid_size"`
	HDSize    int64  `json:"hd_size"`
}

type VoiceItem struct {
	Media    Media  `json:"media"`
	Text     string `json:"text"` // 微信自带的转写
	Playtime int    `json:"playtime"`
}

type FileItem struct {
	Media    Media       `json:"media"`
	FileName string      `json:"file_name"`
	MD5      string      `json:"md5"`
	Len      json.Number `json:"len"`
}

type VideoItem struct {
	Media Media `json:"media"`
}

type Item struct {
	Type   ItemType    `json:"type"`
	MsgID  string      `json:"msg_id,omitempty"`
	Text   *TextItem   `json:"text_item,omitempty"`
	Image  *ImageItem  `json:"image_item,omitempty"`
	Voice  *VoiceItem  `json:"voice_item,omitempty"`
	File   *FileItem   `json:"file_item,omitempty"`
	Video  *VideoItem  `json:"video_item,omitempty"`
	RefMsg *RefMessage `json:"ref_msg,omitempty"`
}

// RefMessage 是被引用的消息。新版微信可能只带 SvrID（上游 2.4.9-beta.0），
// 这时要用 SvrID 去查自己发过的消息（sendmessage 返回的 message_id）。
type RefMessage struct {
	MessageItem *Item           `json:"message_item,omitempty"`
	Title       string          `json:"title,omitempty"`
	SvrID       json.Number     `json:"svr_id,omitempty"`
	PartialText json.RawMessage `json:"partial_text,omitempty"`
}

// HasRef 表示这一项引用了别的消息。
func (it Item) HasRef() bool { return it.RefMsg != nil }

// Kind 返回附件类型名；文字返回空串。
func (it Item) Kind() string {
	switch it.Type {
	case TypeImage:
		return "image"
	case TypeVoice:
		return "voice"
	case TypeFile:
		return "file"
	case TypeVideo:
		return "video"
	}
	return ""
}

func (it Item) Media() *Media {
	switch {
	case it.Type == TypeImage && it.Image != nil:
		return &it.Image.Media
	case it.Type == TypeVoice && it.Voice != nil:
		return &it.Voice.Media
	case it.Type == TypeFile && it.File != nil:
		return &it.File.Media
	case it.Type == TypeVideo && it.Video != nil:
		return &it.Video.Media
	}
	return nil
}

// MediaKey 取 AES 密钥：图片优先用十六进制的 aeskey；其余 aes_key 是 base64，
// 解出来 16 字节就是密钥，32 字节则是十六进制字符串。返回 nil, nil 表示不加密。
func (it Item) MediaKey() ([]byte, error) {
	if it.Type == TypeImage && it.Image != nil && it.Image.AESKeyHex != "" {
		return hex.DecodeString(it.Image.AESKeyHex)
	}
	m := it.Media()
	if m == nil {
		return nil, nil
	}
	if m.AESKey == "" {
		if it.Type == TypeImage {
			return nil, nil // 图片没有密钥时按明文处理（与上游一致）
		}
		return nil, ErrNoMediaKey
	}
	raw, err := base64.StdEncoding.DecodeString(m.AESKey)
	if err != nil {
		return nil, fmt.Errorf("ilink: aes_key 不是 base64：%w", err)
	}
	switch len(raw) {
	case 16:
		return raw, nil
	case 32:
		return hex.DecodeString(string(raw))
	}
	return nil, fmt.Errorf("ilink: aes_key 长度异常：%d", len(raw))
}
