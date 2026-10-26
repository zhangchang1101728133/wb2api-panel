// content_test.go 对话归档接口：全局开关关着时要能「说清楚」，而不是丢一个 501。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/contentlog"
)

func TestContentEndpointsWhenArchiveDisabled(t *testing.T) {
	rec := contentlog.New(t.TempDir(), 7) // 未 SetEnabled = 全局关闭
	p := New(Config{Version: "test", APIKey: "test-key", ContentLog: rec})

	// days：200 + enabled=false，前端据此提示「去哪儿开」，而不是弹一个 501 错误。
	w := httptest.NewRecorder()
	p.contentDays(w, httptest.NewRequest("GET", "/panel/api/content/days", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("days code=%d want 200 body=%s", w.Code, w.Body)
	}
	var days map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &days); err != nil {
		t.Fatal(err)
	}
	if days["enabled"] != false {
		t.Errorf("enabled=%v want false", days["enabled"])
	}
	if list, ok := days["days"].([]any); !ok || len(list) != 0 {
		t.Errorf("days=%v want []", days["days"])
	}
	if days["keep_days"].(float64) != 7 {
		t.Errorf("keep_days=%v want 7", days["keep_days"])
	}

	// list：同样 200，records 为空数组（前端可以直接渲染，不用判空类型）。
	w = httptest.NewRecorder()
	p.contentList(w, httptest.NewRequest("GET", "/panel/api/content", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list code=%d want 200 body=%s", w.Code, w.Body)
	}
	var list map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list["enabled"] != false {
		t.Errorf("enabled=%v want false", list["enabled"])
	}
	if recs, ok := list["records"].([]any); !ok || len(recs) != 0 {
		t.Errorf("records=%v want []", list["records"])
	}
}

// 开关打开后接口立刻反映 enabled=true（热生效，不需要重启）。
func TestContentEndpointsFollowHotToggle(t *testing.T) {
	rec := contentlog.New(t.TempDir(), 7)
	p := New(Config{Version: "test", APIKey: "test-key", ContentLog: rec})

	rec.SetEnabled(true)
	defer rec.SetEnabled(false)

	w := httptest.NewRecorder()
	p.contentDays(w, httptest.NewRequest("GET", "/panel/api/content/days", nil))
	var days map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &days); err != nil {
		t.Fatal(err)
	}
	if days["enabled"] != true {
		t.Errorf("enabled=%v want true", days["enabled"])
	}
}
