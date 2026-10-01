package ilink

import (
	"reflect"
	"testing"
)

func TestDriftKnownMessageIsClean(t *testing.T) {
	raw := `{"seq":1,"message_id":1,"from_user_id":"u","to_user_id":"b","client_id":"c","create_time_ms":1,"update_time_ms":1,"delete_time_ms":0,"session_id":"","group_id":"","message_type":1,"message_state":2,"context_token":"t","root_id":0,"parent_id":0,
	"item_list":[{"type":2,"create_time_ms":1,"update_time_ms":1,"is_completed":true,"msg_id":"v1:1","button_item_list":[],"at_bot_username_list":[],
	"image_item":{"aeskey":"00","media":{"encrypt_query_param":"p","aes_key":"k","full_url":"u"},"mid_size":1,"thumb_size":1,"thumb_height":1,"thumb_width":1,"hd_size":0}},
	{"type":1,"text_item":{"text":"x"},"ref_msg":{"title":"t","svr_id":"9","message_item":{"type":1,"text_item":{"text":"y"}}}}]}`
	if got := Drift([]byte(raw)); len(got) != 0 {
		t.Fatalf("drift = %v", got)
	}
}

func TestDriftReportsUnknown(t *testing.T) {
	raw := `{"message_id":1,"new_top":1,"item_list":[{"type":7,"sticker_item":{}},{"type":2,"image_item":{"media":{"cdn_v2":1},"hd_url":"x"}},{"type":1,"ref_msg":{"svr_id":"1","quote_v3":{}}}]}`
	want := []string{"image_item.hd_url", "item.sticker_item", "item.type=7", "media.cdn_v2", "msg.new_top", "ref_msg.quote_v3"}
	if got := Drift([]byte(raw)); !reflect.DeepEqual(got, want) {
		t.Fatalf("drift = %v, want %v", got, want)
	}
}
