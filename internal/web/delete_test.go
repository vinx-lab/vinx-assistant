package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vinx-lab/vinx-assistant/internal/model"
)

// deletable 建一个带附件文件的条目，返回 id 和文件绝对路径。
func (e *env) deletable(t *testing.T, msg string) (int64, string) {
	t.Helper()
	id := e.item(t, &model.Item{MsgID: msg, RawText: "待删除的记录" + msg, Category: model.CatTodo})
	rel := "2026/10/" + msg + "-0.jpg"
	abs := filepath.Join(e.media, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(abs), 0o700)
	if err := os.WriteFile(abs, []byte("\xff\xd8\xff"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.InsertAttachment(context.Background(), &model.Attachment{ItemID: id, Kind: "image", RelPath: rel, State: "ok"}); err != nil {
		t.Fatal(err)
	}
	return id, abs
}

func TestDeleteItemWeb(t *testing.T) {
	e := newEnv(t)
	id, file := e.deletable(t, "d1")
	keep, keepFile := e.deletable(t, "d2")
	sid := strconv.FormatInt(id, 10)
	path := "/items/" + sid

	_, body := e.get(t, path)
	mustContain(t, body, `action="/items/`+sid+`/delete"`, `data-confirm="删除后无法恢复，确定删除？"`, "删除这条记录")
	_, body = e.get(t, "/")
	mustContain(t, body, `/items/`+sid+`/delete`, `data-confirm="删除后无法恢复，确定删除？"`)

	resp, _ := e.post(t, path+"/delete", url.Values{"back": {"/?cat=todo"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?cat=todo&msg=deleted" {
		t.Fatalf("删除：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, _ := e.get(t, path); code != http.StatusNotFound {
		t.Errorf("条目应已消失：%d", code)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("媒体文件应已删除：%v", err)
	}
	if _, err := os.Stat(keepFile); err != nil {
		t.Errorf("其他条目的文件被删：%v", err)
	}
	if code, _ := e.get(t, "/items/"+strconv.FormatInt(keep, 10)); code != http.StatusOK {
		t.Errorf("其他条目应还在：%d", code)
	}
	_, body = e.get(t, "/?msg=deleted")
	mustContain(t, body, "已删除")

	// 不存在、重复删除、非数字
	for _, p := range []string{path + "/delete", "/items/99999/delete", "/items/abc/delete"} {
		if resp, _ := e.post(t, p, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	// 外站 back 被忽略；以详情页为 back 时回看板
	id3, _ := e.deletable(t, "d3")
	resp, _ = e.post(t, "/items/"+strconv.FormatInt(id3, 10)+"/delete", url.Values{"back": {"https://evil.example/"}})
	if resp.Header.Get("Location") != "/?msg=deleted" {
		t.Errorf("外站 back：%s", resp.Header.Get("Location"))
	}
	id4, _ := e.deletable(t, "d4")
	s4 := strconv.FormatInt(id4, 10)
	resp, _ = e.post(t, "/items/"+s4+"/delete", url.Values{"back": {"/items/" + s4}})
	if resp.Header.Get("Location") != "/?msg=deleted" {
		t.Errorf("详情页 back：%s", resp.Header.Get("Location"))
	}
}

func TestDeleteItemFileTroubleOnlyWarns(t *testing.T) {
	e := newEnv(t)
	id, file := e.deletable(t, "w1")
	os.Remove(file) // 文件已不在：不报错
	resp, _ := e.post(t, "/items/"+strconv.FormatInt(id, 10)+"/delete", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", resp.StatusCode)
	}
	if strings.Contains(e.logs.String(), "level=WARN") {
		t.Errorf("文件本来就不在，不该有 WARN：%s", e.logs.String())
	}
	// 路径是非空目录（删不掉）：条目照样删除，只记 WARN
	id2 := e.item(t, &model.Item{MsgID: "w2", RawText: "x"})
	os.MkdirAll(filepath.Join(e.media, "dir", "sub"), 0o700)
	e.st.InsertAttachment(context.Background(), &model.Attachment{ItemID: id2, Kind: "file", RelPath: "dir", State: "ok"})
	resp, _ = e.post(t, "/items/"+strconv.FormatInt(id2, 10)+"/delete", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", resp.StatusCode)
	}
	if !strings.Contains(e.logs.String(), "level=WARN") {
		t.Errorf("应记 WARN：%s", e.logs.String())
	}
}

func TestDeleteItemCrossSiteRejected(t *testing.T) {
	e := newEnv(t)
	id, file := e.deletable(t, "x1")
	sid := strconv.FormatInt(id, 10)
	resp, _ := e.postFrom(t, "/items/"+sid+"/delete", nil, "cross-site")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site: %d", resp.StatusCode)
	}
	if _, err := os.Stat(file); err != nil {
		t.Error("文件不应被删")
	}
	if code, _ := e.get(t, "/items/"+sid); code != http.StatusOK {
		t.Error("条目不应被删")
	}
}

func TestDeleteItemBasePath(t *testing.T) {
	e := newEnvBase(t, "/todo")
	id, file := e.deletable(t, "b1")
	sid := strconv.FormatInt(id, 10)
	_, body := e.get(t, "/todo/items/"+sid)
	mustContain(t, body, `action="/todo/items/`+sid+`/delete"`)
	resp, _ := e.post(t, "/todo/items/"+sid+"/delete", url.Values{"back": {"/?cat=todo"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/todo/?cat=todo&msg=deleted" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("文件应已删除")
	}
	if resp, _ := e.post(t, "/todo/items/"+sid+"/delete", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("重复删除：%d", resp.StatusCode)
	}
}

func TestDeleteItemWithLogin(t *testing.T) {
	e := newEnv(t)
	id, file := e.deletable(t, "l1")
	path := "/items/" + strconv.FormatInt(id, 10) + "/delete"
	e.setPassword(t, testPW)

	resp, _ := e.do(t, "POST", path, url.Values{}, nil)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/signin") {
		t.Fatalf("未登录：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("未登录时文件被删")
	}
	if _, err := e.st.GetItem(context.Background(), id); err != nil {
		t.Fatal("未登录时条目被删")
	}
	c := e.signin(t, "", testPW)
	resp, _ = e.do(t, "POST", path, url.Values{}, c)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?msg=deleted" {
		t.Fatalf("已登录：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("文件应已删除")
	}
}
