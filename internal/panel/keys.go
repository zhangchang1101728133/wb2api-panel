// keys.go 面板「密钥」视图接口：多密钥的增删改、轮换、用量展示。
//
// 分工与 config.go 一致：本文件只做 HTTP 编排 + 局部字段合并，真正的校验、落盘、
// 热生效仍走 main 注入的 SaveKeys（内部复用 saveConfig，因此与启动同一套
// Default+normalize 校验，绝不会写出一个启动不了的表）。
package panel

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikey"
	"github.com/linguo2625469/workbuddy2api-panel/internal/keyusage"
)

// keyPatch 部分更新：指针用于区分「没提交这个字段」与「显式设成空/零」。
type keyPatch struct {
	ID          *string   `json:"id"`
	Name        *string   `json:"name"`
	Enabled     *bool     `json:"enabled"`
	Accounts    *[]string `json:"accounts"`
	Pin         *string   `json:"pin"`
	MinCredit   *int64    `json:"min_credit"`
	Models      *[]string `json:"models"`
	MaxRPM      *int      `json:"max_rpm"`
	DailyTokens *int64    `json:"daily_tokens"`
	ContentLog  *bool     `json:"content_log"`
	Note        *string   `json:"note"`
}

// applyTo 把补丁合并进配置项（只覆盖显式提交的字段）。
func (pt keyPatch) applyTo(k *apikey.Key) {
	if pt.Name != nil {
		k.Name = strings.TrimSpace(*pt.Name)
	}
	if pt.Enabled != nil {
		k.Enabled = *pt.Enabled
	}
	if pt.Accounts != nil {
		k.Accounts = cleanList(*pt.Accounts)
	}
	if pt.Pin != nil {
		k.Pin = strings.TrimSpace(*pt.Pin)
	}
	if pt.MinCredit != nil {
		k.MinCredit = *pt.MinCredit
	}
	if pt.Models != nil {
		k.Models = cleanList(*pt.Models)
	}
	if pt.MaxRPM != nil {
		k.MaxRPM = *pt.MaxRPM
	}
	if pt.DailyTokens != nil {
		k.DailyTokens = *pt.DailyTokens
	}
	if pt.ContentLog != nil {
		k.ContentLog = *pt.ContentLog
	}
	if pt.Note != nil {
		k.Note = strings.TrimSpace(*pt.Note)
	}
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// keysAvailable 检查密钥接口是否可用（main 未注入闭包时返回 501）。
func (p *Panel) keysAvailable(w http.ResponseWriter) bool {
	if p.cfg.LoadKeys == nil || p.cfg.SaveKeys == nil {
		writeErr(w, http.StatusNotImplemented, "keys api not available")
		return false
	}
	return true
}

// keysList 返回全部密钥（密钥本身只回显掩码）+ 各密钥用量 + 账号候选。
func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	if !p.keysAvailable(w) {
		return
	}
	keys, err := p.cfg.LoadKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load keys: "+err.Error())
		return
	}
	now := time.Now()
	stats := map[string]keyusage.Stat{}
	if p.cfg.KeyUsage != nil {
		for _, s := range p.cfg.KeyUsage.Snapshot(now) {
			stats[s.ID] = s
		}
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		row := map[string]any{
			"id":           k.ID,
			"name":         k.Name,
			"masked":       apikey.Mask(k.Key),
			"enabled":      k.Enabled,
			"accounts":     k.Accounts,
			"pin":          k.Pin,
			"min_credit":   k.MinCredit,
			"models":       k.Models,
			"max_rpm":      k.MaxRPM,
			"daily_tokens": k.DailyTokens,
			"content_log":  k.ContentLog,
			"note":         k.Note,
		}
		st, ok := stats[k.ID]
		if !ok {
			st = keyusage.Stat{ID: k.ID}
		}
		row["today"] = st.Today
		row["total"] = st.Total
		row["today_tokens"] = st.Today.Tokens()
		row["total_tokens"] = st.Total.Tokens()
		row["last_active"] = st.LastActive
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"keys":     out,
		"accounts": p.accountChoices(),
	})
}

// accountChoices 账号候选（密钥的可用账号多选用）。
func (p *Panel) accountChoices() []map[string]any {
	list := p.cfg.Pool.List()
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Realm != list[j].Realm {
			return list[i].Realm < list[j].Realm
		}
		return list[i].UID < list[j].UID
	})
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		label := a.Nickname
		if label == "" {
			label = shortUID(a.UID)
		}
		out = append(out, map[string]any{
			"uid":      a.UID,
			"label":    label,
			"realm":    a.Realm,
			"credits":  a.Credits,
			"disabled": a.Disabled,
			"paused":   a.Paused,
		})
	}
	return out
}

func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

// keyCreate 新建密钥：生成 id（可指定）与明文 key，明文只在本次响应里出现一次。
func (p *Panel) keyCreate(w http.ResponseWriter, r *http.Request) {
	if !p.keysAvailable(w) {
		return
	}
	patch, ok := decodeKeyPatch(w, r)
	if !ok {
		return
	}
	p.keysMu.Lock()
	defer p.keysMu.Unlock()

	keys, err := p.cfg.LoadKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load keys: "+err.Error())
		return
	}
	id := ""
	if patch.ID != nil {
		id = strings.TrimSpace(*patch.ID)
	}
	name := ""
	if patch.Name != nil {
		name = strings.TrimSpace(*patch.Name)
	}
	if id == "" {
		id = slugID(name)
	}
	if id == "" {
		id = randomID()
	}
	// 撞名自动加后缀，避免用户为了「加个人」先手动起个不冲突的 id。
	// base 截到 28 位，给 "-99" 这类后缀留位置（id 上限 32）。
	base := id
	if len(base) > 28 {
		base = strings.Trim(base[:28], "-")
	}
	for i := 2; keyIDExists(keys, id); i++ {
		id = base + "-" + strconv.Itoa(i)
	}

	key, err := newSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "gen key: "+err.Error())
		return
	}
	k := apikey.Key{ID: id, Name: name, Key: key, Enabled: true}
	// 新建默认启用；显式提交的约束照常应用（enabled 也可在创建时给 false）。
	if patch.Enabled != nil {
		k.Enabled = *patch.Enabled
	}
	patch.applyTo(&k)
	keys = append(keys, k)
	if err := p.cfg.SaveKeys(keys); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: 新建密钥 id=%s", id)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"id":     id,
		"key":    key,
		"masked": apikey.Mask(key),
	})
}

// keyUpdate 更新单个密钥的约束字段（不含密钥原文）。
func (p *Panel) keyUpdate(w http.ResponseWriter, r *http.Request) {
	if !p.keysAvailable(w) {
		return
	}
	patch, ok := decodeKeyPatch(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	p.keysMu.Lock()
	defer p.keysMu.Unlock()

	keys, err := p.cfg.LoadKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load keys: "+err.Error())
		return
	}
	idx := keyIndex(keys, id)
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	patch.applyTo(&keys[idx])
	if err := p.cfg.SaveKeys(keys); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keyDelete 删除密钥，并清掉它的用量分桶（否则面板会留下幽灵行）。
func (p *Panel) keyDelete(w http.ResponseWriter, r *http.Request) {
	if !p.keysAvailable(w) {
		return
	}
	id := r.PathValue("id")
	p.keysMu.Lock()
	defer p.keysMu.Unlock()

	keys, err := p.cfg.LoadKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load keys: "+err.Error())
		return
	}
	idx := keyIndex(keys, id)
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	keys = append(keys[:idx], keys[idx+1:]...)
	if err := p.cfg.SaveKeys(keys); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.cfg.KeyUsage != nil {
		p.cfg.KeyUsage.Reset(id)
	}
	log.Printf("panel: 删除密钥 id=%s", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keyRotate 轮换密钥原文（约束与用量保留），明文只在本次响应里出现一次。
func (p *Panel) keyRotate(w http.ResponseWriter, r *http.Request) {
	if !p.keysAvailable(w) {
		return
	}
	id := r.PathValue("id")
	p.keysMu.Lock()
	defer p.keysMu.Unlock()

	keys, err := p.cfg.LoadKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load keys: "+err.Error())
		return
	}
	idx := keyIndex(keys, id)
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	key, err := newSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "gen key: "+err.Error())
		return
	}
	keys[idx].Key = key
	if err := p.cfg.SaveKeys(keys); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: 轮换密钥 id=%s", id)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"key":    key,
		"masked": apikey.Mask(key),
	})
}

// keyResetUsage 清零某密钥的用量与 RPM 窗口（不动密钥本身）。
func (p *Panel) keyResetUsage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.KeyUsage == nil {
		writeErr(w, http.StatusNotImplemented, "keyusage not available")
		return
	}
	id := r.PathValue("id")
	p.cfg.KeyUsage.Reset(id)
	p.cfg.KeyUsage.ResetRPM(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func decodeKeyPatch(w http.ResponseWriter, r *http.Request) (keyPatch, bool) {
	var pt keyPatch
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return pt, false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return pt, true
	}
	if err := json.Unmarshal(raw, &pt); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return pt, false
	}
	return pt, true
}

func keyIndex(keys []apikey.Key, id string) int {
	for i := range keys {
		if keys[i].ID == id {
			return i
		}
	}
	return -1
}

func keyIDExists(keys []apikey.Key, id string) bool { return keyIndex(keys, id) >= 0 }

// slugID 把展示名压成合法 id（小写字母/数字/_/-，1-32 位）；压不出东西时返回空。
func slugID(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '_' || r == '-':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			// 中文名等非 ASCII 字符直接丢弃：id 要能安全用作文件名/统计键。
		}
		if b.Len() >= 32 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func randomID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "key"
	}
	return "key-" + base64.RawURLEncoding.EncodeToString(buf)
}

// newSecret 生成 sk- 前缀的 32 字符密钥（24 字节熵）。
func newSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func itoa(n int) string { return strconv.Itoa(n) }
