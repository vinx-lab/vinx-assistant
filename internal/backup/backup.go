// Package backup 在服务运行时导出数据库快照，连同媒体目录打成 tar.gz。
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/vinx-lab/vinx-assistant/internal/store"
)

func Write(ctx context.Context, st *store.Store, mediaDir string, w io.Writer) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(mediaDir), ".backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	snap := filepath.Join(tmpDir, "vinx-assistant.db")
	if err := st.Snapshot(ctx, snap); err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := addFile(tw, snap, "vinx-assistant.db"); err != nil {
		return err
	}
	err = filepath.WalkDir(mediaDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == mediaDir {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() || filepath.Ext(p) == ".part" {
			return nil
		}
		rel, err := filepath.Rel(mediaDir, p)
		if err != nil {
			return err
		}
		return addFile(tw, p, filepath.ToSlash(filepath.Join("media", rel)))
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func addFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	h, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	h.Name = name
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}
