package ilink

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 样例来自真实消息，经 Sanitize 处理后提交。新增样例：
//
//	go run ./tools/sanitize-fixture < 原始.json > internal/ilink/testdata/messages/<名字>.json
//
// 这个测试保证：样例能按当前结构解析、没有协议之外的字段、已经脱敏。
func TestFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/messages/*.json")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if m.MessageType != 1 || len(m.Items) == 0 {
			t.Errorf("%s: parsed %+v", f, m)
		}
		if d := Drift(raw); len(d) != 0 {
			t.Errorf("%s: drift %v（协议有新字段：先对照上游，再更新 drift.go 的已知字段）", f, d)
		}
		again, err := Sanitize(raw)
		if err != nil || !bytes.Equal(bytes.TrimSpace(again), bytes.TrimSpace(raw)) {
			t.Errorf("%s: 样例没有脱敏（Sanitize 后内容变化），请用 tools/sanitize-fixture 重新生成", f)
		}
	}
}

func TestSanitizeRemovesPersonalData(t *testing.T) {
	raw := []byte(`{"message_id":7511267372956042376,"from_user_id":"o9cq809WYMEy","context_token":"secret","item_list":[{"type":4,"file_item":{"file_name":"附件3-5 一览表.pdf","md5":"e427","len":"402959","media":{"aes_key":"k","full_url":"https://novac2c.cdn.weixin.qq.com/x?y"}}},{"type":1,"text_item":{"text":"我的私事"},"ref_msg":{"svr_id":"123","title":"原话"}}]}`)
	out, err := Sanitize(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"o9cq809", "secret", "附件3-5", "e427", "402959", "novac2c", "我的私事", "原话", "7511267372956042376"} {
		if bytes.Contains(out, []byte(leak)) {
			t.Errorf("leaked %q in %s", leak, out)
		}
	}
	if !bytes.Contains(out, []byte(`"示例.pdf"`)) {
		t.Errorf("file extension lost: %s", out)
	}
	twice, _ := Sanitize(out)
	if !bytes.Equal(out, twice) {
		t.Error("Sanitize not idempotent")
	}
}
