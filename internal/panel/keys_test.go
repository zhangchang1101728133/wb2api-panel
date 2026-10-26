// keys_test.go 密钥明文查看：列表只回掩码，明文必须显式取，且不存在的 id 要 404。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikey"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func newKeysTestPanel(t *testing.T, keys []apikey.Key) *Panel {
	t.Helper()
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(p.Close)
	return New(Config{
		Version:  "test",
		APIKey:   "test-key",
		Pool:     p,
		LoadKeys: func() ([]apikey.Key, error) { return keys, nil },
		SaveKeys: func([]apikey.Key) error { return nil },
	})
}

func TestKeysSecretRevealsPlaintext(t *testing.T) {
	const plain = "sk-alice-0123456789abcdef"
	p := newKeysTestPanel(t, []apikey.Key{{ID: "alice", Name: "Alice", Key: plain, Enabled: true}})

	req := httptest.NewRequest("GET", "/panel/api/keys/alice/secret", nil)
	req.SetPathValue("id", "alice")
	w := httptest.NewRecorder()
	p.keysSecret(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["key"] != plain {
		t.Errorf("key=%v want 明文", out["key"])
	}
	if out["masked"] == plain || out["masked"] == "" {
		t.Errorf("masked=%v 应为掩码", out["masked"])
	}
}

func TestKeysSecretNotFound(t *testing.T) {
	p := newKeysTestPanel(t, []apikey.Key{{ID: "alice", Key: "sk-a-12345678", Enabled: true}})
	req := httptest.NewRequest("GET", "/panel/api/keys/bob/secret", nil)
	req.SetPathValue("id", "bob")
	w := httptest.NewRecorder()
	p.keysSecret(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", w.Code)
	}
}

// 列表接口永远不带明文（哪怕调用方是管理员），否则打开页面就等于把所有 key 摊开。
func TestKeysListNeverContainsPlaintext(t *testing.T) {
	const plain = "sk-alice-0123456789abcdef"
	p := newKeysTestPanel(t, []apikey.Key{{ID: "alice", Name: "Alice", Key: plain, Enabled: true}})

	w := httptest.NewRecorder()
	p.keysList(w, httptest.NewRequest("GET", "/panel/api/keys", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var out struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Keys) != 1 {
		t.Fatalf("keys=%d", len(out.Keys))
	}
	for k, v := range out.Keys[0] {
		if s, ok := v.(string); ok && s == plain {
			t.Errorf("字段 %s 泄漏了明文", k)
		}
	}
}
