package apikey

import (
	"encoding/json"
	"net/http"
	"testing"
)

func req(authz string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func TestBuildResolve(t *testing.T) {
	tbl, err := Build([]Key{
		{ID: "alice", Key: "sk-alice-secret", Enabled: true, Accounts: []string{"u1", "u2"}, MinCredit: 5, MaxRPM: 30},
		{ID: "bob", Key: "sk-bob-secret", Enabled: false},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	e, ok := tbl.Resolve(req("Bearer sk-alice-secret"))
	if !ok {
		t.Fatal("alice 应通过")
	}
	if e.ID != "alice" || e.Admin {
		t.Fatalf("entry = %+v", e)
	}
	if len(e.Allow) != 2 || e.MinCredit != 5 || e.MaxRPM != 30 {
		t.Fatalf("约束未带上: %+v", e)
	}

	// 禁用密钥：与不存在同口径（false）。
	if _, ok := tbl.Resolve(req("Bearer sk-bob-secret")); ok {
		t.Fatal("禁用密钥不应通过")
	}
	if _, ok := tbl.Resolve(req("Bearer nope")); ok {
		t.Fatal("未知密钥不应通过")
	}
	if _, ok := tbl.Resolve(req("")); ok {
		t.Fatal("缺头不应通过")
	}
	if _, ok := tbl.Resolve(req("Basic sk-alice-secret")); ok {
		t.Fatal("非 Bearer 方案不应通过")
	}
}

func TestBuildValidation(t *testing.T) {
	cases := []struct {
		name     string
		keys     []Key
		reserved []string
	}{
		{"空 key", []Key{{ID: "a", Key: "", Enabled: true}}, nil},
		{"非法 id", []Key{{ID: "A B", Key: "k", Enabled: true}}, nil},
		{"id 以符号开头", []Key{{ID: "-a", Key: "k", Enabled: true}}, nil},
		{"重复 id", []Key{{ID: "a", Key: "k1", Enabled: true}, {ID: "a", Key: "k2", Enabled: true}}, nil},
		{"重复 key", []Key{{ID: "a", Key: "k1", Enabled: true}, {ID: "b", Key: "k1", Enabled: true}}, nil},
		{"与管理密钥撞车", []Key{{ID: "a", Key: "admin", Enabled: true}}, []string{"admin"}},
		{"负积分门槛", []Key{{ID: "a", Key: "k", Enabled: true, MinCredit: -1}}, nil},
		{"负 RPM", []Key{{ID: "a", Key: "k", Enabled: true, MaxRPM: -1}}, nil},
	}
	for _, c := range cases {
		if _, err := Build(c.keys, c.reserved...); err == nil {
			t.Errorf("%s: 期望报错，实际通过", c.name)
		}
	}
}

func TestBuildReservedEmptyIgnored(t *testing.T) {
	// api_key 为空（= 不鉴权）时不应把空串当成已占用密钥。
	if _, err := Build([]Key{{ID: "a", Key: "k", Enabled: true}}, ""); err != nil {
		t.Fatalf("空 reserved 不应报错: %v", err)
	}
}

func TestMask(t *testing.T) {
	if got := Mask("sk-1234567890abcdef"); got != "sk-123…cdef" {
		t.Errorf("Mask = %q", got)
	}
	if got := Mask("short"); got != "*****" {
		t.Errorf("短密钥应整体打码，得到 %q", got)
	}
	if got := Mask(""); got != "" {
		t.Errorf("空密钥得到 %q", got)
	}
}

func TestByIDAndEntries(t *testing.T) {
	tbl, err := Build([]Key{{ID: "a", Key: "k1", Enabled: true, Name: ""}})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := tbl.ByID("a")
	if !ok || e.Name != "a" {
		t.Fatalf("Name 应回落到 id: %+v", e)
	}
	if _, ok := tbl.ByID("missing"); ok {
		t.Fatal("不存在的 id 应返回 false")
	}
	if n := len(tbl.Entries()); n != 1 {
		t.Fatalf("Entries = %d", n)
	}
	if tbl.Len() != 1 {
		t.Fatalf("Len = %d", tbl.Len())
	}
	var nilTable *Table
	if nilTable.Len() != 0 || len(nilTable.Entries()) != 0 {
		t.Fatal("nil 表应安全")
	}
	if _, ok := nilTable.Resolve(req("Bearer x")); ok {
		t.Fatal("nil 表不应通过")
	}
}
func TestEnabledDefaultsTrue(t *testing.T) {
	var k Key
	if err := json.Unmarshal([]byte(`{"id":"a","key":"k"}`), &k); err != nil {
		t.Fatal(err)
	}
	if !k.Enabled {
		t.Fatal("缺省 enabled 应为 true，否则手写配置会静默失效")
	}
	if err := json.Unmarshal([]byte(`{"id":"a","key":"k","enabled":false}`), &k); err != nil {
		t.Fatal(err)
	}
	if k.Enabled {
		t.Fatal("显式 false 应保持 false")
	}
}
