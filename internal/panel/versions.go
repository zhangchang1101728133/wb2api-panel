// versions.go 面板「客户端版本」接口：看当前生效版本、手动触发一次版本巡检。
//
// 上游会随客户端发版抬高版本门槛，版本过老直接 11140 / 请求非法。版本号本身走
// 配置（upstream.client_version / intl_client_version 等，保存即热生效），这里
// 额外提供「立即检查」按钮，让运维不必等后台定时巡检。
package panel

import "net/http"

// versions GET /panel/api/versions → 当前生效的四组版本 + 巡检状态。
func (p *Panel) versions(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"ok": true}
	if p.cfg.UpstreamVersions != nil {
		out["effective"] = p.cfg.UpstreamVersions()
	}
	if p.cfg.VersionStatus != nil {
		out["watch"] = p.cfg.VersionStatus()
	}
	writeJSON(w, http.StatusOK, out)
}

// versionCheck POST /panel/api/versions/check → 立即巡检一次（同步返回结果）。
func (p *Panel) versionCheck(w http.ResponseWriter, r *http.Request) {
	if p.cfg.VersionCheck == nil {
		writeErr(w, http.StatusNotImplemented, "version watch not available")
		return
	}
	res, err := p.cfg.VersionCheck()
	if err != nil {
		// 巡检失败不是服务错误：把原因带上，前端照常展示当前版本。
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "watch": res})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "watch": res})
}
