// multikey_test.go 多密钥端到端：鉴权分离、选号约束与回退、限额、模型白名单。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikey"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/contentlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/keyusage"
)

func testKeyTable(t *testing.T, keys ...apikey.Key) *apikey.Table {
	t.Helper()
	tbl, err := apikey.Build(keys, "sk-admin")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return tbl
}

func doChat(t *testing.T, h *Handler, apiKey, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 用户密钥不得读全池状态：/status 会暴露账号 uid、积分与冷却原因。
func TestUserKeyCannotReadStatus(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	tbl := testKeyTable(t, apikey.Key{ID: "alice", Key: "sk-alice", Enabled: true})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	}), APIKey: "sk-admin", Keys: tbl})

	for _, c := range []struct {
		name string
		key  string
		want int
	}{
		{"管理员", "sk-admin", http.StatusOK},
		{"用户密钥", "sk-alice", http.StatusUnauthorized},
		{"错误密钥", "sk-nope", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest("GET", "/status", nil)
		req.Header.Set("Authorization", "Bearer "+c.key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: code=%d want %d", c.name, rec.Code, c.want)
		}
	}
}

// 密钥限定账号：请求只能落到允许的号上。
func TestKeyPicksOnlyAllowedAccount(t *testing.T) {
	var seen []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		seen = append(seen, authz)
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	tbl := testKeyTable(t, apikey.Key{ID: "alice", Key: "sk-alice", Enabled: true, Accounts: []string{"u2"}})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "sk-admin", Keys: tbl})

	if rec := doChat(t, h, "sk-alice", "glm-5.2"); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(seen) != 1 || seen[0] != "Bearer at2" {
		t.Fatalf("上游调用 = %v，期望只用 at2（密钥限定 u2）", seen)
	}
}

// 限定账号全不可用 → 回退全池（可用性优先），而不是直接 503。
func TestKeyFallsBackToFullPool(t *testing.T) {
	var seen []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		seen = append(seen, authz)
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 允许集合里放一个池子里不存在的 uid：必然选不到，只能回退全池。
	tbl := testKeyTable(t, apikey.Key{ID: "alice", Key: "sk-alice", Enabled: true, Accounts: []string{"ghost"}})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "sk-admin", Keys: tbl})

	if rec := doChat(t, h, "sk-alice", "glm-5.2"); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（限定账号不可用时应回退全池）", rec.Code, rec.Body)
	}
	if len(seen) != 1 || seen[0] != "Bearer at1" {
		t.Fatalf("上游调用 = %v，期望回退到 at1", seen)
	}
}

// 积分门槛：低于门槛的号不参与选号（这里唯一账号被门槛挡掉 → 503）。
func TestKeyMinCreditFilters(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 10, 0)
	tbl := testKeyTable(t, apikey.Key{ID: "alice", Key: "sk-alice", Enabled: true, MinCredit: 100})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	}), APIKey: "sk-admin", Keys: tbl})

	if rec := doChat(t, h, "sk-alice", "glm-5.2"); rec.Code == http.StatusOK {
		t.Fatalf("积分 10 < 门槛 100，不该选中该号；body=%s", rec.Body)
	}
}

// RPM 限额：第二次请求 429，并带 Retry-After。
func TestKeyRPMLimit(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	ku := keyusage.New("")
	tbl := testKeyTable(t, apikey.Key{ID: "alice", Key: "sk-alice", Enabled: true, MaxRPM: 1})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	}), APIKey: "sk-admin", Keys: tbl, KeyUsage: ku})

	if rec := doChat(t, h, "sk-alice", "glm-5.2"); rec.Code != http.StatusOK {
		t.Fatalf("第一次应放行: code=%d body=%s", rec.Code, rec.Body)
	}
	rec := doChat(t, h, "sk-alice", "glm-5.2")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第二次应 429: code=%d body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 应带 Retry-After")
	}
	// 管理员不受用户密钥限额影响。
	if rec := doChat(t, h, "sk-admin", "glm-5.2"); rec.Code != http.StatusOK {
		t.Fatalf("管理员不应被限额: code=%d", rec.Code)
	}
}

// 每日 token 额度：入口判「今日已用 ≥ 上限」直接 429。
func TestKeyDailyTokenQuota(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	ku := keyusage.New("")
	ku.Add(time.Now(), "alice", "glm-5.2", keyusage.Delta{PromptTokens: 80, CompletionTokens: 30}, true)
	tbl := testKeyTable(t, apikey.Key{ID: "alice", Key: "sk-alice", Enabled: true, DailyTokens: 100})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	}), APIKey: "sk-admin", Keys: tbl, KeyUsage: ku})

	rec := doChat(t, h, "sk-alice", "glm-5.2")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "quota_exceeded") {
		t.Fatalf("code=%d body=%s（已用 110 ≥ 上限 100，应 429 quota_exceeded）", rec.Code, rec.Body)
	}
}

// 模型白名单：未列出的模型直接 400，不打上游。
func TestKeyModelWhitelist(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	tbl := testKeyTable(t, apikey.Key{ID: "alice", Key: "sk-alice", Enabled: true, Models: []string{"glm-5.2"}})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "sk-admin", Keys: tbl})

	rec := doChat(t, h, "sk-alice", "deepseek-v4-flash")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 0 {
		t.Errorf("白名单外模型不该打上游，calls=%d", calls)
	}
	if rec := doChat(t, h, "sk-alice", "glm-5.2"); rec.Code != http.StatusOK {
		t.Fatalf("白名单内模型应放行: code=%d body=%s", rec.Code, rec.Body)
	}
}

// 正文归档：只归档 content_log=true 的密钥；记录里带请求体与响应正文。
func TestContentArchiveOnlyForContentLogKeys(t *testing.T) {
	dir := t.TempDir()
	cl := contentlog.New(dir, 7)
	cl.Start()
	defer cl.Stop()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	tbl := testKeyTable(t,
		apikey.Key{ID: "alice", Key: "sk-alice", Enabled: true, ContentLog: true},
		apikey.Key{ID: "bob", Key: "sk-bob", Enabled: true},
	)
	h := NewHandler(Config{
		Pool: p,
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
			return 200, sseOK, true
		}),
		APIKey: "sk-admin", Keys: tbl, ContentLog: cl,
	})

	chat := func(key string) {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"请记住暗号：蓝鲸"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code=%d body=%s", key, rec.Code, rec.Body)
		}
	}
	chat("sk-alice")
	chat("sk-bob")
	cl.Stop() // 排空异步队列

	alice, err := cl.Query("alice", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(alice) != 1 {
		t.Fatalf("alice 应归档 1 条，得到 %d", len(alice))
	}
	if !strings.Contains(string(alice[0].Request), "蓝鲸") {
		t.Errorf("请求体应完整保留: %s", alice[0].Request)
	}
	if alice[0].Response == "" || !alice[0].Stream {
		t.Errorf("响应正文/流式标记缺失: %+v", alice[0])
	}
	if bob, _ := cl.Query("bob", "", 10); len(bob) != 0 {
		t.Errorf("未开启 content_log 的密钥不该归档，得到 %d 条", len(bob))
	}
}

// 通配与前缀写法：deepseek-* 放行 cn:deepseek-v4-flash；cn: 前缀只放行该域。
func TestCallerAllowsModelPatterns(t *testing.T) {
	e := apikey.Entry{Models: map[string]struct{}{"deepseek-*": {}}}
	if !callerAllowsModel(e, "cn", "deepseek-v4-flash") {
		t.Error("deepseek-* 应放行 cn:deepseek-v4-flash")
	}
	if callerAllowsModel(e, "cn", "glm-5.2") {
		t.Error("glm-5.2 不在白名单内")
	}
	e2 := apikey.Entry{Models: map[string]struct{}{"global:glm-5.2": {}}}
	if callerAllowsModel(e2, "cn", "glm-5.2") {
		t.Error("global: 前缀不该放行 cn 域")
	}
	if !callerAllowsModel(e2, "global", "glm-5.2") {
		t.Error("global: 前缀应放行 global 域")
	}
	if !callerAllowsModel(apikey.AdminEntry(), "cn", "anything") {
		t.Error("管理员应恒放行")
	}
}
