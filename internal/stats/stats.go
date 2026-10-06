// Package stats aggregates per-day token usage by model and account,
// persisted to data/stats.json with a rolling retention window.
package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Record struct {
	Model      string
	AccountID  string
	Prompt     int64
	Completion int64
	Requests   int64
	Failures   int64
	LatencyMS  int64
}

type bucket struct {
	Requests   int64            `json:"requests"`
	Failures   int64            `json:"failures"`
	Prompt     int64            `json:"prompt_tokens"`
	Completion int64            `json:"completion_tokens"`
	// 耗时累加：tok/s 的分子是 completion token、分母是累计耗时，
	// 所以必须累加「整段耗时之和」和「有耗时的请求数」，不能只留最后一次。
	// 口径与 workbuddy-free 的 LatencyMsSum / LatencyCount 一致。
	LatencyMsSum int64            `json:"latency_ms_sum"`
	LatencyCount int64            `json:"latency_count"`
	// LatencyCompletion：**只有**带耗时的那部分 completion token。
	//
	// tok/s 的分子必须用它，不能用 cumpletion 总量：历史数据（本次改动上线前
	// 落盘的桶）只有 token、没有耗时，两者混算会得到荒谬的比值
	// （实测出现过 75649 tok/s —— 分子是全天的 token、分母只有 1 次请求的耗时）。
	LatencyCompletion int64 `json:"latency_completion"`
	// AvgTokensPerSec 区间聚合吞吐 = completion token / 累计耗时。
	// 与 workbuddy-free 的 `avg_tokens_per_sec` **同一算法**（长请求加权更重，
	// 比"逐请求速率再平均"更贴近真实）。没有耗时数据时为 0，前端据此隐藏该行。
	// 用 `-` 让它不参与落盘（每次 Report 时现算），避免把派生值写进 stats.json。
	AvgTokensPerSec float64 `json:"avg_tokens_per_sec"`
	ByModel    map[string]*cell `json:"by_model"`
	ByAccount  map[string]*cell `json:"by_account"`
}

type cell struct {
	Requests   int64 `json:"requests"`
	Failures   int64 `json:"failures"`
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
}

type file struct {
	Days map[string]*bucket `json:"days"`
	// Hours 是**逐小时**桶，键为 `2006-01-02T15`（本地时区，与 Days 一致）。
	//
	// 存在的理由：只看按天的桶，一天的节奏全被抹平了（用户 2026-10-02 要求「今日按小时统计」）。
	// 天与小时**同时记**、互不替代：天桶负责 7 天 / 30 天档与历史保留，
	// 小时桶只负责「今日」这条曲线，所以它只留最近 keepHours 个，不占长期体积。
	Hours   map[string]*bucket `json:"hours"`
	Grand   bucket             `json:"grand"`
	SavedAt string             `json:"saved_at"`
}

// keepHours 是小时桶的保留个数。只服务「今日」视图，留一个自然日多一点点
// （跨零点时凌晨仍能看到前一天的尾巴），不需要跟 stats_keep_days 一样长。
const keepHours = 26

type Recorder struct {
	mu       sync.Mutex
	path     string
	keepDays int
	data     file
}

func Open(path string, keepDays int) (*Recorder, error) {
	r := &Recorder{path: path, keepDays: keepDays, data: file{
		Days:  map[string]*bucket{},
		Hours: map[string]*bucket{},
	}}
	raw, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(raw, &r.data)
		if r.data.Days == nil {
			r.data.Days = map[string]*bucket{}
		}
		if r.data.Hours == nil {
			r.data.Hours = map[string]*bucket{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return r, nil
}

func (r *Recorder) Add(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	// 同一笔记录同时落进「天」与「小时」两个桶：天桶撑 7 天 / 30 天档，
	// 小时桶撑「今日」档（用户 2026-10-02 要求今日按小时统计）。
	addTo(r.data.Days, now.Format("2006-01-02"), rec)
	addTo(r.data.Hours, now.Format("2006-01-02T15"), rec)

	r.data.Grand.Requests += rec.Requests
	r.data.Grand.Failures += rec.Failures
	r.data.Grand.Prompt += rec.Prompt
	r.data.Grand.Completion += rec.Completion
	r.pruneLocked()
	_ = r.saveLocked()
}

// addTo 把一笔记录累加进 `buckets[key]`（不存在则新建），含模型 / 账号两个维度。
func addTo(buckets map[string]*bucket, key string, rec Record) {
	b := buckets[key]
	if b == nil {
		b = &bucket{ByModel: map[string]*cell{}, ByAccount: map[string]*cell{}}
		buckets[key] = b
	}
	b.Requests += rec.Requests
	b.Failures += rec.Failures
	b.Prompt += rec.Prompt
	b.Completion += rec.Completion
	// 只在真的有耗时（>0）时累加，避免把"没量到耗时"的请求算进分母、把 tok/s 拉高
	if rec.LatencyMS > 0 {
		b.LatencyMsSum += rec.LatencyMS
		b.LatencyCount++
		b.LatencyCompletion += rec.Completion
	}
	get := func(m map[string]*cell, key string) *cell {
		if key == "" {
			key = "unknown"
		}
		c := m[key]
		if c == nil {
			c = &cell{}
			m[key] = c
		}
		return c
	}
	get(b.ByModel, rec.Model).Requests += rec.Requests
	get(b.ByModel, rec.Model).Failures += rec.Failures
	get(b.ByModel, rec.Model).Prompt += rec.Prompt
	get(b.ByModel, rec.Model).Completion += rec.Completion
	get(b.ByAccount, rec.AccountID).Requests += rec.Requests
	get(b.ByAccount, rec.AccountID).Failures += rec.Failures
	get(b.ByAccount, rec.AccountID).Prompt += rec.Prompt
	get(b.ByAccount, rec.AccountID).Completion += rec.Completion
}

func (r *Recorder) pruneLocked() {
	if r.keepDays > 0 && len(r.data.Days) > r.keepDays {
		keys := make([]string, 0, len(r.data.Days))
		for day := range r.data.Days {
			keys = append(keys, day)
		}
		sort.Strings(keys)
		for _, day := range keys[:len(keys)-r.keepDays] {
			delete(r.data.Days, day)
		}
	}
	// 小时桶独立按 keepHours 修剪：它只服务「今日」，留多了纯占体积。
	if len(r.data.Hours) > keepHours {
		keys := make([]string, 0, len(r.data.Hours))
		for hour := range r.data.Hours {
			keys = append(keys, hour)
		}
		sort.Strings(keys)
		for _, hour := range keys[:len(keys)-keepHours] {
			delete(r.data.Hours, hour)
		}
	}
}

func (r *Recorder) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	r.data.SavedAt = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(r.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// Report summarizes a range: today | 7d | 30d | all.
//
// `today` 的 `series` 走**逐小时**（用户 2026-10-02 要求「今日按小时统计」）：
// 一天只有一根日柱的话，节奏全被抹平，看不出哪个时段忙。
// 其余档位仍然是按天。`totals` / `by_model` / `by_account` 一律按天算 ——
// 它们回答的是「这段时间一共多少」，与序列是同一份数据的两种切法，不重复累加。
func (r *Recorder) Report(rangeName string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	days := r.collectDays(rangeName)
	totals := bucket{ByModel: map[string]*cell{}, ByAccount: map[string]*cell{}}
	for _, day := range days {
		mergeBucket(&totals, r.data.Days[day])
	}
	series := r.seriesDays(days)
	if rangeName == "today" {
		series = r.seriesHours()
	}
	// totals 是 bucket 结构体，序列化时会把 AvgTokensPerSec 一并带出去；
	// 这里先把派生值算好（bucket 本身不存它）。
	totals.AvgTokensPerSec = tokensPerSec(&totals)
	grand := map[string]any{
		"requests":          r.data.Grand.Requests,
		"failures":          r.data.Grand.Failures,
		"prompt_tokens":     r.data.Grand.Prompt,
		"completion_tokens": r.data.Grand.Completion,
	}
	return map[string]any{
		"range":      rangeName,
		"totals":     totals,
		"series":     series,
		"by_model":   ranked(totals.ByModel),
		"by_account": ranked(totals.ByAccount),
		"grand":      grand,
	}
}

func (r *Recorder) seriesDays(days []string) []map[string]any {
	out := make([]map[string]any, 0, len(days))
	for _, day := range days {
		out = append(out, bucketPoint(day, r.data.Days[day]))
	}
	return out
}

// seriesHours 今日逐小时序列：从 00:00 补到**当前这一小时**（含）。
//
// 不把 24 小时全铺出来：未来小时恒为 0，曲线会在当前时刻一头栽回基线，
// 看着像故障。铺到当下为止，右端永远是「刚发生的」。
func (r *Recorder) seriesHours() []map[string]any {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	out := make([]map[string]any, 0, now.Hour()+1)
	for h := 0; h <= now.Hour(); h++ {
		key := start.Add(time.Duration(h) * time.Hour).Format("2006-01-02T15")
		out = append(out, bucketPoint(key, r.data.Hours[key]))
	}
	return out
}

// bucketPoint 把桶摊成一个序列点。桶为 nil（该时段没有请求）也要出点、值全 0：
// 横轴必须连续，缺格会让曲线断开（与 cli2api 服务端补零的口径一致）。
func bucketPoint(key string, b *bucket) map[string]any {
	point := map[string]any{
		"day":               key,
		"requests":          int64(0),
		"failures":          int64(0),
		"prompt_tokens":     int64(0),
		"completion_tokens": int64(0),
	}
	if b == nil {
		return point
	}
	point["requests"] = b.Requests
	point["failures"] = b.Failures
	point["prompt_tokens"] = b.Prompt
	point["completion_tokens"] = b.Completion
	point["avg_tokens_per_sec"] = tokensPerSec(b)
	return point
}

// tokensPerSec 区间聚合吞吐 = completion token / 累计耗时（秒）。
//
// 口径与 workbuddy-free 的 `avg_tokens_per_sec` 一致：分子只用 completion
// （生成量，prompt 是"读进去"的不算生成速度），分母是**累计**耗时而不是平均耗时
// —— 长请求在两者里权重不同，用累计更贴近真实体感。
//
// 分子用 `LatencyCompletion`（只有带耗时那部分 token）而不是 `Completion`：
// 本次改动上线前落盘的历史桶没有耗时，用它当分子会算出 75649 这种荒谬值。
// 没有耗时数据（LatencyCount==0）时返回 0，前端据此隐藏该行。
func tokensPerSec(b *bucket) float64 {
	if b == nil || b.LatencyCount == 0 || b.LatencyMsSum <= 0 {
		return 0
	}
	return float64(b.LatencyCompletion) * 1000 / float64(b.LatencyMsSum)
}

// collectDays 返回该档位要画的日子，**按自然日铺满固定格数**。
//
// 用户 2026-10-03（m07135）：「7天和30天需要按workbuddy的处理，7天默认显示7格，
// 30天默认显示30格」—— workbuddy 的服务端本来就是按自然日铺满的，而这里以前是
// 「取已有桶的最后 N 个」：只跑过一天就只返回 1 个点，柱状图/曲线被压成一根，
// 横轴也看不出节奏。现在改成以今天为右端往前数 N 天，缺失的日子照样出点
// （值为 0，`bucketPoint` 已经处理 nil 桶），横轴连续。
//
// 注意 `totals` / `by_model` / `by_account` 也是按这份日期列表累加的：补出来的
// 空日子在 `mergeBucket` 里遇到 nil 桶会直接跳过，不会污染统计。
func (r *Recorder) collectDays(rangeName string) []string {
	switch rangeName {
	case "today":
		return []string{time.Now().Format("2006-01-02")}
	case "7d":
		return calendarDays(7)
	case "30d":
		return calendarDays(30)
	}
	// 其它档位（all）：有多少算多少，按日期升序。
	keys := make([]string, 0, len(r.data.Days))
	for day := range r.data.Days {
		keys = append(keys, day)
	}
	sort.Strings(keys)
	return keys
}

// calendarDays 以今天为右端往前数 n 天（含今天），升序返回日期键。
func calendarDays(n int) []string {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	out := make([]string, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, start.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return out
}

func mergeBucket(dst *bucket, src *bucket) {
	if src == nil {
		return
	}
	dst.Requests += src.Requests
	dst.Failures += src.Failures
	dst.Prompt += src.Prompt
	dst.Completion += src.Completion
	dst.LatencyMsSum += src.LatencyMsSum
	dst.LatencyCount += src.LatencyCount
	dst.LatencyCompletion += src.LatencyCompletion
	mergeCells(dst.ByModel, src.ByModel)
	mergeCells(dst.ByAccount, src.ByAccount)
}

func mergeCells(dst, src map[string]*cell) {
	for key, c := range src {
		target := dst[key]
		if target == nil {
			target = &cell{}
			dst[key] = target
		}
		target.Requests += c.Requests
		target.Failures += c.Failures
		target.Prompt += c.Prompt
		target.Completion += c.Completion
	}
}

type rankedRow struct {
	Key        string `json:"key"`
	Requests   int64  `json:"requests"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
}

func ranked(cells map[string]*cell) []rankedRow {
	rows := make([]rankedRow, 0, len(cells))
	for key, c := range cells {
		rows = append(rows, rankedRow{
			Key:        key,
			Requests:   c.Requests,
			Prompt:     c.Prompt,
			Completion: c.Completion,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Prompt+rows[i].Completion > rows[j].Prompt+rows[j].Completion
	})
	return rows
}
