// Package keyusage 按调用方密钥累计用量（请求数 / token / 积分 / 模型分布），
// 并提供 RPM 与每日 token 两个限额判定。
//
// 与 internal/usage 的分工：usage 按「账号 × 模型」聚合，回答"这个号被用了多少"；
// 本包按「密钥 × 天」聚合，回答"这个朋友用了多少"。两者互不替代，故独立落盘
// （keyusage.json），避免改动既有聚合结构。
//
// 日界按 UTC+8 零点（与签到 / 402 硬冷却同一口径）。数据保留 90 天。
package keyusage

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// retentionDays 分桶保留天数。超过的日期在 flush 时清理。
	retentionDays = 90
	// flushInterval 定时落盘间隔。
	flushInterval = 60 * time.Second
)

// cst UTC+8 固定时区（不依赖宿主 tzdata）。
var cst = time.FixedZone("UTC+8", 8*3600)

func dayKey(t time.Time) string { return t.In(cst).Format("2006-01-02") }

// Delta 一次尝试的用量增量。
type Delta struct {
	PromptTokens     int64
	CompletionTokens int64
	Credit           float64
	HasCredit        bool
}

// ModelDay 单模型的分桶。
type ModelDay struct {
	Requests   int64   `json:"requests"`
	Prompt     int64   `json:"prompt_tokens"`
	Completion int64   `json:"completion_tokens"`
	Credit     float64 `json:"credit"`
}

// Day 单日汇总。
type Day struct {
	Requests   int64                `json:"requests"`
	OK         int64                `json:"ok"`
	Failed     int64                `json:"failed"`
	Prompt     int64                `json:"prompt_tokens"`
	Completion int64                `json:"completion_tokens"`
	Credit     float64              `json:"credit"`
	Models     map[string]*ModelDay `json:"models,omitempty"`
}

// Tokens 返回 prompt + completion。
func (d Day) Tokens() int64 { return d.Prompt + d.Completion }

func (d *Day) add(delta Delta, ok bool) {
	d.Requests++
	if ok {
		d.OK++
	} else {
		d.Failed++
	}
	d.Prompt += delta.PromptTokens
	d.Completion += delta.CompletionTokens
	if delta.HasCredit {
		d.Credit += delta.Credit
	}
}

type keyState struct {
	Days       map[string]*Day `json:"days"`
	LastActive time.Time       `json:"last_active,omitempty"`
}

type fileData struct {
	Version int                  `json:"version"`
	Keys    map[string]*keyState `json:"keys"`
}

// Stat 面板展示用的一条密钥汇总。
type Stat struct {
	ID         string    `json:"id"`
	Today      Day       `json:"today"`
	Total      Day       `json:"total"`
	LastActive time.Time `json:"last_active,omitempty"`
}

// Recorder 并发安全的按密钥用量记录器。
type Recorder struct {
	mu   sync.Mutex
	path string
	data fileData
	// wins RPM 滑动窗口：keyID → 最近一分钟内的请求时刻（升序）。
	wins map[string][]time.Time
	// dirty 自上次落盘后是否有变更。
	dirty bool

	stop     chan struct{}
	done     chan struct{}
	startOne sync.Once
	stopOne  sync.Once
}

// New 创建记录器（path 为空 = 纯内存，不落盘）。
func New(path string) *Recorder {
	return &Recorder{
		path: path,
		data: fileData{Version: 1, Keys: map[string]*keyState{}},
		wins: map[string][]time.Time{},
	}
}

// Start 加载磁盘数据并启动定时落盘。
func (r *Recorder) Start() {
	r.startOne.Do(func() {
		r.load()
		r.stop = make(chan struct{})
		r.done = make(chan struct{})
		go r.loop()
	})
}

func (r *Recorder) loop() {
	defer close(r.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.flush(false)
		case <-r.stop:
			return
		}
	}
}

// Stop 停掉定时器并落盘。
func (r *Recorder) Stop() {
	r.stopOne.Do(func() {
		if r.stop != nil {
			close(r.stop)
			<-r.done
		}
		r.flush(true)
	})
}

func (r *Recorder) load() {
	if r.path == "" {
		return
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: [keyusage] 读取 %s 失败: %v", r.path, err)
		}
		return
	}
	var d fileData
	if err := json.Unmarshal(raw, &d); err != nil {
		log.Printf("WARN: [keyusage] 解析 %s 失败（将重新开始）: %v", r.path, err)
		return
	}
	if d.Keys == nil {
		d.Keys = map[string]*keyState{}
	}
	r.mu.Lock()
	r.data = d
	r.mu.Unlock()
}

// Add 记一次尝试。ok = 本次是否拿到上游用量（与 usage 视图同口径）。
func (r *Recorder) Add(now time.Time, keyID, model string, d Delta, ok bool) {
	if keyID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ks := r.data.Keys[keyID]
	if ks == nil {
		ks = &keyState{Days: map[string]*Day{}}
		r.data.Keys[keyID] = ks
	}
	if ks.Days == nil {
		ks.Days = map[string]*Day{}
	}
	day := ks.Days[dayKey(now)]
	if day == nil {
		day = &Day{}
		ks.Days[dayKey(now)] = day
	}
	day.add(d, ok)
	if model != "" {
		if day.Models == nil {
			day.Models = map[string]*ModelDay{}
		}
		md := day.Models[model]
		if md == nil {
			md = &ModelDay{}
			day.Models[model] = md
		}
		md.Requests++
		md.Prompt += d.PromptTokens
		md.Completion += d.CompletionTokens
		if d.HasCredit {
			md.Credit += d.Credit
		}
	}
	ks.LastActive = now
	r.dirty = true
}

// AllowRPM 判定并占用一个 RPM 名额。limit<=0 恒放行。
//
// 未超限时把本次请求计入窗口；超限时返回建议的重试间隔（首条记录滑出窗口的时间）。
func (r *Recorder) AllowRPM(keyID string, limit int, now time.Time) (bool, time.Duration) {
	if limit <= 0 || keyID == "" {
		return true, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := now.Add(-time.Minute)
	hits := r.wins[keyID]
	i := 0
	for i < len(hits) && !hits[i].After(cut) {
		i++
	}
	hits = hits[i:]
	if len(hits) >= limit {
		r.wins[keyID] = hits
		wait := hits[0].Add(time.Minute).Sub(now)
		if wait < time.Second {
			wait = time.Second
		}
		return false, wait
	}
	r.wins[keyID] = append(hits, now)
	return true, 0
}

// TodayTokens 返回该密钥今日已用 token（prompt + completion）。
func (r *Recorder) TodayTokens(keyID string, now time.Time) int64 {
	if keyID == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ks := r.data.Keys[keyID]
	if ks == nil {
		return 0
	}
	if day := ks.Days[dayKey(now)]; day != nil {
		return day.Tokens()
	}
	return 0
}

// Stat 返回单个密钥的今日与累计汇总。
func (r *Recorder) Stat(keyID string, now time.Time) Stat {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Stat{ID: keyID}
	ks := r.data.Keys[keyID]
	if ks == nil {
		return out
	}
	out.LastActive = ks.LastActive
	if day := ks.Days[dayKey(now)]; day != nil {
		out.Today = *day
	}
	for _, day := range ks.Days {
		out.Total.Requests += day.Requests
		out.Total.OK += day.OK
		out.Total.Failed += day.Failed
		out.Total.Prompt += day.Prompt
		out.Total.Completion += day.Completion
		out.Total.Credit += day.Credit
	}
	return out
}

// Snapshot 返回全部密钥的汇总（按今日请求数降序，同数量按 id 升序）。
func (r *Recorder) Snapshot(now time.Time) []Stat {
	r.mu.Lock()
	ids := make([]string, 0, len(r.data.Keys))
	for id := range r.data.Keys {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	sort.Strings(ids)
	out := make([]Stat, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.Stat(id, now))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Today.Requests != out[j].Today.Requests {
			return out[i].Today.Requests > out[j].Today.Requests
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Reset 清掉某密钥的历史（删除密钥时调用，避免旧数据在面板里变成幽灵行）。
func (r *Recorder) Reset(keyID string) {
	r.mu.Lock()
	delete(r.data.Keys, keyID)
	delete(r.wins, keyID)
	r.dirty = true
	r.mu.Unlock()
}

// ResetRPM 清空 RPM 窗口（面板解除限流时用）。
func (r *Recorder) ResetRPM(keyID string) {
	r.mu.Lock()
	delete(r.wins, keyID)
	r.mu.Unlock()
}

func (r *Recorder) flush(force bool) {
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	r.pruneLocked(time.Now())
	raw, err := json.MarshalIndent(r.data, "", "  ")
	r.dirty = false
	path := r.path
	r.mu.Unlock()
	if err != nil || path == "" {
		return
	}
	if err := writeAtomic(path, raw); err != nil {
		log.Printf("WARN: [keyusage] 落盘 %s 失败: %v", path, err)
	}
}

// pruneLocked 丢弃超过保留期的日期桶。调用方需持锁。
func (r *Recorder) pruneLocked(now time.Time) {
	cutoff := now.In(cst).AddDate(0, 0, -retentionDays).Format("2006-01-02")
	for id, ks := range r.data.Keys {
		for day := range ks.Days {
			if day < cutoff { // 日期串可直接字典序比较
				delete(ks.Days, day)
			}
		}
		if len(ks.Days) == 0 && now.Sub(ks.LastActive) > 0 {
			// 全部过期且从未活跃：保留空条目没有意义。
			delete(r.data.Keys, id)
		}
	}
}

// writeAtomic 先写 tmp 再 rename（跨盘/挂载点失败时退回就地覆盖）。
func writeAtomic(path string, raw []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// bind mount 的单文件替换会 EBUSY，退回就地写。
		if werr := os.WriteFile(path, raw, 0o600); werr != nil {
			_ = os.Remove(tmp)
			return err
		}
		_ = os.Remove(tmp)
	}
	return nil
}

// Describe 启动日志用的简短描述。
func (r *Recorder) Describe() string {
	if r.path == "" {
		return "内存模式"
	}
	return r.path
}
