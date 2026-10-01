// check-upstream 对照上游 openclaw-weixin，检查 ilink 包跟随的协议相关文件有没有变化。只读。
//
//	make check-upstream                    有变化时输出 Markdown 报告，退出码 1；没变化退出码 0；出错退出码 2
//	make check-upstream ARGS=-update       同步完成后，把 upstream.lock 更新为上游当前状态
//
// 不要用 go run 运行：它会把子进程的退出码 2 折成 1。
//
// 环境变量：GITHUB_TOKEN（可选，提高 API 限额）、UPSTREAM_API（测试用，默认 https://api.github.com）。
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

type Lock struct {
	Repo    string
	Version string
	Commit  string
	Files   []string          // 保持顺序
	SHAs    map[string]string // 路径 → blob sha
}

func parseLock(r io.Reader) (*Lock, error) {
	l := &Lock{SHAs: map[string]string{}}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch {
		case f[0] == "repo" && len(f) == 2:
			l.Repo = f[1]
		case f[0] == "version" && len(f) == 2:
			l.Version = f[1]
		case f[0] == "commit" && len(f) == 2:
			l.Commit = f[1]
		case f[0] == "file" && len(f) == 3:
			l.Files = append(l.Files, f[1])
			l.SHAs[f[1]] = f[2]
		default:
			return nil, fmt.Errorf("lock 文件格式不对：%q", sc.Text())
		}
	}
	if l.Repo == "" || l.Version == "" || l.Commit == "" || len(l.Files) == 0 {
		return nil, fmt.Errorf("lock 文件缺少 repo、version、commit 或 file")
	}
	return l, sc.Err()
}

func (l *Lock) Format() string {
	var b strings.Builder
	b.WriteString("# 本项目 ilink 包对齐的上游版本。只在完成一次同步（改完代码、样例、测试）后更新：\n#   make check-upstream ARGS=-update\n")
	fmt.Fprintf(&b, "repo %s\nversion %s\ncommit %s\n", l.Repo, l.Version, l.Commit)
	for _, f := range l.Files {
		fmt.Fprintf(&b, "file %s %s\n", f, l.SHAs[f])
	}
	return b.String()
}

type client struct {
	base, token string
	http        *http.Client
}

func (c *client) get(path string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, c.base+path, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s：HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type Upstream struct {
	Version   string
	Commit    string
	SHAs      map[string]string // 路径 → blob sha（文件被删时为空）
	Changelog string
}

func fetch(c *client, l *Lock) (*Upstream, error) {
	u := &Upstream{SHAs: map[string]string{}}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.get("/repos/"+l.Repo, &repo); err != nil {
		return nil, err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := c.get("/repos/"+l.Repo+"/commits/"+repo.DefaultBranch, &commit); err != nil {
		return nil, err
	}
	u.Commit = commit.SHA
	var pkg struct {
		Content string `json:"content"`
	}
	if err := c.get("/repos/"+l.Repo+"/contents/package.json?ref="+u.Commit, &pkg); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(pkg.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("package.json 内容解码失败：%w", err)
	}
	var meta struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("package.json 解析失败：%w", err)
	}
	if meta.Version == "" {
		return nil, fmt.Errorf("package.json 没有 version")
	}
	u.Version = meta.Version
	for _, f := range l.Files {
		var file struct {
			SHA string `json:"sha"`
		}
		if err := c.get("/repos/"+l.Repo+"/contents/"+f+"?ref="+u.Commit, &file); err != nil {
			if strings.Contains(err.Error(), "HTTP 404") {
				continue // 文件被删或改名：SHA 留空，报告里会列出
			}
			return nil, err
		}
		u.SHAs[f] = file.SHA
	}
	var cl struct {
		Content string `json:"content"`
	}
	if err := c.get("/repos/"+l.Repo+"/contents/CHANGELOG.zh_CN.md?ref="+u.Commit, &cl); err == nil {
		b, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(cl.Content, "\n", ""))
		u.Changelog = string(b)
	}
	return u, nil
}

var sectionRe = regexp.MustCompile(`(?m)^## \[([^\]]+)\]`)

// newSections 返回变更日志里比 lock 版本新的段落（含「未发布」里有内容的部分）。
func newSections(changelog, version string) string {
	idx := sectionRe.FindAllStringSubmatchIndex(changelog, -1)
	var out []string
	for i, m := range idx {
		name := changelog[m[2]:m[3]]
		if name == version {
			break
		}
		end := len(changelog)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		sec := strings.TrimSpace(changelog[m[0]:end])
		if strings.Contains(sec, "\n") { // 跳过空的「未发布」
			out = append(out, sec)
		}
	}
	return strings.Join(out, "\n\n")
}

// report 返回 Markdown 报告；没有变化时返回空串。
func report(l *Lock, u *Upstream) string {
	var changed []string
	for _, f := range l.Files {
		switch {
		case u.SHAs[f] == "":
			changed = append(changed, fmt.Sprintf("- `%s`：上游已删除或改名", f))
		case u.SHAs[f] != l.SHAs[f]:
			changed = append(changed, fmt.Sprintf("- `%s`：有改动", f))
		}
	}
	if len(changed) == 0 && u.Version == l.Version {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## 上游 %s 有变化\n\n", l.Repo)
	fmt.Fprintf(&b, "- 版本：%s → %s\n", l.Version, u.Version)
	fmt.Fprintf(&b, "- 对比：https://github.com/%s/compare/%s...%s\n\n", l.Repo, l.Commit, u.Commit)
	if len(changed) > 0 {
		b.WriteString("### 协议相关文件\n\n" + strings.Join(changed, "\n") + "\n\n")
	} else {
		b.WriteString("协议相关文件没有改动（可能只是 OpenClaw 宿主适配），确认后直接 `-update`。\n\n")
	}
	if s := newSections(u.Changelog, l.Version); s != "" {
		b.WriteString("### 上游变更日志\n\n" + s + "\n\n")
	}
	b.WriteString("### 同步步骤\n\n见 `internal/ilink/UPSTREAM.md`「同步流程」。\n")
	return b.String()
}

func main() {
	lockPath := flag.String("lock", "internal/ilink/upstream.lock", "lock 文件")
	update := flag.Bool("update", false, "把 lock 更新为上游当前状态（同步完成后用）")
	flag.Parse()
	if code := run(*lockPath, *update, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func run(lockPath string, update bool, stdout, stderr io.Writer) int {
	f, err := os.Open(lockPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	l, err := parseLock(f)
	f.Close()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	base := os.Getenv("UPSTREAM_API")
	if base == "" {
		base = "https://api.github.com"
	}
	c := &client{base: strings.TrimRight(base, "/"), token: os.Getenv("GITHUB_TOKEN"), http: &http.Client{Timeout: 20 * time.Second}}
	u, err := fetch(c, l)
	if err != nil {
		fmt.Fprintln(stderr, "check-upstream：", err)
		return 2
	}
	if update {
		for _, p := range l.Files {
			if u.SHAs[p] == "" {
				fmt.Fprintf(stderr, "check-upstream：%s 在上游已删除或改名，需要人工决定：删掉或改名该条目\n", p)
				return 2
			}
		}
		l.Version, l.Commit = u.Version, u.Commit
		for _, p := range l.Files {
			l.SHAs[p] = u.SHAs[p]
		}
		if err := os.WriteFile(lockPath, []byte(l.Format()), 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintf(stdout, "已更新 %s 到 %s（%s）\n", lockPath, u.Version, u.Commit[:min(12, len(u.Commit))])
		return 0
	}
	if r := report(l, u); r != "" {
		fmt.Fprint(stdout, r)
		return 1
	}
	fmt.Fprintf(stdout, "上游没有变化（%s）。\n", l.Version)
	return 0
}
