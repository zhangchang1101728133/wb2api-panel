// quota.go 对外额度查询：给 CC Switch 之类客户端算「这个 key 还剩多少积分」。
//
// 与 /status 的分工：/status 是面板/运维看的完整快照（含今日请求、失败数、
// 账号明细），本接口只回答一个问题——**按这个 key 的权限，还能用多少积分**。
// 因此：
//   - 任何密钥都能调（用户密钥、管理密钥都行），视图按身份收敛；
//   - remaining = 该密钥可用账号的积分合计（受 accounts / pin / min_credit 约束）；
//   - 不返回账号 uid、昵称、冷却原因——那是管理员信息。
package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikey"
	"github.com/linguo2625469/workbuddy2api-panel/internal/keyusage"
)

// callerCredits 汇总「该密钥实际能用到的账号」的积分。
//
// 三个条件缺一不可：
//  1. 在密钥的允许集合内（Allow 为空 = 全池；pin 只是优先，不缩小集合）；
//  2. 积分达到密钥的 min_credit 门槛——达不到的号永远不会被选中，
//     算进来会让用户以为还有额度；
//  3. 账号未被禁用（disabled 是永久态；paused / cooling 只是暂时，积分仍在）。
func (h *Handler) callerCredits(caller apikey.Entry) (remaining, total int64, accounts, available int) {
	for _, s := range h.cfg.Pool.List() {
		if !callerAllows(caller, s.UID) {
			continue
		}
		if caller.MinCredit > 0 && s.Credits < caller.MinCredit {
			continue
		}
		accounts++
		remaining += s.Credits
		total += s.CreditsTotal
		if !s.Disabled && !s.Paused && !s.Cooling {
			available++
		}
	}
	return remaining, total, accounts, available
}

// quota 返回该密钥的剩余积分与限额。
//
// 响应同时满足两种客户端：
//   - 自定义脚本：直接读 isValid / remaining / unit / planName / extra；
//   - CC Switch 内置的 Sub2API 模板：读 isValid / unit / remaining / quota.limit / quota.used。
//
// 路由注册在 /v1/quota 与 /v1/usage（后者对齐 Sub2API 模板的固定路径），
// 鉴权复用 withStatus：管理密钥与用户密钥都能查，但只看到自己权限内的数。
func (h *Handler) quota(w http.ResponseWriter, r *http.Request) {
	caller := keyFrom(r.Context())
	now := time.Now()

	remaining, total, accounts, available := h.callerCredits(caller)

	var today keyusage.Day
	rpmUsed := 0
	if h.cfg.KeyUsage != nil && !caller.Admin {
		st := h.cfg.KeyUsage.Stat(caller.ID, now)
		today = st.Today
		rpmUsed = h.cfg.KeyUsage.RPMUsed(caller.ID, now)
	}

	role := "user"
	scope := "limited"
	keyID, keyName := caller.ID, caller.Name
	if caller.Admin {
		role = "admin"
		keyID, keyName = "admin", "管理员"
	}
	plan := fmt.Sprintf("指定 %d 个号", accounts)
	if len(caller.Allow) == 0 {
		scope = "all"
		plan = "全池"
	}
	if caller.MinCredit > 0 {
		plan += fmt.Sprintf(" · 积分 ≥ %d", caller.MinCredit)
	}
	if keyName != "" {
		plan = keyName + " · " + plan
	} else if keyID != "" {
		plan = keyID + " · " + plan
	}

	dailyLeft := int64(-1) // -1 = 不限额
	if caller.DailyTokens > 0 {
		dailyLeft = caller.DailyTokens - today.Tokens()
		if dailyLeft < 0 {
			dailyLeft = 0
		}
	}

	used := total - remaining
	if used < 0 {
		used = 0
	}
	extra := fmt.Sprintf("%d 个号可用", available)
	if today.Tokens() > 0 {
		extra += fmt.Sprintf(" · 今日 %d tok", today.Tokens())
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service": ServiceName,
		// 下面几个字段直接喂给客户端的提取器（CC Switch 约定）。
		"isValid":   true,
		"unit":      "积分",
		"remaining": remaining,
		"total":     total,
		"used":      used,
		"planName":  plan,
		"extra":     extra,
		"quota": map[string]any{
			"limit":              total,
			"used":               used,
			"remaining":          remaining,
			"unit":               "积分",
			"accounts":           accounts,
			"accounts_available": available,
		},
		"limits": map[string]any{
			"rpm_used":           rpmUsed,
			"rpm_limit":          caller.MaxRPM,
			"daily_tokens_used":  today.Tokens(),
			"daily_tokens_limit": caller.DailyTokens,
			"daily_tokens_left":  dailyLeft,
		},
		"key": map[string]any{
			"id":             keyID,
			"name":           keyName,
			"role":           role,
			"accounts_scope": scope,
		},
		"today": map[string]any{
			"requests": today.Requests,
			"tokens":   today.Tokens(),
		},
		"updated_at": now.UTC().Format(time.RFC3339),
	})
}
