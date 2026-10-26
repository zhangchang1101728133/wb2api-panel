// Package verwatch 客户端版本巡检：从版本源取最新 WorkBuddy 客户端 / CLI 版本号，
// 与当前生效值比较，比当前新就交给调用方写回配置并热生效。
//
// 为什么需要它：上游随时抬高客户端版本门槛，版本过老直接 11140 / 请求非法。
// 以前只能改常量重新编译。现在版本号走配置（面板可改），外加这里的自动发现。
//
// 「版本源」就是一段文本：可以是官方下载页、某个 JSON 清单、自家镜像，
// 或者桌面客户端落盘的 product.json（本地文件路径也支持）。用正则从里面
// 抓版本号，不绑定具体格式。
package verwatch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultVersionRe 默认的版本号正则：抓出文本里所有 x.y.z[-build]。
var DefaultVersionRe = `(\d+\.\d+\.\d+(?:-\d+)?)`

// Source 一个域（cn / global）的版本源。
type Source struct {
	// URL http(s) 地址，或本地文件路径（不含 scheme 即当文件）。
	URL string
	// ClientRegex / CLRegex 从文本里抓客户端版本与 CLI 版本的正则（取第一个分组）。
	// 留空且无 ClientRegex 时：取全页最大的 x.y.z 作为客户端版本（Changelog 场景）；
	// CLI 版本没有正则就不猜（官方文档页不列 CLI 版本）。
	ClientRegex string
	CLRegex     string
}

// 官方 Changelog：版本号来源（也是默认源）。
const (
	ChangelogCN     = "https://www.workbuddy.cn/docs/workbuddy/Changelog"
	ChangelogGlobal = "https://www.workbuddy.ai/docs/zh/workbuddy/Changelog"
)

// Resolve 返回实际要拉的地址：显式配了就用配置的，没配就用官方 Changelog。
func (s Source) Resolve(deflt string) string {
	if u := strings.TrimSpace(s.URL); u != "" {
		return u
	}
	return deflt
}

// Configured 是否配了版本源。
func (s Source) Configured() bool { return strings.TrimSpace(s.URL) != "" }

// Result 一次巡检的解析结果。
type Result struct {
	Client string // 客户端版本（可能为空 = 没解析到）
	CLI    string
	Raw    string // 原始文本（截断后，便于面板展示失败原因）
}

// Fetch 读取版本源文本：http(s) 走 GET，其余按本地文件读。
func Fetch(ctx context.Context, rawURL string, timeout time.Duration) (string, error) {
	url := strings.TrimSpace(rawURL)
	if url == "" {
		return "", fmt.Errorf("empty url")
	}
	if strings.HasPrefix(url, "file://") {
		b, err := os.ReadFile(strings.TrimPrefix(url, "file://"))
		return string(b), err
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		b, err := os.ReadFile(url)
		return string(b), err
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	// 版本源多为官网/静态资源，带一个普通浏览器 UA 更不容易被拦。
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/126 Safari/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Parse 从文本里解析版本号。两个正则都可留空。
//
// 留空时（Changelog 页面场景）取「全页最大的版本号」——文档页通常把最新版写在
// 最上面，但导航/目录里也可能先出现旧版本，取最大值比取第一个稳。
func Parse(text string, src Source) Result {
	res := Result{Raw: clipText(text, 300)}
	if src.ClientRegex != "" {
		res.Client = firstMatch(text, src.ClientRegex)
	}
	if src.CLRegex != "" {
		res.CLI = firstMatch(text, src.CLRegex)
	}
	if src.ClientRegex == "" {
		res.Client = MaxVersion(text)
	}
	return res
}

// MaxVersion 返回文本里最大的 x.y.z（找不到返回空串）。
func MaxVersion(text string) string {
	all := allMatches(text, DefaultVersionRe)
	best := ""
	for _, v := range all {
		if best == "" || Newer(v, best) {
			best = v
		}
	}
	return best
}

// Newer a 是否比 b 新（按 x.y.z 数字段比较，忽略 -build 后缀）。
func Newer(a, b string) bool {
	as, bs := splitVer(a), splitVer(b)
	if len(as) == 0 {
		return false
	}
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func splitVer(v string) []int {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func firstMatch(text, re string) string {
	m := allMatches(text, re)
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// allMatches 用 Go 正则取第一个捕获组；正则非法/无分组时退回整段匹配。
func allMatches(text, re string) []string {
	rx, err := regexp.Compile(re)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range rx.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 && m[1] != "" {
			out = append(out, m[1])
		} else if len(m) > 0 {
			out = append(out, m[0])
		}
	}
	return out
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
