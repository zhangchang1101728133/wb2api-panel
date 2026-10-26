// content_test.go 对话归档接口：全局开关关着时要能「说清楚」，而不是丢一个 501。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// 列表接口不下发完整请求/响应（90 条 × 十几 MB 会把面板卡死），只给正文预览；
// 完整内容走 content/item 单条取。
func TestContentListIsLightweight(t *testing.T) {
	rec := contentlog.New(t.TempDir(), 7)
	rec.SetEnabled(true)
	now := time.Now()
	rec.Append(contentlog.Record{
		ID: "big", Time: now, Key: "alice", Status: 200,
		Request:  []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 5000) + `"}]}`),
		Response: strings.Repeat("y", 200000),
		Text:     strings.Repeat("答", 3000),
	})
	rec.Stop()
	p := New(Config{Version: "test", APIKey: "test-key", ContentLog: rec})

	w := httptest.NewRecorder()
	p.contentList(w, httptest.NewRequest("GET", "/panel/api/content?limit=10", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	if w.Body.Len() > 4000 {
		t.Fatalf("列表响应 %d 字节，应只含摘要", w.Body.Len())
	}
	var out struct {
		Records []struct {
			ID       string `json:"id"`
			Request  any    `json:"request"`
			Response string `json:"response"`
			Text     string `json:"text"`
		} `json:"records"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Records) != 1 {
		t.Fatalf("records=%d", len(out.Records))
	}
	r := out.Records[0]
	if r.Request != nil || r.Response != "" {
		t.Errorf("列表不应包含原始请求/响应")
	}
	if n := len([]rune(r.Text)); n > 700 {
		t.Errorf("正文预览 %d 字，应截断", n)
	}

	// 单条详情要拿得到完整内容
	w = httptest.NewRecorder()
	p.contentItem(w, httptest.NewRequest("GET", "/panel/api/content/item?id=big&date="+now.Format("2006-01-02"), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("item code=%d body=%s", w.Code, w.Body)
	}
	if w.Body.Len() < 200000 {
		t.Fatalf("详情应含完整响应，实际 %d 字节", w.Body.Len())
	}

	// 不存在的 id → 404
	w = httptest.NewRecorder()
	p.contentItem(w, httptest.NewRequest("GET", "/panel/api/content/item?id=nope&date="+now.Format("2006-01-02"), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", w.Code)
	}
}

// 删除接口：缺日期要 400（按天归档必须指名日期），全量清空要能删掉文件。
func TestContentDelete(t *testing.T) {
	rec := contentlog.New(t.TempDir(), 7)
	rec.SetEnabled(true)
	defer rec.SetEnabled(false)
	now := time.Now()
	rec.Append(contentlog.Record{ID: "r1", Time: now, Key: "alice", Status: 200})
	rec.Append(contentlog.Record{ID: "r2", Time: now, Key: "bob", Status: 200})
	rec.Stop()

	p := New(Config{Version: "test", APIKey: "test-key", ContentLog: rec})
	date := now.Format("2006-01-02")

	// 缺 date：400，且不动数据
	w := httptest.NewRecorder()
	p.contentDelete(w, httptest.NewRequest("POST", "/panel/api/content/delete",
		strings.NewReader(`{"key":"alice"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 date code=%d want 400 body=%s", w.Code, w.Body)
	}

	// 删单条
	w = httptest.NewRecorder()
	p.contentDelete(w, httptest.NewRequest("POST", "/panel/api/content/delete",
		strings.NewReader(`{"date":"`+date+`","id":"r1"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("删单条 code=%d body=%s", w.Code, w.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["deleted"].(float64) != 1 {
		t.Errorf("deleted=%v", out["deleted"])
	}
	if days := rec.Days(""); len(days) != 1 {
		t.Fatalf("bob 的记录应保留，days=%v", days)
	}

	// 全量清空
	w = httptest.NewRecorder()
	p.contentDelete(w, httptest.NewRequest("POST", "/panel/api/content/delete",
		strings.NewReader(`{"all":true}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("清空 code=%d body=%s", w.Code, w.Body)
	}
	if days := rec.Days(""); len(days) != 0 {
		t.Fatalf("清空后仍有归档: %v", days)
	}
}
