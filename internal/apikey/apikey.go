// Package apikey 多密钥：调用方密钥的配置形态、运行期校验表与请求侧解析。
//
// 与 internal/httpauth 的分工：
//   - httpauth 只管「单个密钥对不对」（面板管理密钥沿用这条路径，口径不变）；
//   - 本包管「这张表里哪个密钥对」，并带回该密钥的使用约束（可用账号、
//     积分门槛、模型白名单、限额、是否记录正文）。
//
// 安全约定：Resolve 遍历**全部**条目做常量时间比较，命中也不提前 return。
// 一旦按命中短路，攻击者就能用响应耗时把「哪些密钥存在」逐字节探出来。
package apikey

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const bearerPrefix = "Bearer "

// Key 是 config.json 里 api_keys 的一项（可直接序列化）。
type Key struct {
	// ID 稳定标识：统计键、归档目录名都用它，创建后不应修改。
	ID string `json:"id"`
	// Name 展示名（面板列表用），可为空。
	Name string `json:"name,omitempty"`
	// Key 明文密钥。与 api_key 同级敏感，仅存于 config.json（0600）。
	Key string `json:"key"`
	// Enabled 显式开关。关闭后该密钥立即失效（401），但配置与统计保留。
	Enabled bool `json:"enabled"`
	// Accounts 可用账号 uid 集合。空 = 全池自动选。
	Accounts []string `json:"accounts,omitempty"`
	// Pin 固定单号（优先级最高）。该号不可用时按 Accounts → 全池回退。
	Pin string `json:"pin,omitempty"`
	// MinCredit 只选积分 ≥ 该值的账号；0 = 不设门槛。
	MinCredit int64 `json:"min_credit,omitempty"`
	// Models 模型白名单（支持 "cn:" / "global:" 前缀）。空 = 不限。
	Models []string `json:"models,omitempty"`
	// MaxRPM 每分钟请求上限；0 = 不限。
	MaxRPM int `json:"max_rpm,omitempty"`
	// DailyTokens 每日 token 上限（UTC+8 零点重置）；0 = 不限。
	DailyTokens int64 `json:"daily_tokens,omitempty"`
	// ContentLog 是否归档该密钥的对话正文（需全局开关同时打开）。
	ContentLog bool `json:"content_log,omitempty"`
	// Note 备注。
	Note string `json:"note,omitempty"`
}

// UnmarshalJSON 让 `enabled` 缺省为 true。
//
// 手写 config.json 时漏写 enabled，用零值语义会得到「密钥配了但永远 401」——
// 这种静默失败极难排查。显式写 false 仍然是关停，语义不变。
func (k *Key) UnmarshalJSON(b []byte) error {
	type alias Key
	tmp := alias{Enabled: true}
	if err := json.Unmarshal(b, &tmp); err != nil {
		return err
	}
	*k = Key(tmp)
	return nil
}

// Entry 是 Key 的运行期形态：预解析成集合，热路径不做字符串切分。
// 零值 Entry 表示管理员（Admin=true 时 ID 为空）。
type Entry struct {
	ID          string
	Name        string
	Admin       bool
	Enabled     bool
	Allow       map[string]struct{}
	Pin         string
	MinCredit   int64
	Models      map[string]struct{}
	MaxRPM      int
	DailyTokens int64
	ContentLog  bool
	Masked      string

	digest [sha256.Size]byte
}

// AdminEntry 管理员条目：不受任何密钥约束（沿用 api_key / panel.admin_key 的既有语义）。
func AdminEntry() Entry { return Entry{Admin: true, Enabled: true} }

// Table 不可变密钥表。构建后只读，可安全被多个 goroutine 共享
// （livecfg 快照整体替换，不做增量修改）。
type Table struct {
	entries []Entry
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Build 校验并构建密钥表。
//
// reserved 是不得被占用的密钥原文（api_key / panel.admin_key）——重复会让
// 「谁是管理员」变得不可判定，故在配置加载期直接失败，而不是运行期二选一。
func Build(keys []Key, reserved ...string) (*Table, error) {
	t := &Table{entries: make([]Entry, 0, len(keys))}
	seenID := make(map[string]struct{}, len(keys))
	seenKey := make(map[string]struct{}, len(keys)+len(reserved))
	for _, r := range reserved {
		if r = strings.TrimSpace(r); r != "" {
			seenKey[r] = struct{}{}
		}
	}
	for i, k := range keys {
		id := strings.TrimSpace(k.ID)
		if !idPattern.MatchString(id) {
			return nil, fmt.Errorf("api_keys[%d].id: %q 不合法（小写字母/数字/_/-，1-32 位，且不以符号开头）", i, k.ID)
		}
		if _, dup := seenID[id]; dup {
			return nil, fmt.Errorf("api_keys[%d].id: %q 重复", i, id)
		}
		seenID[id] = struct{}{}

		raw := strings.TrimSpace(k.Key)
		if raw == "" {
			return nil, fmt.Errorf("api_keys[%d] (%s).key: 不能为空", i, id)
		}
		if _, dup := seenKey[raw]; dup {
			return nil, fmt.Errorf("api_keys[%d] (%s).key: 与其它密钥重复（含 api_key / panel.admin_key）", i, id)
		}
		seenKey[raw] = struct{}{}

		if k.MinCredit < 0 {
			return nil, fmt.Errorf("api_keys[%d] (%s).min_credit: 不能为负", i, id)
		}
		if k.MaxRPM < 0 {
			return nil, fmt.Errorf("api_keys[%d] (%s).max_rpm: 不能为负", i, id)
		}
		if k.DailyTokens < 0 {
			return nil, fmt.Errorf("api_keys[%d] (%s).daily_tokens: 不能为负", i, id)
		}

		name := strings.TrimSpace(k.Name)
		if name == "" {
			name = id
		}
		t.entries = append(t.entries, Entry{
			ID:          id,
			Name:        name,
			Enabled:     k.Enabled,
			Allow:       toSet(k.Accounts),
			Pin:         strings.TrimSpace(k.Pin),
			MinCredit:   k.MinCredit,
			Models:      toSet(k.Models),
			MaxRPM:      k.MaxRPM,
			DailyTokens: k.DailyTokens,
			ContentLog:  k.ContentLog,
			Masked:      Mask(raw),
			digest:      sha256.Sum256([]byte(raw)),
		})
	}
	return t, nil
}

// Resolve 从请求头解析调用方密钥。
//
// 返回 (Entry, true) 表示鉴权通过；Entry.Admin 为 true 即管理员。
// 失败只有一种对外表现（调用方统一回 401），不区分「不存在」「已禁用」。
func (t *Table) Resolve(r *http.Request) (Entry, bool) {
	if t == nil || len(t.entries) == 0 {
		return Entry{}, false
	}
	authz := r.Header.Get("Authorization")
	tok := ""
	if strings.HasPrefix(authz, bearerPrefix) {
		tok = authz[len(bearerPrefix):]
	}
	// Anthropic 客户端（Claude Code 等）用 x-api-key 而不是 Authorization，
	// 两套头都认；Authorization 优先，避免误伤既有客户端。
	if tok == "" {
		tok = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	d := sha256.Sum256([]byte(tok))
	// 全表比较：不 break，命中位置不影响耗时形状。
	hit := -1
	for i := range t.entries {
		if subtle.ConstantTimeCompare(d[:], t.entries[i].digest[:]) == 1 {
			hit = i
		}
	}
	if hit < 0 {
		return Entry{}, false
	}
	e := t.entries[hit]
	if !e.Enabled {
		return Entry{}, false
	}
	return e, true
}

// ByID 按 id 取条目（面板编辑用；不含明文密钥）。
func (t *Table) ByID(id string) (Entry, bool) {
	if t == nil {
		return Entry{}, false
	}
	for _, e := range t.entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Entries 返回全部条目副本（面板列表用）。
func (t *Table) Entries() []Entry {
	if t == nil {
		return nil
	}
	out := make([]Entry, len(t.entries))
	copy(out, t.entries)
	return out
}

// Len 返回密钥条目数。
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.entries)
}

// Mask 生成展示用掩码：保留前 6 位与后 4 位，中间省略。
// 过短的密钥整体打码，避免「掩码」反而泄露大部分内容。
func Mask(key string) string {
	if key == "" {
		return ""
	}
	r := []rune(key)
	if len(r) <= 12 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:6]) + "…" + string(r[len(r)-4:])
}

func toSet(items []string) map[string]struct{} {
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			out[item] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
