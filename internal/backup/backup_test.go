package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/model"
	"github.com/vinx-lab/vinx-assistant/internal/store"
)

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "vinx-assistant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	st.InsertItem(ctx, &model.Item{MsgID: "b", RawText: "备份"})
	media := filepath.Join(dir, "media")
	os.MkdirAll(filepath.Join(media, "2026/10"), 0o700)
	os.WriteFile(filepath.Join(media, "2026/10/1-0.jpg"), []byte("img"), 0o600)

	var buf bytes.Buffer
	if err := Write(ctx, st, media, &buf); err != nil {
		t.Fatal(err)
	}
	gz, _ := gzip.NewReader(&buf)
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = b
	}
	if string(files["media/2026/10/1-0.jpg"]) != "img" {
		t.Fatalf("media missing: %v", keys(files))
	}
	dbBytes, ok := files["vinx-assistant.db"]
	if !ok {
		t.Fatalf("db missing: %v", keys(files))
	}
	out := filepath.Join(t.TempDir(), "restored.db")
	os.WriteFile(out, dbBytes, 0o600)
	db, _ := sql.Open("sqlite", out)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM items`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("restored items = %d err=%v", n, err)
	}
}

func keys(m map[string][]byte) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	return k
}
