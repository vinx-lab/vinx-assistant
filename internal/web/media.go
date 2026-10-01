package web

import (
	"errors"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/vinx-lab/vinx-assistant/internal/store"
)

// media 只提供附件表里登记过、且位于 <data>/media 下的文件。
// 路径必须已经是规范形式（没有 ..、前导 /、重复斜杠），先查附件表，
// 再解析符号链接确认真实位置仍在媒体目录内。
func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("path")
	clean := path.Clean("/" + raw)[1:]
	if clean == "" || clean != raw || strings.ContainsAny(raw, "\x00\\") {
		http.NotFound(w, r)
		return
	}
	a, err := s.d.Store.AttachmentByPath(r.Context(), clean)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.d.Log.Error("查附件失败", "path", clean, "err", err)
		}
		http.NotFound(w, r)
		return
	}
	root, err := filepath.EvalSymlinks(s.d.MediaDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil || !strings.HasPrefix(real, root+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(real)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	name := path.Base(clean)
	if a.FileName != "" {
		name = a.FileName
	}
	disp := "inline"
	if a.Kind == "file" || r.URL.Query().Get("download") == "1" {
		disp = "attachment"
	}
	h := w.Header()
	h.Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	h.Set("X-Content-Type-Options", "nosniff")
	if !authFrom(r.Context()).Enabled { // 设了密码时 guard 已设 no-store
		h.Set("Cache-Control", "private, max-age=86400")
	}
	http.ServeContent(w, r, path.Base(clean), fi.ModTime(), f)
}
