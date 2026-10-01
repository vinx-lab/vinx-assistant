package ingest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// sanitizeName 只保留文件名最后一段，去掉控制字符，限制在 100 个字符内（保留扩展名）。
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "file"
	}
	r := []rune(name)
	if len(r) > 100 {
		ext := []rune(filepath.Ext(name))
		if len(ext) > 10 {
			ext = nil
		}
		r = append(r[:100-len(ext)], ext...)
	}
	return string(r)
}

func guessExt(data []byte, kind string) string {
	switch {
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return ".jpg"
	case bytes.HasPrefix(data, []byte("\x89PNG")):
		return ".png"
	case bytes.HasPrefix(data, []byte("GIF8")):
		return ".gif"
	case bytes.HasPrefix(data, []byte("#!SILK")), bytes.HasPrefix(data, []byte("\x02#!SILK")):
		return ".silk"
	case len(data) >= 8 && string(data[4:8]) == "ftyp":
		return ".mp4"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp"
	}
	switch kind {
	case "image":
		return ".jpg"
	case "voice":
		return ".silk"
	case "video":
		return ".mp4"
	}
	return ".bin"
}

// saveMedia 把附件写到 <dir>/YYYY/MM/，返回相对 dir 的路径。先写临时文件再改名，避免半截文件。
func saveMedia(dir string, now time.Time, itemID int64, idx int, kind, fileName string, data []byte) (string, error) {
	sub := now.Format("2006/01")
	var name string
	if kind == "file" {
		name = fmt.Sprintf("%d-%d-%s", itemID, idx, sanitizeName(fileName))
	} else {
		name = fmt.Sprintf("%d-%d%s", itemID, idx, guessExt(data, kind))
	}
	full := filepath.Join(dir, sub, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", err
	}
	tmp := full + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, full); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join(sub, name)), nil
}
