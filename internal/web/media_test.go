package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

func TestMediaServingAndTraversal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// secret.txt 放在媒体目录的上一级。
	os.MkdirAll(filepath.Join(e.media, "2026/10"), 0o700)
	os.WriteFile(filepath.Join(e.media, "2026/10/1-0-合同.pdf"), []byte("%PDF"), 0o600)
	os.WriteFile(filepath.Join(e.media, "2026/10/1-1.jpg"), []byte("\xff\xd8\xff"), 0o600)
	os.WriteFile(filepath.Join(e.media, "2026/10/unlisted.txt"), []byte("x"), 0o600)
	outside := filepath.Join(filepath.Dir(e.media), "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o600)
	os.Symlink(outside, filepath.Join(e.media, "2026/10/link.txt"))
	id, err := e.st.InsertItem(ctx, &model.Item{MsgID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []*model.Attachment{
		{ItemID: id, Kind: "file", RelPath: "2026/10/1-0-合同.pdf", FileName: "合同.pdf", State: "ok"},
		{ItemID: id, Kind: "image", RelPath: "2026/10/1-1.jpg", State: "ok"},
		{ItemID: id, Kind: "file", RelPath: "2026/10/link.txt", FileName: "link.txt", State: "ok"},
	} {
		if _, err := e.st.InsertAttachment(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := e.client().Get(e.srv.URL + "/media/2026/10/1-0-%E5%90%88%E5%90%8C.pdf")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "filename*=utf-8''%E5%90%88%E5%90%8C.pdf") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("file: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = e.client().Get(e.srv.URL + "/media/2026/10/1-1.jpg")
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("image: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = e.client().Get(e.srv.URL + "/media/2026/10/1-1.jpg?download=1")
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download=1: %d %v", resp.StatusCode, resp.Header)
	}

	bad := []string{
		"/media/../secret.txt", "/media/%2e%2e/secret.txt", "/media/2026/10/../../../secret.txt",
		"/media/2026/10/%2e%2e/%2e%2e/%2e%2e/secret.txt", "/media/..%2fsecret.txt",
		"/media/2026/10/link.txt",     // 符号链接指向目录外，即使在附件表里
		"/media//etc/passwd",          // 绝对路径
		"/media/%2fetc/passwd",        // 编码的绝对路径
		"/media/2026/10/unlisted.txt", // 文件在，但不在附件表
		"/media/2026/10/nope.pdf", "/media/2026", "/media/",
	}
	for _, p := range bad {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/x", nil)
		req.URL.Opaque = p // 不让客户端先把 .. 规范化掉
		resp, err := e.client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("%s served", p)
		}
	}
}

// 直接调 handler，绕开 mux 对路径的清理，确认 handler 自己也拒绝穿越。
func TestMediaHandlerRejectsTraversalDirectly(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(e.media, 0o700)
	os.WriteFile(filepath.Join(filepath.Dir(e.media), "secret.txt"), []byte("secret"), 0o600)
	s := New(Deps{Store: e.st, MediaDir: e.media})
	for _, p := range []string{"../secret.txt", "a/../../secret.txt", "/etc/passwd", "", "x\x00y"} {
		req, _ := http.NewRequest(http.MethodGet, "/media/x", nil)
		req.SetPathValue("path", p)
		rec := &recorder{h: http.Header{}}
		s.media(rec, req)
		if rec.code == 200 {
			t.Errorf("%q served", p)
		}
	}
}

type recorder struct {
	h    http.Header
	code int
}

func (r *recorder) Header() http.Header         { return r.h }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(c int)           { r.code = c }
