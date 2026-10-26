package contentlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractTextJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"普通字符串", `{"choices":[{"message":{"content":"你好"}}]}`, "你好"},
		{"文本块数组", `{"choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}]}`, "ab"},
		{"缺字段", `{"choices":[{"message":{}}]}`, ""},
		{"非 JSON", `not json`, ""},
	}
	for _, c := range cases {
		if got := ExtractText([]byte(c.raw), false); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestExtractTextStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"你"}}]}`,
		`data: {"choices":[{"delta":{"content":"好"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	if got := ExtractText([]byte(sse), true); got != "你好" {
		t.Fatalf("got %q", got)
	}
}

func TestAppendQueryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, 7)
	r.Start()
	defer r.Stop()

	now := time.Now().In(cst)
	r.Append(Record{
		ID: "r1", Time: now, Key: "alice", Model: "cn:m", Status: 200,
		Request: jsonRaw(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		Response: `{"choices":[{"message":{"content":"hello"}}]}`,
		Text:     "hello",
	})
	r.Append(Record{ID: "r2", Time: now, Key: "bob", Status: 200})
	r.Append(Record{ID: "r3", Time: now, Key: "alice", Status: 500})

	// Stop 排空队列。
	r.Stop()

	date := now.Format("2006-01-02")
	recs, err := r.Query("alice", date, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].ID != "r1" || recs[1].ID != "r3" {
		t.Fatalf("alice 记录 = %+v", recs)
	}
	if recs[0].Text != "hello" {
		t.Fatalf("正文丢失: %+v", recs[0])
	}
	all, err := r.Query("", date, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("全部记录 = %d, err=%v", len(all), err)
	}
	if days := r.Days("alice"); len(days) != 1 || days[0] != date {
		t.Fatalf("days = %v", days)
	}
	if days := r.Days("carol"); len(days) != 0 {
		t.Fatalf("carol 不应有归档: %v", days)
	}
}

func TestTruncate(t *testing.T) {
	rec := Record{Request: jsonRaw(`"` + strings.Repeat("a", maxRecordBytes) + `"`)}
	rec.truncate()
	if !rec.Truncated || len(rec.Request) > maxRecordBytes {
		t.Fatalf("请求体应被截断: len=%d trunc=%v", len(rec.Request), rec.Truncated)
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().In(cst).AddDate(0, 0, -30).Format("2006-01-02")
	p := filepath.Join(dir, old+".jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(dir, 7)
	r.Start()
	defer r.Stop()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("过期文件应被删除: %v", err)
	}
}

func TestDisabledRecorder(t *testing.T) {
	r := New("", 7)
	if r.Enabled() {
		t.Fatal("空目录应禁用")
	}
	r.Start()
	r.Append(Record{})
	r.Stop()
	if _, err := r.Query("", "", 10); err == nil {
		t.Fatal("禁用时应返回错误")
	}
}

func jsonRaw(s string) []byte { return []byte(s) }
