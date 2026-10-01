package ilink

import (
	"bytes"
	"encoding/json"
	"path"
	"regexp"
)

// Sanitize 把一条真实消息的 JSON 变成可以提交进仓库的测试样例：保留全部字段和类型，
// 非空字符串一律换成按键名决定的占位值，ID 和时间换成固定值。结果对同一输入稳定，
// 对已经处理过的样例再处理一次不变（测试用这一点确认提交的样例都处理过）。
func Sanitize(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(sanitize("", v)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var placeholders = map[string]string{
	"from_user_id": "owner@im.wechat",
	"to_user_id":   "bot@im.bot",
	"text":         "示例文字",
	"title":        "示例摘要",
	"svr_id":       "9000000000000000001",
	"msg_id":       "v1:1000000000000000001",
	"len":          "1024", // 线上是十进制字符串
}

var fixedNumbers = map[string]string{
	"message_id":     "7000000000000000001",
	"seq":            "1",
	"create_time_ms": "1790000000000",
	"update_time_ms": "1790000000000",
}

// structural 是描述协议结构、不含个人信息的数字字段，原样保留；其余非零数字一律换成固定值
// （尺寸、时长、各种 ID 都可能泄露内容或身份）。
var structural = map[string]bool{
	"type": true, "message_type": true, "message_state": true,
	"encode_type": true, "bits_per_sample": true, "sample_rate": true,
}

var extRe = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)

func sanitize(key string, v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = sanitize(k, val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = sanitize(key, t[i])
		}
		return t
	case string:
		if t == "" {
			return t
		}
		if key == "file_name" {
			if ext := path.Ext(t); extRe.MatchString(ext) {
				return "示例" + ext
			}
			return "示例"
		}
		if p, ok := placeholders[key]; ok {
			return p
		}
		return "<" + key + ">"
	case json.Number:
		if f, err := t.Float64(); err == nil && f == 0 {
			return t
		}
		if structural[key] {
			return t
		}
		if n, ok := fixedNumbers[key]; ok {
			return json.Number(n)
		}
		return json.Number("1")
	}
	return v
}
