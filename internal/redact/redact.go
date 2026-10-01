// Package redact 对密钥和令牌打码，日志、错误、入库的原始 JSON 都要先经过这里。
package redact

import (
	"bytes"
	"encoding/json"
)

// Secret 只保留后 4 位；不足 8 个字符的全部打码。
func Secret(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) < 8 {
		return "***"
	}
	return "***" + string(r[len(r)-4:])
}

var sensitiveKeys = map[string]bool{
	"context_token": true,
	"bot_token":     true,
	"aes_key":       true,
	"aeskey":        true,
	"api_key":       true,
	"apikey":        true,
	"authorization": true,
}

// JSON 把敏感键的非空字符串值换成 "***"。输入不是合法 JSON 时返回固定占位，不回显原文。
func JSON(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return []byte(`{"redact_error":"invalid json"}`)
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		return []byte(`{"redact_error":"marshal failed"}`)
	}
	return out
}

func walk(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok && sensitiveKeys[k] && s != "" {
				t[k] = "***"
				continue
			}
			t[k] = walk(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = walk(t[i])
		}
		return t
	default:
		return v
	}
}
