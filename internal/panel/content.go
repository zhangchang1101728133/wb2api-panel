// content.go 面板「对话」视图接口：回看密钥的对话正文归档。
//
// 只读，且只对 admin 开放（路由已用 withAuth → 管理密钥）。归档本体由
// internal/contentlog 负责；这里只做参数校验与响应整形。
package panel

import (
	"net/http"
	"strconv"
)

// contentDays 返回有归档的日期（可按密钥过滤），供前端下拉。
func (p *Panel) contentDays(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ContentLog == nil || !p.cfg.ContentLog.Enabled() {
		writeErr(w, http.StatusNotImplemented, "content archive disabled")
		return
	}
	key := r.URL.Query().Get("key")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"days":      p.cfg.ContentLog.Days(key),
		"keep_days": p.cfg.ContentLog.KeepDays(),
	})
}

// contentList 返回某天的归档记录（含请求体与响应正文）。
//
// 参数：key（空 = 全部）、date（空 = 今天）、limit（默认 50，上限 200）。
// 返回按时间正序，取最近 limit 条。
func (p *Panel) contentList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ContentLog == nil || !p.cfg.ContentLog.Enabled() {
		writeErr(w, http.StatusNotImplemented, "content archive disabled")
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	recs, err := p.cfg.ContentLog.Query(q.Get("key"), q.Get("date"), limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"records": recs,
	})
}
