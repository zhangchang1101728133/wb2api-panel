package keyusage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAddAndStat(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, cst)

	r.Add(now, "alice", "deepseek-v4-flash", Delta{PromptTokens: 100, CompletionTokens: 50, Credit: 0.01, HasCredit: true}, true)
	r.Add(now, "alice", "deepseek-v4-flash", Delta{PromptTokens: 20, CompletionTokens: 10}, false)
	r.Add(now, "bob", "glm-5.2", Delta{PromptTokens: 5, CompletionTokens: 5}, true)

	st := r.Stat("alice", now)
	if st.Today.Requests != 2 || st.Today.OK != 1 || st.Today.Failed != 1 {
		t.Fatalf("today = %+v", st.Today)
	}
	if st.Today.Tokens() != 180 {
		t.Fatalf("tokens = %d", st.Today.Tokens())
	}
	if st.Today.Credit != 0.01 {
		t.Fatalf("credit = %v", st.Today.Credit)
	}
	if md := st.Today.Models["deepseek-v4-flash"]; md == nil || md.Requests != 2 {
		t.Fatalf("models = %+v", st.Today.Models)
	}
	if st.Total.Requests != 2 {
		t.Fatalf("total = %+v", st.Total)
	}

	snap := r.Snapshot(now)
	if len(snap) != 2 || snap[0].ID != "alice" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if got := r.TodayTokens("alice", now); got != 180 {
		t.Fatalf("TodayTokens = %d", got)
	}
}

func TestDayBoundary(t *testing.T) {
	r := New("")
	before := time.Date(2026, 10, 26, 23, 59, 0, 0, cst)
	after := before.Add(2 * time.Minute) // 跨到 10-27 00:01

	r.Add(before, "alice", "", Delta{PromptTokens: 10}, true)
	if got := r.TodayTokens("alice", before); got != 10 {
		t.Fatalf("before = %d", got)
	}
	if got := r.TodayTokens("alice", after); got != 0 {
		t.Fatalf("after 应为新的一天，得到 %d", got)
	}
	r.Add(after, "alice", "", Delta{PromptTokens: 7}, true)
	if got := r.TodayTokens("alice", after); got != 7 {
		t.Fatalf("after add = %d", got)
	}
	if st := r.Stat("alice", after); st.Total.Tokens() != 17 {
		t.Fatalf("total 应累计跨天: %+v", st.Total)
	}
}

func TestAllowRPM(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, cst)
	for i := 0; i < 3; i++ {
		if ok, _ := r.AllowRPM("alice", 3, now.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	ok, wait := r.AllowRPM("alice", 3, now.Add(3*time.Second))
	if ok {
		t.Fatal("第 4 次应被限流")
	}
	if wait <= 0 || wait > time.Minute {
		t.Fatalf("重试间隔异常: %v", wait)
	}
	// 窗口滑过后恢复。
	if ok, _ := r.AllowRPM("alice", 3, now.Add(61*time.Second)); !ok {
		t.Fatal("窗口滑过后应放行")
	}
	// 不同密钥互不影响。
	if ok, _ := r.AllowRPM("bob", 3, now); !ok {
		t.Fatal("bob 不应受 alice 影响")
	}
	// limit<=0 不限。
	for i := 0; i < 100; i++ {
		if ok, _ := r.AllowRPM("carol", 0, now); !ok {
			t.Fatal("limit=0 应恒放行")
		}
	}
}

func TestPersistAndReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyusage.json")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, cst)

	r := New(path)
	r.Start()
	r.Add(now, "alice", "m", Delta{PromptTokens: 42}, true)
	r.Stop()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应已落盘: %v", err)
	}

	r2 := New(path)
	r2.Start()
	defer r2.Stop()
	if got := r2.TodayTokens("alice", now); got != 42 {
		t.Fatalf("重启后应恢复，得到 %d", got)
	}
	r2.Reset("alice")
	if got := r2.TodayTokens("alice", now); got != 0 {
		t.Fatalf("Reset 后应为 0，得到 %d", got)
	}
}

func TestPruneOldDays(t *testing.T) {
	r := New("")
	old := time.Date(2026, 1, 1, 12, 0, 0, 0, cst)
	now := old.AddDate(0, 0, retentionDays+5)
	r.Add(old, "alice", "", Delta{PromptTokens: 10}, true)
	r.flush(true)
	if st := r.Stat("alice", now); st.Total.Tokens() != 0 {
		t.Fatalf("过期分桶应被清理，得到 %+v", st.Total)
	}
}
