package panel

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestIndexHTMLNoDuplicateIDs 页面里的 id 必须唯一。
//
// 为什么单独测：getElementById 只返回第一个匹配，重复 id 会让两个弹层互相顶替。
// v1.14 就踩过——「新建密钥」编辑器和「登录密钥门」都叫 keyVeil，手机上没有本地
// 密钥时每次 401 都会弹出新建密钥表单，且真正的输入框永远拿不到焦点。
func TestIndexHTMLNoDuplicateIDs(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rx := regexp.MustCompile(`\bid="([^"]+)"`)
	count := map[string]int{}
	for _, m := range rx.FindAllStringSubmatch(body, -1) {
		count[m[1]]++
	}
	for id, n := range count {
		if n > 1 {
			t.Errorf("id=%q 出现了 %d 次（getElementById 只认第一个）", id, n)
		}
	}
	if !strings.Contains(body, `id="keyGate"`) {
		t.Error("缺少独立的密钥门 #keyGate")
	}
	if strings.Contains(body, `id="keyGate" data-lock`) && !strings.Contains(body, `id="keyInput"`) {
		t.Error("密钥门里缺少 #keyInput")
	}
}

// TestIndexHTMLNoInlineHandlers 页面不得用内联事件处理器（CSP script-src 'self'
// 会直接拦掉，表现为「点了没反应」）。
func TestIndexHTMLNoInlineHandlers(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	for _, attr := range []string{"onclick", "onsubmit", "onchange", "oninput", "onload", "onerror"} {
		if strings.Contains(body, " "+attr+"=") {
			t.Errorf("index.html 含内联处理器 %s=（会被 CSP 拦掉）", attr)
		}
	}
}
