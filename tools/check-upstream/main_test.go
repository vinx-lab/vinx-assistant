package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lockText = `# 注释
repo T/w
version 2.4.9
commit c1
file docs/protocol_zh_CN.md s1
file src/api/types.ts s2
`

const changelog = "# 变更日志\n\n## [未发布]\n\n## [2.5.0] - 2026-10-20\n\n### 变更\n\n- 新的引用格式\n\n## [2.4.9] - 2026-09-17\n\n- 旧的\n"

func fakeGitHub(t *testing.T, version, typesSHA string, deleted bool) *httptest.Server {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v any
		switch {
		case r.URL.Path == "/repos/T/w":
			v = map[string]string{"default_branch": "main"}
		case r.URL.Path == "/repos/T/w/commits/main":
			v = map[string]string{"sha": "c2"}
		case strings.HasSuffix(r.URL.Path, "/package.json"):
			v = map[string]string{"content": b64(`{"version":"` + version + `"}`)}
		case strings.HasSuffix(r.URL.Path, "/protocol_zh_CN.md"):
			v = map[string]string{"sha": "s1"}
		case strings.HasSuffix(r.URL.Path, "/types.ts"):
			if deleted {
				http.NotFound(w, r)
				return
			}
			v = map[string]string{"sha": typesSHA}
		case strings.HasSuffix(r.URL.Path, "/CHANGELOG.zh_CN.md"):
			v = map[string]string{"content": b64(changelog)}
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(v)
	}))
}

func writeLock(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "upstream.lock")
	os.WriteFile(p, []byte(lockText), 0o644)
	return p
}

func TestNoChange(t *testing.T) {
	srv := fakeGitHub(t, "2.4.9", "s2", false)
	defer srv.Close()
	t.Setenv("UPSTREAM_API", srv.URL)
	var out, errb bytes.Buffer
	if code := run(writeLock(t), false, &out, &errb); code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), errb.String())
	}
}

func TestChangeReported(t *testing.T) {
	srv := fakeGitHub(t, "2.5.0", "s2-new", false)
	defer srv.Close()
	t.Setenv("UPSTREAM_API", srv.URL)
	var out, errb bytes.Buffer
	if code := run(writeLock(t), false, &out, &errb); code != 1 {
		t.Fatalf("code=%d err=%s", code, errb.String())
	}
	r := out.String()
	for _, want := range []string{"2.4.9 → 2.5.0", "`src/api/types.ts`：有改动", "新的引用格式", "compare/c1...c2"} {
		if !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
	if strings.Contains(r, "旧的") || strings.Contains(r, "protocol_zh_CN.md`：") {
		t.Errorf("report has stale content:\n%s", r)
	}
}

func TestDeletedFileReported(t *testing.T) {
	srv := fakeGitHub(t, "2.4.9", "", true)
	defer srv.Close()
	t.Setenv("UPSTREAM_API", srv.URL)
	var out, errb bytes.Buffer
	if code := run(writeLock(t), false, &out, &errb); code != 1 || !strings.Contains(out.String(), "已删除或改名") {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
}

func TestUpdateRewritesLock(t *testing.T) {
	srv := fakeGitHub(t, "2.5.0", "s2-new", false)
	defer srv.Close()
	t.Setenv("UPSTREAM_API", srv.URL)
	p := writeLock(t)
	var out, errb bytes.Buffer
	if code := run(p, true, &out, &errb); code != 0 {
		t.Fatalf("code=%d err=%s", code, errb.String())
	}
	b, _ := os.ReadFile(p)
	l, err := parseLock(bytes.NewReader(b))
	if err != nil || l.Version != "2.5.0" || l.Commit != "c2" || l.SHAs["src/api/types.ts"] != "s2-new" || l.Files[0] != "docs/protocol_zh_CN.md" {
		t.Fatalf("lock = %+v err=%v\n%s", l, err, b)
	}
	if code := run(p, false, &out, &errb); code != 0 {
		t.Fatal("after update must be clean")
	}
}

func TestParseLockErrors(t *testing.T) {
	if _, err := parseLock(strings.NewReader("repo x\nbogus line\n")); err == nil {
		t.Fatal("want error")
	}
	if _, err := parseLock(strings.NewReader("repo x\n")); err == nil {
		t.Fatal("want error for missing fields")
	}
}

func TestUpdateRefusesMissingFile(t *testing.T) {
	srv := fakeGitHub(t, "2.5.0", "", true)
	defer srv.Close()
	t.Setenv("UPSTREAM_API", srv.URL)
	p := writeLock(t)
	var out, errb bytes.Buffer
	if code := run(p, true, &out, &errb); code != 2 || !strings.Contains(errb.String(), "需要人工决定") {
		t.Fatalf("code=%d err=%s", code, errb.String())
	}
	if b, _ := os.ReadFile(p); string(b) != lockText {
		t.Fatalf("lock was modified:\n%s", b)
	}
}

func TestBadPackageJSONIsError(t *testing.T) {
	for _, ver := range []string{""} {
		srv := fakeGitHub(t, ver, "s2", false)
		t.Setenv("UPSTREAM_API", srv.URL)
		p := writeLock(t)
		var out, errb bytes.Buffer
		if code := run(p, true, &out, &errb); code != 2 {
			t.Errorf("version %q: code=%d", ver, code)
		}
		if b, _ := os.ReadFile(p); string(b) != lockText {
			t.Errorf("lock was modified:\n%s", b)
		}
		srv.Close()
	}
}

func TestParseLockRequiresCommit(t *testing.T) {
	if _, err := parseLock(strings.NewReader("repo x\nversion 1\nfile a b\n")); err == nil {
		t.Fatal("want error for missing commit")
	}
}
