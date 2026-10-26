// Package contentlog 对话正文归档：按「密钥 × 天」落盘完整请求/响应，供面板回看。
//
// 与 reqlog 的分工：reqlog 只记脱敏元数据（谁、什么时候、用了多少 token），
// 本包记正文（用户说了什么、模型回了什么）。正文远比元数据敏感，因此：
//   - 全局开关 logging.content_archive.enabled 默认 false，可热开/关；
//   - 每个密钥还要单独打开 content_log 才归档；
//   - 默认只保留 7 天，启动时与每 6 小时清理过期文件。
//
// 存储形态：<dir>/<YYYY-MM-DD>.jsonl，一行一条记录。按天分文件让「保留 N 天」
// 退化成删文件，不需要重写整个归档。
package contentlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxRecordBytes 单条记录（请求体 + 响应体）上限。超出部分截断并标记
// Truncated——保留「有这次对话」比保留 50MB 的超长上下文更重要。
const maxRecordBytes = 2 << 20

// queueSize 异步写队列长度。写盘失败只记日志，绝不影响聊天请求。
const queueSize = 256

// Record 一次对话的归档记录。
type Record struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Key      string    `json:"key"`
	Model    string    `json:"model,omitempty"`
	Account  string    `json:"account,omitempty"`
	Status   int       `json:"status"`
	Stream   bool      `json:"stream,omitempty"`
	Prompt   int64     `json:"prompt_tokens,omitempty"`
	Complete int64     `json:"completion_tokens,omitempty"`
	// Request 客户端原始请求体（JSON）。
	Request json.RawMessage `json:"request,omitempty"`
	// Response 网关写出的原始响应（非流式为 JSON，流式为 SSE 文本）。
	Response string `json:"response,omitempty"`
	// Text 从响应里抽出的助手正文（流式会把 delta 拼起来），便于直接阅读。
	Text string `json:"text,omitempty"`
	// Truncated 请求或响应因超过单条上限被截断。
	Truncated bool `json:"truncated,omitempty"`
}

// Recorder 异步写入的归档器。dir 为空时整体禁用。
//
// 开关可热改（SetEnabled）：面板里打开/关闭「对话正文归档」立即生效，
// 不必重启进程。关闭只停写入，历史文件仍可查询。
type Recorder struct {
	dir      string
	keepDays atomic.Int64

	mu      sync.RWMutex // 保护 ch/stop/done/enabled
	fileMu  sync.Mutex   // 串行化写盘与删除/清空（删除要重写整个日文件）
	ch      chan Record
	stop    chan struct{}
	done    chan struct{}
	enabled bool
}

// New 创建归档器；keepDays <=0 按 7 天。
func New(dir string, keepDays int) *Recorder {
	if keepDays <= 0 {
		keepDays = 7
	}
	r := &Recorder{dir: dir}
	r.keepDays.Store(int64(keepDays))
	return r
}

// Enabled 是否正在归档（全局开关）。
func (r *Recorder) Enabled() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enabled && r.dir != ""
}

// Dir 归档目录（面板展示用）。
func (r *Recorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// KeepDays 保留天数。
func (r *Recorder) KeepDays() int {
	if r == nil {
		return 0
	}
	return int(r.keepDays.Load())
}

// SetKeepDays 热改保留天数（下一次清理生效）。
func (r *Recorder) SetKeepDays(d int) {
	if r == nil || d <= 0 {
		return
	}
	r.keepDays.Store(int64(d))
}

// SetEnabled 热开/关归档。开启时建目录、清理过期文件并启动写协程；
// 关闭时排空队列后退出。重复调用同一状态为空操作。
func (r *Recorder) SetEnabled(on bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if on == r.enabled {
		r.mu.Unlock()
		return
	}
	if on {
		if r.dir == "" {
			r.mu.Unlock()
			return
		}
		if err := os.MkdirAll(r.dir, 0o700); err != nil {
			log.Printf("WARN: [contentlog] 创建归档目录失败（归档保持关闭）: %v", err)
			r.mu.Unlock()
			return
		}
		r.ch = make(chan Record, queueSize)
		r.stop = make(chan struct{})
		r.done = make(chan struct{})
		r.enabled = true
		ch, stop, done := r.ch, r.stop, r.done
		r.mu.Unlock()
		r.prune()
		go r.loop(ch, stop, done)
		log.Printf("[contentlog] 对话正文归档已启用: %s（保留 %d 天，仅归档显式开启 content_log 的密钥）", r.dir, r.KeepDays())
		return
	}
	r.enabled = false
	stop, done := r.stop, r.done
	r.ch, r.stop, r.done = nil, nil, nil
	r.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	log.Printf("[contentlog] 对话正文归档已关闭（历史文件保留，可在面板继续查看）")
}

// Start 建目录、清理过期文件并启动写协程（启动期等价于 SetEnabled(true)）。
func (r *Recorder) Start() { r.SetEnabled(true) }

// Stop 停掉写协程（排空队列）。
func (r *Recorder) Stop() { r.SetEnabled(false) }

func (r *Recorder) loop(ch chan Record, stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case rec := <-ch:
			r.write(rec)
		case <-ticker.C:
			r.prune()
		case <-stop:
			// 排空队列后退出，避免关停时丢最后几条。
			for {
				select {
				case rec := <-ch:
					r.write(rec)
				default:
					return
				}
			}
		}
	}
}

// Append 入队一条记录。队列满时丢弃并记日志——宁可丢归档，不能阻塞对话。
func (r *Recorder) Append(rec Record) {
	if r == nil {
		return
	}
	r.mu.RLock()
	ch, on := r.ch, r.enabled
	r.mu.RUnlock()
	if !on || ch == nil {
		return
	}
	rec.truncate()
	select {
	case ch <- rec:
	default:
		log.Printf("WARN: [contentlog] 归档队列已满，丢弃一条记录 id=%s", rec.ID)
	}
}

// truncate 限制单条记录体积（请求体 + 响应体）。
func (rec *Record) truncate() {
	budget := maxRecordBytes
	if len(rec.Request) > budget {
		rec.Request = append(json.RawMessage(nil), rec.Request[:budget]...)
		rec.Truncated = true
	}
	budget -= len(rec.Request)
	if len(rec.Response) > budget {
		rec.Response = rec.Response[:budget]
		rec.Truncated = true
	}
	if len(rec.Text) > 64<<10 {
		rec.Text = rec.Text[:64<<10] + "…"
	}
}

func (r *Recorder) write(rec Record) {
	path := filepath.Join(r.dir, rec.Time.In(cst).Format("2006-01-02")+".jsonl")
	raw, err := json.Marshal(rec)
	if err != nil {
		log.Printf("WARN: [contentlog] 序列化失败: %v", err)
		return
	}
	// fileMu 与删除/清空互斥：删除是「读全文 → 重写 → rename」，若此时有 app 写
	// 进旧 inode，rename 后那一行就丢了。
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("WARN: [contentlog] 打开 %s 失败: %v", path, err)
		return
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		log.Printf("WARN: [contentlog] 写入 %s 失败: %v", path, err)
	}
	_ = f.Close()
}

// Delete 删除归档记录，返回实际删除的条数。
//
//   - date 必填（归档按天分文件），为空按今天；
//   - id 非空：只删这一条；
//   - 否则 key 非空：删该天该密钥的全部记录；
//   - 都不给：删掉这一天的整个文件。
//
// 单条/按密钥删除走「读 → 过滤 → 写临时文件 → rename」，因此不会留下半截文件。
func (r *Recorder) Delete(date, key, id string) (int, error) {
	if r == nil || r.dir == "" {
		return 0, fmt.Errorf("content archive unavailable")
	}
	if date == "" {
		date = time.Now().In(cst).Format("2006-01-02")
	}
	if _, err := time.ParseInLocation("2006-01-02", date, cst); err != nil {
		return 0, fmt.Errorf("invalid date %q", date)
	}
	path := filepath.Join(r.dir, date+".jsonl")

	r.fileMu.Lock()
	defer r.fileMu.Unlock()

	if id == "" && key == "" {
		// 整天删除：先数一下条数，返回值对用户有意义（面板提示「已清除 N 条」）。
		recs, rerr := readRecords(path)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return 0, nil
			}
			return 0, rerr
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		log.Printf("[contentlog] 已删除整天归档 %s（%d 条）", date, len(recs))
		return len(recs), nil
	}

	recs, err := readRecords(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	kept := make([]Record, 0, len(recs))
	removed := 0
	for _, rec := range recs {
		match := false
		if id != "" {
			match = rec.ID == id
		} else {
			match = rec.Key == key
		}
		if match {
			removed++
			continue
		}
		kept = append(kept, rec)
	}
	if removed == 0 {
		return 0, nil
	}
	if len(kept) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		log.Printf("[contentlog] 已删除归档 %s key=%q id=%q（文件已空，整日移除）", date, key, id)
		return removed, nil
	}
	if err := writeRecords(path, kept); err != nil {
		return 0, err
	}
	log.Printf("[contentlog] 已删除归档记录 date=%s key=%q id=%q 共 %d 条", date, key, id, removed)
	return removed, nil
}

// Purge 删除全部归档文件，返回删除的文件数。
func (r *Recorder) Purge() (int, error) {
	if r == nil || r.dir == "" {
		return 0, fmt.Errorf("content archive unavailable")
	}
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".jsonl") && !strings.HasSuffix(e.Name(), ".jsonl.tmp") {
			continue
		}
		if err := os.Remove(filepath.Join(r.dir, e.Name())); err == nil {
			n++
		}
	}
	log.Printf("[contentlog] 已清空归档目录 %s（%d 个文件）", r.dir, n)
	return n, nil
}

// Get 取单条归档的完整记录（date 为空 = 今天）。找不到返回 (nil, nil)。
func (r *Recorder) Get(date, id string) (*Record, error) {
	if r == nil || r.dir == "" {
		return nil, fmt.Errorf("content archive unavailable")
	}
	if date == "" {
		date = time.Now().In(cst).Format("2006-01-02")
	}
	if _, err := time.ParseInLocation("2006-01-02", date, cst); err != nil {
		return nil, fmt.Errorf("invalid date %q", date)
	}
	if id == "" {
		return nil, fmt.Errorf("id required")
	}
	recs, err := readRecords(filepath.Join(r.dir, date+".jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for i := range recs {
		if recs[i].ID == id {
			rec := recs[i]
			return &rec, nil
		}
	}
	return nil, nil
}

// readRecords 读取整天文件（跳过坏行）。
func readRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make([]Record, 0, 128)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// writeRecords 用临时文件 + rename 整份重写（同目录，保证原子）。
func writeRecords(path string, recs []Record) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for i := range recs {
		raw, err := json.Marshal(recs[i])
		if err != nil {
			continue
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// prune 删除超过保留期的日文件。
func (r *Recorder) prune() {
	keep := r.KeepDays()
	cutoff := time.Now().In(cst).AddDate(0, 0, -keep).Format("2006-01-02")
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		date := strings.TrimSuffix(name, ".jsonl")
		if len(date) != len("2006-01-02") {
			continue
		}
		if date < cutoff { // 日期串字典序即时间序
			if err := os.Remove(filepath.Join(r.dir, name)); err == nil {
				log.Printf("[contentlog] 已清理过期归档 %s（保留 %d 天）", name, keep)
			}
		}
	}
}

// Days 返回有归档的日期（新→旧），只列该密钥真的出现过的天。
//
// 不要求全局开关处于打开状态：关掉归档后历史文件仍在，应该还能翻。
func (r *Recorder) Days(key string) []string {
	if r == nil || r.dir == "" {
		return nil
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	dates := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		date := strings.TrimSuffix(name, ".jsonl")
		if len(date) != len("2006-01-02") {
			continue
		}
		if key == "" || r.fileHasKey(filepath.Join(r.dir, name), key) {
			dates = append(dates, date)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	return dates
}

func (r *Recorder) fileHasKey(path, key string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var probe struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(sc.Bytes(), &probe) == nil && probe.Key == key {
			return true
		}
	}
	return false
}

// Query 返回某天（date 为空 = 今天）该密钥的记录，按时间正序。
// key 为空 = 全部密钥。limit<=0 默认 50，上限 200（取最近的 limit 条）。
//
// 与 Days 同理：全局开关关掉后历史仍可读，只有写入会被拦。
func (r *Recorder) Query(key, date string, limit int) ([]Record, error) {
	if r == nil || r.dir == "" {
		return nil, fmt.Errorf("content archive unavailable")
	}
	if date == "" {
		date = time.Now().In(cst).Format("2006-01-02")
	}
	if _, err := time.ParseInLocation("2006-01-02", date, cst); err != nil {
		return nil, fmt.Errorf("invalid date %q", date)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	path := filepath.Join(r.dir, date+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Record{}, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // 单行损坏不该让整天的归档不可读
		}
		if key != "" && rec.Key != key {
			continue
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// cst UTC+8 固定时区（与用量/签到日界同口径，不依赖宿主 tzdata）。
var cst = time.FixedZone("UTC+8", 8*3600)
