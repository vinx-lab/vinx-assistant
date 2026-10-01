package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSecret(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"short":             "***",
		"sk-1234567890abcd": "***abcd",
	}
	for in, want := range cases {
		if got := Secret(in); got != want {
			t.Errorf("Secret(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJSONMasksNestedSecrets(t *testing.T) {
	raw := []byte(`{"message_id":7511267372956042376,"context_token":"abc","item_list":[{"image_item":{"aeskey":"00ff","media":{"aes_key":"Zm9v","full_url":"https://x"}}}],"empty":{"context_token":""}}`)
	out := string(JSON(raw))
	for _, leak := range []string{`"abc"`, `"00ff"`, `"Zm9v"`} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %s in %s", leak, out)
		}
	}
	if !strings.Contains(out, "7511267372956042376") {
		t.Fatalf("big integer mangled: %s", out)
	}
	if !strings.Contains(out, `"full_url":"https://x"`) {
		t.Fatalf("non-secret field lost: %s", out)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
}

func TestJSONInvalidInputDoesNotEcho(t *testing.T) {
	out := string(JSON([]byte(`{"context_token":"abc"`)))
	if strings.Contains(out, "abc") {
		t.Fatalf("invalid JSON echoed: %s", out)
	}
}
