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

// 开关热改：打开后立即落盘，关闭后不再写，但历史文件仍可查。
func TestHotToggle(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, 7)
	if r.Enabled() {
		t.Fatal("新建的归档器默认关闭")
	}
	now := time.Now().In(cst)
	r.Append(Record{ID: "dropped", Time: now, Key: "alice"}) // 关闭态：丢弃

	r.SetEnabled(true)
	if !r.Enabled() {
		t.Fatal("SetEnabled(true) 后应为开启")
	}
	r.Append(Record{ID: "r1", Time: now, Key: "alice"})
	r.SetEnabled(false)
	if r.Enabled() {
		t.Fatal("SetEnabled(false) 后应为关闭")
	}
	r.Append(Record{ID: "r2", Time: now, Key: "alice"}) // 关闭态：丢弃

	date := now.Format("2006-01-02")
	recs, err := r.Query("alice", date, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].ID != "r1" {
		t.Fatalf("只应留下开启期间的 r1，实际 %+v", recs)
	}
	// 关掉写入不影响回看历史。
	if days := r.Days("alice"); len(days) != 1 || days[0] != date {
		t.Fatalf("days=%v", days)
	}
	// 重复开关不出错。
	r.SetEnabled(true)
	r.SetEnabled(true)
	r.Stop()
}

func jsonRaw(s string) []byte { return []byte(s) }

// 删除归档：单条 / 按密钥 / 整天 / 清空，且删除后 Days 与 Query 立即反映。
func TestDeleteAndPurge(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, 7)
	r.SetEnabled(true)
	now := time.Now().In(cst)
	date := now.Format("2006-01-02")

	r.Append(Record{ID: "a1", Time: now, Key: "alice", Status: 200})
	r.Append(Record{ID: "a2", Time: now, Key: "alice", Status: 200})
	r.Append(Record{ID: "b1", Time: now, Key: "bob", Status: 200})
	r.Stop() // 排空队列

	// 单条
	n, err := r.Delete(date, "", "a1")
	if err != nil || n != 1 {
		t.Fatalf("删单条 n=%d err=%v", n, err)
	}
	recs, _ := r.Query("alice", date, 10)
	if len(recs) != 1 || recs[0].ID != "a2" {
		t.Fatalf("删单条后 alice=%+v", recs)
	}

	// 按密钥
	n, err = r.Delete(date, "alice", "")
	if err != nil || n != 1 {
		t.Fatalf("按密钥删 n=%d err=%v", n, err)
	}
	if recs, _ := r.Query("alice", date, 10); len(recs) != 0 {
		t.Fatalf("alice 应已清空: %+v", recs)
	}
	if days := r.Days("alice"); len(days) != 0 {
		t.Fatalf("alice 不应再有归档日期: %v", days)
	}
	if recs, _ := r.Query("bob", date, 10); len(recs) != 1 {
		t.Fatalf("bob 不应被牵连: %+v", recs)
	}

	// 整天
	n, err = r.Delete(date, "", "")
	if err != nil || n != 1 {
		t.Fatalf("整天删 n=%d err=%v", n, err)
	}
	if days := r.Days(""); len(days) != 0 {
		t.Fatalf("整天删后仍有日期: %v", days)
	}

	// 清空：先补两天再全删
	r.SetEnabled(true)
	r.Append(Record{ID: "x", Time: now, Key: "carol", Status: 200})
	r.Stop()
	if files, err := r.Purge(); err != nil || files == 0 {
		t.Fatalf("purge files=%d err=%v", files, err)
	}
	if days := r.Days(""); len(days) != 0 {
		t.Fatalf("purge 后仍有归档: %v", days)
	}
	// 不存在的日期：不报错，返回 0
	if n, err := r.Delete("2000-01-01", "", ""); err != nil || n != 0 {
		t.Fatalf("空日期删除 n=%d err=%v", n, err)
	}
	// 非法日期要拒绝
	if _, err := r.Delete("2026/10/26", "", ""); err == nil {
		t.Fatal("非法日期应报错")
	}
}

func TestExtractUsage(t *testing.T) {
	// 非流式：顶层 usage。
	p, c := ExtractUsage(jsonRaw(`{"usage":{"prompt_tokens":35,"completion_tokens":43}}`), false)
	if p != 35 || c != 43 {
		t.Fatalf("非流式 usage = %d/%d", p, c)
	}
	// 流式：usage 在最后一帧，前面的帧是 null。
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20}}\n\n" +
		"data: [DONE]\n\n"
	p, c = ExtractUsage([]byte(sse), true)
	if p != 100 || c != 20 {
		t.Fatalf("流式 usage = %d/%d", p, c)
	}
	if p, c := ExtractUsage(jsonRaw(`{"choices":[]}`), false); p != 0 || c != 0 {
		t.Fatalf("无 usage 应返回 0/0，实际 %d/%d", p, c)
	}
}
