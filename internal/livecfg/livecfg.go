// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikey"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey string // 网关鉴权密钥；空 = 不鉴权（既有单密钥语义，仍然有效）
	// AdminKey 管理面专用密钥（panel.admin_key）。空 = 只用 APIKey。
	// 与 APIKey 并列而不是替换：老客户端拿 api_key 调 /v1/* 不受影响。
	AdminKey string
	// Keys 多密钥表（api_keys）。nil / 空表 = 未启用多密钥。
	// 表本身不可变，快照整体替换即完成热更新。
	Keys                 *apikey.Table
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
	RecordClientInfo     bool          // 请求日志是否记录调用来源（客户端 IP / UA）
	HiddenModels         map[string]struct{} // 精确隐藏的模型 id（已归一化为 "cn:x"/"global:x" 全量形式）；nil = 不隐藏
	// ContentArchive 对话正文归档全局开关（logging.content_archive.enabled）。
	// 密钥的 content_log 只是第二道闸：两者都开才归档。
	ContentArchive bool
}

// IsAdmin 判定请求是否持有管理员凭证。
//
// 两个密钥都为空 = 未启用鉴权（既有语义），恒为 true。
// APIKey 与 AdminKey 同时有效：前者兼容存量客户端，后者可单独轮换。
func (s Snapshot) IsAdmin(r *http.Request) bool {
	if s.APIKey == "" && s.AdminKey == "" {
		return true
	}
	if s.APIKey != "" && httpauth.VerifyBearer(r, s.APIKey) {
		return true
	}
	if s.AdminKey != "" && httpauth.VerifyBearer(r, s.AdminKey) {
		return true
	}
	return false
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }

// NormalizeHiddenModels 把用户配置的 hidden 条目归一化为精确 id 集合。
//
// 条目带合法 "cn:"/"global:" 前缀 → 只隐藏该域；裸名 → 两域都隐藏。
// 大小写敏感（与 internal/server.resolveModel 的前缀规则对称：取第一个 ":",
// 前段恰为 "cn"/"global" 才剥离）。返回的 map 只读共享，调用方不得修改。
func NormalizeHiddenModels(hidden []string) map[string]struct{} {
	if len(hidden) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hidden)*2)
	for _, m := range hidden {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if prefix, bare, ok := strings.Cut(m, ":"); ok && (prefix == "cn" || prefix == "global") {
			out[prefix+":"+bare] = struct{}{}
		} else {
			out["cn:"+m] = struct{}{}
			out["global:"+m] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
