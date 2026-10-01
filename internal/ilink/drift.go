package ilink

import (
	"encoding/json"
	"fmt"
	"sort"
)

// 已知字段：上游 src/api/types.ts（2.4.9）里定义的，加上实测出现但上游类型没写的（标注「实测」）。
// 收到这之外的字段或消息类型，说明协议可能变了，Drift 会报告出来，提醒去对照上游。
var knownKeys = map[string]map[string]bool{
	"msg": set("seq", "message_id", "from_user_id", "to_user_id", "client_id", "create_time_ms",
		"update_time_ms", "delete_time_ms", "session_id", "group_id", "message_type", "message_state",
		"item_list", "context_token", "run_id",
		"root_id", "parent_id"), // 实测
	"item": set("type", "create_time_ms", "update_time_ms", "is_completed", "msg_id", "ref_msg",
		"text_item", "image_item", "voice_item", "file_item", "video_item",
		"tool_call_start_item", "tool_call_result_item",
		"button_item_list", "at_bot_username_list"), // 实测
	"text_item":  set("text"),
	"image_item": set("media", "thumb_media", "aeskey", "url", "mid_size", "thumb_size", "thumb_height", "thumb_width", "hd_size"),
	"voice_item": set("media", "encode_type", "bits_per_sample", "sample_rate", "playtime", "text"),
	"file_item":  set("media", "file_name", "md5", "len"),
	"video_item": set("media", "video_size", "play_length", "video_md5", "thumb_media", "thumb_size", "thumb_height", "thumb_width"),
	"ref_msg":    set("message_item", "title", "svr_id", "partial_text"),
	"media":      set("encrypt_query_param", "aes_key", "encrypt_type", "full_url"),
}

// 上游定义的消息项类型：0 是 MessageItemType.NONE（引用里的 message_item 用它），
// 1–5 是内容，11、12 是 agent 工具调用进度（我们不处理）。
var knownItemTypes = map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 11: true, 12: true}

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// Drift 返回一条原始消息里协议之外的东西，如 "msg.new_field"、"item.type=7"、"image_item.foo"。结果已排序、去重。
func Drift(raw []byte) []string {
	found := map[string]bool{}
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return []string{"msg.<invalid json>"}
	}
	checkKeys("msg", msg, found)
	var items []map[string]json.RawMessage
	json.Unmarshal(msg["item_list"], &items)
	for _, it := range items {
		driftItem(it, found)
	}
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func driftItem(it map[string]json.RawMessage, found map[string]bool) {
	checkKeys("item", it, found)
	var typ int
	if json.Unmarshal(it["type"], &typ) == nil && !knownItemTypes[typ] {
		found[fmt.Sprintf("item.type=%d", typ)] = true
	}
	for _, sub := range []string{"text_item", "image_item", "voice_item", "file_item", "video_item"} {
		var obj map[string]json.RawMessage
		if json.Unmarshal(it[sub], &obj) != nil || obj == nil {
			continue
		}
		checkKeys(sub, obj, found)
		for _, mk := range []string{"media", "thumb_media"} {
			var media map[string]json.RawMessage
			if json.Unmarshal(obj[mk], &media) == nil && media != nil {
				checkKeys("media", media, found)
			}
		}
	}
	var ref map[string]json.RawMessage
	if json.Unmarshal(it["ref_msg"], &ref) == nil && ref != nil {
		checkKeys("ref_msg", ref, found)
		var inner map[string]json.RawMessage
		if json.Unmarshal(ref["message_item"], &inner) == nil && inner != nil {
			driftItem(inner, found)
		}
	}
}

func checkKeys(scope string, obj map[string]json.RawMessage, found map[string]bool) {
	for k := range obj {
		if !knownKeys[scope][k] {
			found[scope+"."+k] = true
		}
	}
}
