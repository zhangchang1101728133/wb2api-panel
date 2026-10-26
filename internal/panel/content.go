// content.go 面板「对话」视图接口：回看密钥的对话正文归档。
//
// 只读，且只对 admin 开放（路由已用 withAuth → 管理密钥）。归档本体由
// internal/contentlog 负责；这里只做参数校验与响应整形。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

// contentDelete 删除归档记录：
//
//	all=true             → 清空全部归档文件；
//	date + key           → 删该天该密钥的全部记录；
//	date + id            → 只删那一条；
//	只给 date            → 删整天的文件。
//
// 归档是运维自己的数据，删掉不可恢复，所以前端每一步都要求二次确认；
// 后端只做参数校验，不做任何「保留副本」的兜底。
func (p *Panel) contentDelete(w http.ResponseWriter, r *http.Request) {
	rec := p.cfg.ContentLog
	if rec == nil {
		writeErr(w, http.StatusNotImplemented, "content archive unavailable")
		return
	}
	var req struct {
		Date string `json:"date"`
		Key  string `json:"key"`
		ID   string `json:"id"`
		All  bool   `json:"all"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
	}
	if req.All {
		n, err := rec.Purge()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "purge: "+err.Error())
			return
		}
		log.Printf("[panel] 清空对话归档 files=%d remote=%s", n, requestIP(r))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": n})
		return
	}
	if req.Date == "" {
		writeErr(w, http.StatusBadRequest, "date required（按天归档，需指定日期）")
		return
	}
	n, err := rec.Delete(req.Date, req.Key, req.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "delete: "+err.Error())
		return
	}
	log.Printf("[panel] 删除对话归档 date=%s key=%q id=%q 条数=%d remote=%s",
		req.Date, req.Key, req.ID, n, requestIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n})
}

// contentDays 返回有归档的日期（可按密钥过滤），供前端下拉。
//
// 全局开关关闭时不再回 501：历史文件还在，前端要能区分「没开」和「没有记录」，
// 所以统一 200 + enabled 标志，由界面决定提示文案。
func (p *Panel) contentDays(w http.ResponseWriter, r *http.Request) {
	rec := p.cfg.ContentLog
	enabled := rec != nil && rec.Enabled()
	key := r.URL.Query().Get("key")
	days := []string{}
	keep := 0
	if rec != nil {
		days = rec.Days(key)
		keep = rec.KeepDays()
	}
	if days == nil {
		days = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"enabled":   enabled,
		"days":      days,
		"keep_days": keep,
	})
}

// contentList 返回某天的归档记录（含请求体与响应正文）。
//
// 参数：key（空 = 全部）、date（空 = 今天）、limit（默认 50，上限 200）。
// 返回按时间正序，取最近 limit 条。
func (p *Panel) contentList(w http.ResponseWriter, r *http.Request) {
	rec := p.cfg.ContentLog
	if rec == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"enabled": false,
			"records": []any{},
		})
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	// before 是翻页游标（RFC3339）：只返回比它更早的记录，配合前端「加载更多」。
	var before time.Time
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid before: "+err.Error())
			return
		}
		before = t
	}
	// 列表只要摘要：QueryMeta 在解析阶段就丢掉请求体/原始响应，避免把上百 MB
	// 正文解进内存（详情展开时再按 id 单条取全文）。
	recs, err := rec.QueryMeta(q.Get("key"), q.Get("date"), limit, 600, before)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"enabled": rec.Enabled(),
		"records": recs,
		"has_more": len(recs) >= limit && limit > 0,
	})
}

// contentItem 取单条归档的完整内容（正文 + 请求体 + 原始响应），供详情展开时加载。
func (p *Panel) contentItem(w http.ResponseWriter, r *http.Request) {
	rec := p.cfg.ContentLog
	if rec == nil {
		writeErr(w, http.StatusNotImplemented, "content archive unavailable")
		return
	}
	q := r.URL.Query()
	id := q.Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	item, err := rec.Get(q.Get("date"), id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if item == nil {
		writeErr(w, http.StatusNotFound, "record not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "record": item})
}

