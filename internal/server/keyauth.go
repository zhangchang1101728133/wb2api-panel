package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikey"
)

// keyLimitError 判定密钥限额，返回 (状态码, 错误码, 文案, 建议重试秒数)。
// 状态码 0 = 未超限。单独抽出是因为两个协议入口（OpenAI 兼容 / Anthropic 兼容）
// 需要各自的错误外壳，但限流判定必须只有一份。
func (h *Handler) keyLimitError(caller apikey.Entry, now time.Time) (int, string, string, int) {
	if h.cfg.KeyUsage == nil || caller.Admin {
		return 0, "", "", 0
	}
	if caller.MaxRPM > 0 {
		ok, wait := h.cfg.KeyUsage.AllowRPM(caller.ID, caller.MaxRPM, now)
		if !ok {
			secs := int(wait.Seconds())
			if wait > time.Duration(secs)*time.Second {
				secs++
			}
			if secs < 1 {
				secs = 1
			}
			return http.StatusTooManyRequests, "rate_limit_exceeded",
				fmt.Sprintf("requests per minute limit (%d) exceeded for this API key, retry in %ds", caller.MaxRPM, secs), secs
		}
	}
	if caller.DailyTokens > 0 {
		if used := h.cfg.KeyUsage.TodayTokens(caller.ID, now); used >= caller.DailyTokens {
			return http.StatusTooManyRequests, "quota_exceeded",
				fmt.Sprintf("daily token quota (%d) exhausted for this API key", caller.DailyTokens), 0
		}
	}
	return 0, "", "", 0
}

// enforceKeyLimits 检查调用方密钥的 RPM 与每日 token 上限；超限时直接写 429 并返回 false。
//
// 每日 token 是**事后记账**：入口只判「今日已用 ≥ 上限」，因此单个请求可以略微
// 越过上限（大 prompt 无法预知），但下一条立刻被拦。这是刻意的取舍——预扣需要
// 准确估算 token，估错会误伤正常请求。
func (h *Handler) enforceKeyLimits(w http.ResponseWriter, caller apikey.Entry, now time.Time) bool {
	status, code, msg, retry := h.keyLimitError(caller, now)
	if status == 0 {
		return true
	}
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
	}
	writeOpenAIError(w, status, code, msg)
	return false
}

// callerAllows 判断账号是否在该密钥的允许集合内（空集合 = 全池）。
func callerAllows(e apikey.Entry, uid string) bool {
	if len(e.Allow) == 0 {
		return true
	}
	_, ok := e.Allow[uid]
	return ok
}

// callerAllowsModel 判断模型是否在该密钥的白名单内。
//
// 条目支持三种写法：
//   - "deepseek-v4-flash"  两域都放行
//   - "cn:deepseek-v4-flash" 只放行该域
//   - "deepseek-*"         前缀通配（避免每加一个模型就改一次配置）
//
// 管理员条目与空名单恒放行（既有行为）。
func callerAllowsModel(e apikey.Entry, realm, bareModel string) bool {
	if e.Admin || len(e.Models) == 0 {
		return true
	}
	qualified := realm + ":" + bareModel
	if _, ok := e.Models[qualified]; ok {
		return true
	}
	if _, ok := e.Models[bareModel]; ok {
		return true
	}
	for pat := range e.Models {
		prefix, wild := strings.CutSuffix(pat, "*")
		if !wild || prefix == "" {
			continue
		}
		if strings.HasPrefix(bareModel, prefix) || strings.HasPrefix(qualified, prefix) {
			return true
		}
	}
	return false
}

// filterModelsForCaller 按密钥白名单裁剪 /v1/models 列表（保持原顺序）。
func filterModelsForCaller(list []map[string]any, e apikey.Entry) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		id, _ := item["id"].(string)
		realm, bare := "cn", id
		if prefix, rest, ok := strings.Cut(id, ":"); ok && (prefix == "cn" || prefix == "global") {
			realm, bare = prefix, rest
		}
		if callerAllowsModel(e, realm, bare) {
			out = append(out, item)
		}
	}
	return out
}
