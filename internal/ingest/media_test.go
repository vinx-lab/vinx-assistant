package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/clock"
)

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"附件3-5 一览表.pdf":    "附件3-5 一览表.pdf",
		"../../etc/passwd": "passwd",
		`a\b\c.txt`:        "c.txt",
		"":                 "file",
		"..":               "file",
		"a\x00b.txt":       "ab.txt",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("长", 200) + ".pdf"
	if got := sanitizeName(long); len([]rune(got)) > 100 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("long name = %q", got)
	}
}

func TestGuessExt(t *testing.T) {
	cases := []struct {
		data []byte
		kind string
		want string
	}{
		{[]byte("\xff\xd8\xff\xe0"), "image", ".jpg"},
		{[]byte("\x89PNG\r\n"), "image", ".png"},
		{[]byte("\x02#!SILK_V3"), "voice", ".silk"},
		{[]byte("\x00\x00\x00\x18ftypmp42"), "video", ".mp4"},
		{[]byte("????"), "image", ".jpg"},
		{[]byte("????"), "voice", ".silk"},
		{[]byte("????"), "file", ".bin"},
	}
	for _, c := range cases {
		if got := guessExt(c.data, c.kind); got != c.want {
			t.Errorf("guessExt(%q,%s) = %s, want %s", c.data, c.kind, got, c.want)
		}
	}
}

func TestSaveMediaStaysInDir(t *testing.T) {
	dir := t.TempDir()
	now := clock.At(2026, 10, 1, 9, 0)
	rel, err := saveMedia(dir, now, 12, 0, "file", "../../evil.pdf", []byte("pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if rel != "2026/10/12-0-evil.pdf" {
		t.Fatalf("rel = %s", rel)
	}
	if b, err := os.ReadFile(filepath.Join(dir, rel)); err != nil || string(b) != "pdf" {
		t.Fatalf("read back: %q %v", b, err)
	}
	rel2, _ := saveMedia(dir, now, 12, 1, "image", "", []byte("\xff\xd8\xff"))
	if rel2 != "2026/10/12-1.jpg" {
		t.Fatalf("rel2 = %s", rel2)
	}
}
