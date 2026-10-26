// quota_test.go /v1/quota 与 /v1/usage：任何密钥都能查「按自己权限算出的剩余积分」。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikey"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func getJSON(t *testing.T, h *Handler, path, apiKey string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 响应不是 JSON: %v", path, err)
		}
	}
	return rec.Code, out
}

// 用户密钥：remaining 只算它允许的账号，且 min_credit 以下的号不计入。
func TestQuotaScopedByKeyPermission(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 50, 50)   // 低于门槛
	p.SetCredits("u2", 900, 1000)
	p.SetCredits("u3", 5000, 5000) // 不在允许集合内
	tbl := testKeyTable(t, apikey.Key{ID: "alice", Key: "sk-alice", Enabled: true,
		Accounts: []string{"u1", "u2"}, MinCredit: 100})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	}), APIKey: "sk-admin", Keys: tbl})

	for _, path := range []string{"/v1/quota", "/v1/usage"} {
		code, body := getJSON(t, h, path, "sk-alice")
		if code != http.StatusOK {
			t.Fatalf("%s code=%d body=%v", path, code, body)
		}
		if body["isValid"] != true {
			t.Errorf("%s isValid=%v", path, body["isValid"])
		}
		if body["unit"] != "积分" {
			t.Errorf("%s unit=%v", path, body["unit"])
		}
		// u1 积分 50 < min_credit 100，选不中；u3 不在 allow 内。
		if got := body["remaining"].(float64); got != 900 {
			t.Errorf("%s remaining=%v want 900（只算可用账号 u2）", path, got)
		}
		q := body["quota"].(map[string]any)
		if q["accounts"].(float64) != 1 || q["accounts_available"].(float64) != 1 {
			t.Errorf("%s quota=%v", path, q)
		}
		if q["limit"].(float64) != 1000 { // Sub2API 模板读 quota.limit / quota.used
			t.Errorf("%s quota.limit=%v want 1000", path, q["limit"])
		}
		key := body["key"].(map[string]any)
		if key["role"] != "user" || key["accounts_scope"] != "limited" {
			t.Errorf("%s key=%v", path, key)
		}
		if _, leaked := body["accounts"]; leaked {
			t.Errorf("%s 不得返回账号明细", path)
		}
	}
}

// 管理密钥：推理接口仍然拒绝，但额度查询放行（否则没法用同一个提取器）。
func TestQuotaAllowsAdminKey(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 700, 800)
	tbl := testKeyTable(t, apikey.Key{ID: "alice", Key: "sk-alice", Enabled: true})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	}), APIKey: "sk-admin", Keys: tbl})

	code, body := getJSON(t, h, "/v1/quota", "sk-admin")
	if code != http.StatusOK {
		t.Fatalf("admin quota code=%d", code)
	}
	if body["remaining"].(float64) != 700 {
		t.Errorf("remaining=%v want 700（全池）", body["remaining"])
	}
	if body["key"].(map[string]any)["role"] != "admin" {
		t.Errorf("role=%v", body["key"])
	}

	// 推理入口的管理密钥语义不变。
	if code, _ := getJSON(t, h, "/v1/models", "sk-admin"); code != http.StatusUnauthorized {
		t.Errorf("/v1/models admin code=%d want 401", code)
	}

	// 无凭证 / 错误密钥：401。
	if code, _ := getJSON(t, h, "/v1/quota", ""); code != http.StatusUnauthorized {
		t.Errorf("no key code=%d want 401", code)
	}
	if code, _ := getJSON(t, h, "/v1/quota", "sk-nope"); code != http.StatusUnauthorized {
		t.Errorf("bad key code=%d want 401", code)
	}
}
