// versionwatch.go 客户端版本巡检：手动/自动更新国内版与国际版的客户端与 CLI 版本号。
//
// 背景：上游随时抬高客户端版本门槛，版本过老直接 11140 / 请求非法。以前只能改
// 常量重编译；现在版本号走配置且热生效（手动），另有这里按周期从「版本源」拉
// 最新版本自动写回（自动）。版本源可以是官方下载页、自家镜像、或桌面客户端
// 落盘的 product.json（本地文件也支持）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/verwatch"
)

// versionWatchStatus 最近一次巡检结果（面板展示）。
type versionWatchStatus struct {
	mu        sync.Mutex
	running   bool
	lastAt    time.Time
	lastErr   string
	changed   []string
	lastNote  string
}

func (s *versionWatchStatus) begin() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

func (s *versionWatchStatus) finish(err error, changed []string, note string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.lastAt = time.Now()
	s.changed = changed
	s.lastNote = note
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = ""
	}
}

func (s *versionWatchStatus) snapshot() map[string]any {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"running":   s.running,
		"changed":   s.changed,
		"last_note": s.lastNote,
	}
	if !s.lastAt.IsZero() {
		out["last_check"] = s.lastAt.Format(time.RFC3339)
	}
	if s.lastErr != "" {
		out["last_error"] = s.lastErr
	}
	return out
}

func (s *versionWatchStatus) snapshotWith(changed []string) map[string]any {
	out := s.snapshot()
	if out == nil {
		out = map[string]any{}
	}
	out["changed"] = changed
	return out
}

// runVersionWatch 巡检一次：拉源 → 解析 → 比当前新就写回配置（写回即热生效）。
//
// 返回本次实际更新的字段（如 "client_version=5.9.1"）。某个域失败不影响其它域。
func runVersionWatch(ctx context.Context, vw VersionWatch, up *upstream.Client, save func([]byte) ([]string, error), st *versionWatchStatus) ([]string, error) {
	if !st.begin() {
		return nil, fmt.Errorf("版本巡检正在进行中")
	}
	cnClient, cnCLI, intlClient, intlCLI := up.Versions()
	patch := map[string]string{}
	var problems []string
	var notes []string

	check := func(realm string, src verwatch.Source, deflt string, curClient, curCLI, clientKey, cliKey string) {
		url := src.Resolve(deflt)
		text, err := verwatch.Fetch(ctx, url, 20*time.Second)
		if err != nil {
			problems = append(problems, realm+": 拉取失败 "+err.Error())
			return
		}
		res := verwatch.Parse(text, src)
		if res.Client == "" && res.CLI == "" {
			problems = append(problems, realm+": 版本源里没解析到版本号（检查正则）")
			return
		}
		if res.Client != "" && verwatch.Newer(res.Client, curClient) {
			patch[clientKey] = res.Client
			notes = append(notes, fmt.Sprintf("%s client %s → %s", realm, curClient, res.Client))
		}
		if res.CLI != "" && verwatch.Newer(res.CLI, curCLI) {
			patch[cliKey] = res.CLI
			notes = append(notes, fmt.Sprintf("%s cli %s → %s", realm, curCLI, res.CLI))
		}
	}

	check("cn", verwatch.Source{URL: vw.CNURL, ClientRegex: vw.CNClientRegex, CLRegex: vw.CNCliRegex},
		verwatch.ChangelogCN, cnClient, cnCLI, "client_version", "cli_version")
	check("global", verwatch.Source{URL: vw.GlobalURL, ClientRegex: vw.GlobalClientRegex, CLRegex: vw.GlobalCliRegex},
		verwatch.ChangelogGlobal, intlClient, intlCLI, "intl_client_version", "intl_cli_version")

	var changed []string
	var err error
	if len(patch) > 0 {
		body, mErr := json.Marshal(map[string]any{"upstream": patch})
		if mErr != nil {
			err = mErr
		} else if _, sErr := save(body); sErr != nil {
			err = fmt.Errorf("写回配置失败: %w", sErr)
		} else {
			for k, v := range patch {
				changed = append(changed, k+"="+v)
			}
			log.Printf("[verwatch] 已自动更新客户端版本: %s", strings.Join(notes, "; "))
		}
	} else if len(problems) == 0 {
		notes = append(notes, "已是最新")
	}
	if len(problems) > 0 {
		msg := strings.Join(problems, "; ")
		log.Printf("WARN: [verwatch] 巡检未完成: %s", msg)
		if err == nil && len(patch) == 0 {
			err = fmt.Errorf("%s", msg)
		}
	}
	st.finish(err, changed, strings.Join(notes, "; "))
	return changed, err
}

// startVersionWatch 周期巡检。配置每轮从磁盘重读（面板改了源/周期立刻按新的来），
// 用 1 分钟节拍判断是否到期，避免改配置还要重启进程。
func startVersionWatch(ctx context.Context, get func() (VersionWatch, time.Duration), up *upstream.Client, save func([]byte) ([]string, error), st *versionWatchStatus) {
	go func() {
		// 启动后先等 90s：让模型目录预热、账号就位，不和启动流量抢资源。
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
		}
		var lastRun time.Time
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			vw, interval := get()
			if !vw.EnabledOrNil() {
				continue
			}
			if interval <= 0 {
				interval = 6 * time.Hour
			}
			if !lastRun.IsZero() && time.Since(lastRun) < interval {
				continue
			}
			lastRun = time.Now()
			_, _ = runVersionWatch(ctx, vw, up, save, st)
		}
	}()
}
