// Package ilinktest 是测试用的假 iLink 后端：消息队列、媒体 CDN、发送记录、二维码登录。
package ilinktest

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/vinx-lab/vinx-assistant/internal/ilink"
)

const (
	OwnerID = "owner@im.wechat"
	BotID   = "bot@im.bot"
	Token   = "test-bot-token"
)

type Sent struct {
	To, ContextToken, Text, MsgID string
}

type Server struct {
	*httptest.Server
	mu          sync.Mutex
	queue       []json.RawMessage
	wake        chan struct{}
	sent        []Sent
	sendRet     int
	updatesRet  int
	bufs        []string
	media       map[string][]byte
	mediaHook   func(param string)
	loginStates []string
	VerifyCode  string // need_verifycode 之后要求带上的验证码
	LongPoll    time.Duration
	// PollHintMs 非零时在 getupdates 响应里返回 longpolling_timeout_ms
	PollHintMs int
}

func New() *Server {
	s := &Server{wake: make(chan struct{}, 1), media: map[string][]byte{}, VerifyCode: "1234", LongPoll: 100 * time.Millisecond}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ilink/bot/getupdates", s.getUpdates)
	mux.HandleFunc("POST /ilink/bot/sendmessage", s.sendMessage)
	mux.HandleFunc("POST /ilink/bot/msg/notifystart", s.ok)
	mux.HandleFunc("POST /ilink/bot/msg/notifystop", s.ok)
	mux.HandleFunc("GET /download", s.download)
	mux.HandleFunc("POST /ilink/bot/get_bot_qrcode", s.getQR)
	mux.HandleFunc("GET /ilink/bot/get_qrcode_status", s.qrStatus)
	s.Server = httptest.NewServer(mux)
	return s
}

func (s *Server) Cred() ilink.Cred {
	return ilink.Cred{BotToken: Token, BotID: BotID, UserID: OwnerID, BaseURL: s.URL}
}

func (s *Server) Push(raw string) {
	s.mu.Lock()
	s.queue = append(s.queue, json.RawMessage(raw))
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Server) AddMedia(param string, cipher []byte) {
	s.mu.Lock()
	s.media[param] = cipher
	s.mu.Unlock()
}

// SetMediaHook 设置媒体下载请求进入时调用的函数（在返回数据之前），测试用它制造慢下载。
// hook 可以阻塞；nil 取消。
func (s *Server) SetMediaHook(hook func(param string)) {
	s.mu.Lock()
	s.mediaHook = hook
	s.mu.Unlock()
}

func (s *Server) Sent() []Sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sent(nil), s.sent...)
}

func (s *Server) Bufs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bufs...)
}

func (s *Server) SetSendRet(ret int)    { s.mu.Lock(); s.sendRet = ret; s.mu.Unlock() }
func (s *Server) SetUpdatesRet(ret int) { s.mu.Lock(); s.updatesRet = ret; s.mu.Unlock() }

// SetLoginStates 设置 get_qrcode_status 依次返回的状态；最后一个是 confirmed 时返回凭证。
func (s *Server) SetLoginStates(states ...string) {
	s.mu.Lock()
	s.loginStates = states
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+Token && r.Header.Get("iLink-App-Id") == "bot"
}

func (s *Server) ok(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]int{"ret": 0}) }

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Buf string `json:"get_updates_buf"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	s.bufs = append(s.bufs, req.Buf)
	ret := s.updatesRet
	empty := len(s.queue) == 0
	s.mu.Unlock()
	if ret != 0 {
		writeJSON(w, map[string]any{"ret": ret, "errmsg": "fake error"})
		return
	}
	if empty {
		select {
		case <-s.wake:
		case <-time.After(s.LongPoll):
		case <-r.Context().Done():
			return
		}
	}
	s.mu.Lock()
	msgs := s.queue
	s.queue = nil
	n := len(s.bufs)
	s.mu.Unlock()
	select { // 排掉没人等时 Push 留下的唤醒标记，免得之后的空轮询立刻返回
	case <-s.wake:
	default:
	}
	resp := map[string]any{"ret": 0, "msgs": msgs, "get_updates_buf": fmt.Sprintf("buf-%d", n)}
	if s.PollHintMs > 0 {
		resp["longpolling_timeout_ms"] = s.PollHintMs
	}
	writeJSON(w, resp)
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Msg struct {
			To       string `json:"to_user_id"`
			Ctx      string `json:"context_token"`
			ItemList []struct {
				TextItem struct {
					Text string `json:"text"`
				} `json:"text_item"`
			} `json:"item_list"`
		} `json:"msg"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	ret := s.sendRet
	id := ""
	if ret == 0 && len(req.Msg.ItemList) > 0 {
		id = fmt.Sprintf("900000000000000%04d", len(s.sent)+1) // 服务端 message_id，引用时作为 svr_id 出现
		s.sent = append(s.sent, Sent{To: req.Msg.To, ContextToken: req.Msg.Ctx, Text: req.Msg.ItemList[0].TextItem.Text, MsgID: id})
	}
	s.mu.Unlock()
	if ret != 0 {
		writeJSON(w, map[string]any{"ret": ret})
		return
	}
	writeJSON(w, map[string]any{"ret": 0, "message_id": id})
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	param := r.URL.Query().Get("encrypted_query_param")
	s.mu.Lock()
	hook := s.mediaHook
	s.mu.Unlock()
	if hook != nil {
		hook(param)
	}
	s.mu.Lock()
	data, ok := s.media[param]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(data)
}

func (s *Server) getQR(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"qrcode": "qr-1", "qrcode_img_content": "https://example.invalid/qr/qr-1"})
}

func (s *Server) qrStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.loginStates) == 0 {
		writeJSON(w, map[string]string{"status": "expired"})
		return
	}
	st := s.loginStates[0]
	if st == "confirmed" && r.URL.Query().Get("verify_code") != "" && r.URL.Query().Get("verify_code") != s.VerifyCode {
		writeJSON(w, map[string]string{"status": "verify_code_blocked"})
		return
	}
	s.loginStates = s.loginStates[1:]
	if st == "confirmed" {
		writeJSON(w, map[string]string{"status": st, "bot_token": Token, "ilink_bot_id": BotID, "ilink_user_id": OwnerID, "baseurl": s.URL})
		return
	}
	writeJSON(w, map[string]string{"status": st})
}

// ---- 消息构造 ----

func envelope(id int64, from string, items string) string {
	return fmt.Sprintf(`{"seq":%d,"message_id":%d,"from_user_id":%q,"to_user_id":%q,"create_time_ms":1790825694222,"message_type":1,"message_state":2,"context_token":"ctx-%d","item_list":[%s]}`,
		id, id, from, BotID, id, items)
}

func TextMsg(id int64, from, text string) string {
	t, _ := json.Marshal(text)
	return envelope(id, from, fmt.Sprintf(`{"type":1,"text_item":{"text":%s}}`, t))
}

// RefTextMsg 是带原文的引用（旧版微信：ref_msg.message_item 里有被引用的文字）。
func RefTextMsg(id int64, from, text, refText string) string {
	t, _ := json.Marshal(text)
	r, _ := json.Marshal(refText)
	return envelope(id, from, fmt.Sprintf(`{"type":1,"text_item":{"text":%s},"ref_msg":{"message_item":{"type":1,"text_item":{"text":%s}}}}`, t, r))
}

// RefSvrMsg 是只带 svr_id 的引用（新版微信，上游 2.4.9-beta.0 起处理）。
func RefSvrMsg(id int64, from, text, svrID string) string {
	t, _ := json.Marshal(text)
	return envelope(id, from, fmt.Sprintf(`{"type":1,"text_item":{"text":%s},"ref_msg":{"svr_id":%q}}`, t, svrID))
}

// RefMsgIDMsg 是实测的新版引用：ref_msg 只有 message_item{type:0,msg_id}，msg_id 是被引用消息的服务端 message_id。
func RefMsgIDMsg(id int64, from, text, quotedMsgID string) string {
	t, _ := json.Marshal(text)
	return envelope(id, from, fmt.Sprintf(`{"type":1,"text_item":{"text":%s},"ref_msg":{"message_item":{"type":0,"msg_id":%q,"create_time_ms":1790825694222,"update_time_ms":1790825694222,"is_completed":true,"at_bot_username_list":[],"button_item_list":[]}}}`, t, quotedMsgID))
}

// ImageMsg：图片用十六进制 aeskey，媒体走 encrypt_query_param。
func ImageMsg(id int64, from, param string, key []byte) string {
	return envelope(id, from, fmt.Sprintf(`{"type":2,"image_item":{"aeskey":%q,"media":{"encrypt_query_param":%q}}}`, hex.EncodeToString(key), param))
}

// VoiceMsg：aes_key 是「十六进制字符串再 base64」（32 字节那种）。
func VoiceMsg(id int64, from, param string, key []byte, text string) string {
	k := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(key)))
	t, _ := json.Marshal(text)
	return envelope(id, from, fmt.Sprintf(`{"type":3,"voice_item":{"media":{"encrypt_query_param":%q,"aes_key":%q},"text":%s}}`, param, k, t))
}

// FileMsg：aes_key 是 16 字节原始密钥的 base64。
func FileMsg(id int64, from, param string, key []byte, name, md5hex string) string {
	k := base64.StdEncoding.EncodeToString(key)
	n, _ := json.Marshal(name)
	return envelope(id, from, fmt.Sprintf(`{"type":4,"file_item":{"media":{"encrypt_query_param":%q,"aes_key":%q},"file_name":%s,"md5":%q,"len":"3"}}`, param, k, n, md5hex))
}
