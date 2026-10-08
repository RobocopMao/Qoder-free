// Package panel serves the embedded admin UI and its JSON API under /panel.
// The panel is unauthenticated by design: it binds to loopback together with
// the rest of the service, while client traffic on /v1/* keeps the API key.
package panel

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"qoder-free/internal/accounts"
	"qoder-free/internal/config"
	"qoder-free/internal/pool"
	"qoder-free/internal/stats"
	"qoder-free/internal/worker"
)

// Version is the release identifier shown in the panel and startup log.
// Release builds override it via:
//
//	go build -ldflags "-X qoder-free/internal/panel.Version=vX.Y.Z" ./cmd/server
var Version = "0.1.0"

type Panel struct {
	Cfg     config.Config
	CfgPath string
	Store   *accounts.Store
	Pool    *pool.Pool
	Manager *worker.Manager
	Stats   *stats.Recorder
	Start   time.Time

	ring *LogRing

	quotaMu    sync.Mutex
	quotaCache map[string]*worker.Quota
	quotaAt    map[string]time.Time

	// checkinWake 用来**立刻叫醒**自动签到的调度器（缓冲 1，非阻塞发送）。
	//
	// 为什么需要：调度器现在会「睡到下一个签到点」（可能十几个小时），
	// 若用户中途在面板上打开某账号的自动签到、或改了签到时间，
	// 不该干等那么久才生效。
	checkinWake chan struct{}
}

func New(cfg config.Config, cfgPath string, store *accounts.Store, pl *pool.Pool, manager *worker.Manager, statsRecorder *stats.Recorder, logs io.Writer) *Panel {
	ring, _ := logs.(*LogRing)
	return &Panel{
		Cfg:         cfg,
		CfgPath:     cfgPath,
		Store:       store,
		Pool:        pl,
		Manager:     manager,
		Stats:       statsRecorder,
		Start:       time.Now(),
		ring:        ring,
		quotaCache:  map[string]*worker.Quota{},
		quotaAt:     map[string]time.Time{},
		checkinWake: make(chan struct{}, 1),
	}
}

// StartQuotaLoop refreshes cached quota for ready accounts every 5 minutes
// so the accounts table always has credits to show without blocking requests.
func (p *Panel) StartQuotaLoop(ctx context.Context) {
	refresh := func() {
		for _, acct := range p.Store.List() {
			if !acct.Enabled || !p.Manager.Running(acct.ID) {
				continue
			}
			if entry := p.Pool.Get(acct.ID); entry == nil || !entry.Ready {
				continue
			}
			url, ok := p.Manager.URL(acct.ID)
			if !ok {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			quota, err := p.Manager.Client(acct.ID).Quota(cctx, url, false)
			cancel()
			if err == nil && quota != nil {
				// Skip empty buckets: workers report zeros before the first warm login.
				if totalQ, _, _ := quota.Remaining(); totalQ > 0 || quota.IsQuotaExceeded {
					p.quotaMu.Lock()
					p.quotaCache[acct.ID] = quota
					p.quotaAt[acct.ID] = time.Now()
					p.quotaMu.Unlock()
				}
			}
		}
	}
	refresh()
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

// StartCheckinLoop 每天为「开启了自动签到」的账号跑一次签到。
// 设计参照 trae-free 的同名调度器，但**不用固定周期轮询**。
//
// 为什么不用固定轮询（用户 m00313）：
// 「如果已经签到了，就不用轮询，不然老是弹一个密钥的弹窗」。
// 取设备令牌要起一个会碰钥匙串的原生二进制，无谓地反复唤醒调度器
// 只会增加触发系统授权弹窗的机会、也白费 CPU。
//
// 所以现在按**下一个签到点**精确睡眠：算出来还有哪一天、几点需要动手，
// 一觉睡到那时候（顺带每 30 分钟醒一次做兜底复查，防止系统休眠/时钟跳变
// 导致长睡眠失效）。已经签完的账号不会再被打扰。
func (p *Panel) StartCheckinLoop(ctx context.Context) {
	p.runAutoCheckin(ctx)

	go func() {
		for {
			wait := p.nextCheckinDelay(time.Now())
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-p.checkinWake:
				// 配置或开关变了，立刻重算。
				timer.Stop()
			case <-timer.C:
				p.runAutoCheckin(ctx)
			}
		}
	}()
}

// wakeCheckinLoop 非阻塞地叫醒签到调度器，让它立刻重算下次唤醒时间。
// 开关变动 / 改了签到时间时调用。
func (p *Panel) wakeCheckinLoop() {
	if p.checkinWake == nil {
		return
	}
	select {
	case p.checkinWake <- struct{}{}:
	default:
		// 已经有一个待处理的唤醒信号，不必重复投递。
	}
}

// nextCheckinDelay 算出距离「下一次需要跑签到」还有多久。
//
// 规则：
//   - 没有开启自动签到的账号 → 睡久一点（1 小时）等唤醒信号即可。
//   - 有账号待签且已过签到点 → 立刻跑（0）。
//   - 有账号待签但还没到签到点 → 睡到今天/明天的签到点。
//   - 全部签完 → 睡到**明天**的签到点。
func (p *Panel) nextCheckinDelay(now time.Time) time.Duration {
	const idlePoll = 30 * time.Minute

	// 注意这里**不设「每小时醒一次」的上限**（用户 m00313：「如果已经签到了，
	// 就不用轮询」）。醒来却无事可做虽然不会触网（日期守卫在发请求前就
	// continue 了），但会让日志和 CPU 白转；而「睡到明天签到点」本身是安全的：
	// 期间任何配置/开关变动都会通过 `checkinWake` 立刻叫醒重算。
	//
	// 只在「全部签完」那条路径给一个下限（60s），防止时钟跳变算出非正间隔
	// 导致 select 空转。

	hour := p.Cfg.CheckinHourLocal
	today := now.Format("2006-01-02")

	hasAuto := false
	for _, acct := range p.Store.List() {
		if !acct.Enabled || !acct.AutoCheckin {
			continue
		}
		hasAuto = true
		signedToday := len(acct.LastCheckinAt) >= 10 && acct.LastCheckinAt[:10] == today
		if !signedToday {
			// 有待签账号：到点就跑，没到点就睡到签到点。
			at := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
			if now.Before(at) {
				return at.Sub(now)
			}
			return 0
		}
	}
	if !hasAuto {
		// 没有账号开启自动签到：低频兜底即可，开关变动会通过 checkinWake 唤醒。
		return idlePoll
	}
	// 全部签完 → 一觉睡到明天的签到点，期间不再轮询。
	tomorrow := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	d := tomorrow.Sub(now)
	if d < time.Minute {
		// 正常情况下不会走到（已过点才会算到明天）；仅防时钟异常导致的死循环。
		return time.Minute
	}
	return d
}

// runAutoCheckin 跑一轮自动签到，返回**是否还有账号等待签到**
// （已开启、但今天还没签上）。调用方据此决定下次轮询间隔。
func (p *Panel) runAutoCheckin(ctx context.Context) bool {
	return p.runAutoCheckinAt(ctx, time.Now())
}

// runAutoCheckinAt 是 runAutoCheckin 的可注入时间的版本，便于测试。
func (p *Panel) runAutoCheckinAt(ctx context.Context, now time.Time) bool {
	if now.Hour() < p.Cfg.CheckinHourLocal {
		return false
	}
	pending := false
	today := now.Format("2006-01-02")
	for _, acct := range p.Store.List() {
		if !acct.Enabled || !acct.AutoCheckin {
			continue
		}
		// LastCheckinAt 是本地 RFC3339，前 10 字节即本地日期 ——
		// 直接比这 10 字节就是「今天是否已签」的守卫。
		// 这个判断要**放在就绪检查之前**：已经签过的账号不该再拉长轮询。
		if len(acct.LastCheckinAt) >= 10 && acct.LastCheckinAt[:10] == today {
			continue
		}
		// 只有「真的打了一轮上游、拿到明确答复」才算结清。
		// 下面任何一条 continue（没跑起来 / 没就绪 / 拿不到地址 / 请求出错）
		// 都意味着这个账号**今天还没签上**，要保住 pending 让调用方稍后重试。
		//
		// 注意用独立的 waiting 标记、不能复用 pending：pending 是「所有账号」的
		// 汇总，一个账号签成功就把它清零，会把**后面还没签的账号**一起漏掉。
		waiting := false
		if !p.Manager.Running(acct.ID) {
			waiting = true
		} else if entry := p.Pool.Get(acct.ID); entry == nil || !entry.Ready {
			waiting = true
		} else if url, ok := p.Manager.URL(acct.ID); !ok {
			waiting = true
		} else {
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			result, err := p.Manager.Client(acct.ID).Checkin(cctx, url)
			cancel()
			if err != nil {
				log.Printf("[checkin] %s: %v", acct.ID, err)
				p.noteCheckin(acct.ID, err.Error())
				// 出错也可能是暂时性的（网络 / worker 抖动），保持短周期重试。
				waiting = true
			} else {
				msg := summariseCheckin(result)
				log.Printf("[checkin] %s: %s", acct.ID, msg)
				p.noteCheckin(acct.ID, msg)
			}
		}
		if waiting {
			pending = true
		}
	}
	return pending
}

// summariseCheckin 把 worker 透传的原始签到返回，压成一句可读文案。
//
// worker 的返回形如 {"ok":true,"status":"success","message":"签到成功 +100 积分"}
// （见 worker/src/checkin.mjs 的 status 四种取值）。这里优先用上游给的 message，
// 缺失时按 status 兜底，避免账号表出现空白。
func summariseCheckin(raw json.RawMessage) string {
	var r struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &r); err == nil {
		if strings.TrimSpace(r.Message) != "" {
			return r.Message
		}
		switch r.Status {
		case "success":
			return "签到完成"
		case "already":
			return "今日已签到"
		case "skipped":
			return "签到活动未开放"
		}
	}
	// 解不出来时退回原文（截断，别把整段 JSON 塞进账号表）。
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "签到已尝试"
	}
	if len(text) > 120 {
		text = text[:120] + "…"
	}
	return text
}

// noteCheckin 把签到结果记到账号上，账号表直接展示、不用再跑一次请求。
func (p *Panel) noteCheckin(id, message string) {
	if strings.TrimSpace(message) == "" {
		message = "签到已尝试"
	}
	_, _ = p.Store.Update(id, func(a *accounts.Account) {
		a.LastCheckinAt = time.Now().Format(time.RFC3339)
		a.LastCheckinMsg = message
	})
}

func (p *Panel) cachedQuota(id string) (*worker.Quota, string) {
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	return p.quotaCache[id], p.quotaAt[id].Format("15:04:05")
}

// Logs returns the io.Writer the app mirrors its log output into.
func (p *Panel) Logs() io.Writer {
	if p.ring == nil {
		p.ring = NewLogRing()
	}
	return p.ring
}

// ---- log ring ----

const ringCapacity = 800

// LogEntry is one classified log line served to the panel.
type LogEntry struct {
	ID   int    `json:"id"`
	Ch   string `json:"ch"` // chat | worker | sys
	Text string `json:"text"`
	TS   string `json:"ts"`
}

type LogRing struct {
	mu     sync.Mutex
	lines  []LogEntry
	nextID int
}

func NewLogRing() *LogRing { return &LogRing{lines: make([]LogEntry, 0, ringCapacity)} }

// classify maps a log line to a panel channel.
func classifyLine(line string) string {
	switch {
	case strings.HasPrefix(line, "[chat]"):
		return "chat"
	case strings.HasPrefix(line, "[worker]"), strings.HasPrefix(line, "[warm]"):
		return "sys"
	case strings.HasPrefix(line, "[qod_"):
		return "worker"
	default:
		return "sys"
	}
}

func (r *LogRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(p) > 0 {
		idx := -1
		for i, b := range p {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			if len(p) > 0 {
				r.appendLocked(string(p))
			}
			break
		}
		line := strings.TrimRight(string(p[:idx]), "\r")
		r.appendLocked(line)
		p = p[idx+1:]
	}
	return len(p), nil
}

func (r *LogRing) appendLocked(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	r.nextID++
	r.lines = append(r.lines, LogEntry{
		ID:   r.nextID,
		Ch:   classifyLine(line),
		Text: line,
		TS:   time.Now().Format("15:04:05"),
	})
	if len(r.lines) > ringCapacity {
		r.lines = r.lines[len(r.lines)-ringCapacity:]
	}
}

func (r *LogRing) Snapshot(after int) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []LogEntry{}
	for _, line := range r.lines {
		if line.ID > after {
			out = append(out, line)
		}
	}
	return out
}

// ---- HTTP ----

func (p *Panel) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/panel/{$}", p.serveIndex)
	mux.HandleFunc("/panel/app.js", p.serveApp)
	mux.HandleFunc("/panel/api/", p.route)
	return mux
}

func (p *Panel) route(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/panel/api/")
	parts := strings.Split(rest, "/")
	switch {
	case rest == "overview" && r.Method == http.MethodGet:
		p.handleOverview(w, r)
	case rest == "logs" && r.Method == http.MethodGet:
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		entries := []LogEntry{}
		if p.ring != nil {
			entries = p.ring.Snapshot(after)
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
	case rest == "stats" && r.Method == http.MethodGet:
		if p.Stats == nil {
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
			return
		}
		report := p.Stats.Report(r.URL.Query().Get("range"))
		report["enabled"] = true
		writeJSON(w, http.StatusOK, report)
	case rest == "models" && r.Method == http.MethodGet:
		p.handleModels(w, r)
	case rest == "config" && r.Method == http.MethodGet:
		visible := p.Cfg
		if visible.APIKey != "" {
			visible.APIKey = visible.APIKey[:8] + "…"
		}
		writeJSON(w, http.StatusOK, visible)
	case rest == "config" && r.Method == http.MethodPost:
		p.handleConfigSave(w, r)
	case rest == "config/api-key" && r.Method == http.MethodGet:
		// Returns the full key for the config page's copy button; the
		// config listing above stays masked so screenshots are safe.
		writeJSON(w, http.StatusOK, map[string]string{"api_key": p.Cfg.APIKey})
	case len(parts) == 1 && r.Method == http.MethodPost:
		p.handleAccountAction(w, r, parts[0], "")
	case len(parts) == 4 && parts[0] == "accounts" && r.Method == http.MethodPost:
		// Sub-actions such as login/start and login/poll.
		p.handleAccountAction(w, r, parts[1], parts[2]+"/"+parts[3])
	case len(parts) == 3 && parts[0] == "accounts" && r.Method == http.MethodPost:
		p.handleAccountAction(w, r, parts[1], parts[2])
	case len(parts) == 3 && parts[0] == "accounts" && r.Method == http.MethodGet:
		p.handleAccountGet(w, r, parts[1], parts[2])
	case len(parts) == 2 && r.Method == http.MethodGet:
		p.handleAccountGet(w, r, parts[0], parts[1])
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

// ---- overview ----

func (p *Panel) handleOverview(w http.ResponseWriter, r *http.Request) {
	healthy, total := p.Pool.Counts()
	type accountRow struct {
		accounts.Account
		Ready      bool    `json:"ready"`
		UID        string  `json:"uid"`
		InFlight   int64   `json:"in_flight"`
		CoolKind   string  `json:"cool_kind"`
		CoolUntil  string  `json:"cool_until"`
		Success    int64   `json:"success_count"`
		Errors     int64   `json:"err_total"`
		LastErr    string  `json:"last_err"`
		LastErrKnd string  `json:"last_err_kind"`
		Running    bool    `json:"running"`
		Credits    float64 `json:"credits"`
		CredTotal  float64 `json:"credits_total"`
		QuotaAt    string  `json:"quota_at"`
		HasQuota   bool    `json:"has_quota"`
	}
	rows := []accountRow{}
	for _, acct := range p.Store.List() {
		row := accountRow{Account: acct, CoolKind: string(pool.KindNone)}
		if entry := p.Pool.Get(acct.ID); entry != nil {
			row.Ready = entry.Ready
			row.UID = entry.UID
			row.InFlight = entry.InFlight
			row.CoolKind = string(entry.CoolKind)
			if !entry.Until.IsZero() && time.Now().Before(entry.Until) {
				row.CoolUntil = entry.Until.Format(time.RFC3339)
			}
			row.Success = entry.SuccessCount
			row.Errors = entry.ErrTotal
			row.LastErr = entry.LastErr
			row.LastErrKnd = string(entry.LastErrKind)
		}
		row.Running = p.Manager.Running(acct.ID)
		if quota, at := p.cachedQuota(acct.ID); quota != nil {
			if totalQ, remainingQ, ok := quota.Remaining(); ok {
				row.Credits = remainingQ
				row.CredTotal = totalQ
				row.QuotaAt = at
				row.HasQuota = true
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":      Version,
		"uptime_s":     int(time.Since(p.Start).Seconds()),
		"healthy":      healthy,
		"total":        total,
		"sticky_count": p.Pool.StickyCount(),
		"accounts":     rows,
	})
}

// ---- account ops ----

func (p *Panel) handleAccountAction(w http.ResponseWriter, r *http.Request, id, action string) {
	acct, ok := p.Store.Get(id)
	if !ok && action != "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "account_not_found"})
		return
	}
	switch {
	case action == "": // create account
		var body struct {
			Name        string `json:"name"`
			Region      string `json:"region"`
			Priority    int    `json:"priority"`
			MaxInFlight int    `json:"max_inflight"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
			return
		}
		created, err := p.Store.Create(body.Name, body.Region, body.Priority, body.MaxInFlight)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		p.Pool.Sync(p.syncItems())
		p.syncAccount(created)
		writeJSON(w, http.StatusOK, created)
	case action == "delete":
		p.Manager.Stop(id)
		_ = p.Store.Delete(id)
		p.Pool.Sync(p.syncItems())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case action == "enable" || action == "disable":
		enabled := action == "enable"
		updated, err := p.Store.Update(id, func(a *accounts.Account) { a.Enabled = enabled })
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if !enabled {
			p.Manager.Stop(id)
		}
		p.Pool.Sync(p.syncItems())
		if enabled {
			p.syncAccount(updated)
		}
		writeJSON(w, http.StatusOK, updated)
	case action == "login/start":
		p.managerAction(w, acct, func(ctx context.Context, url string) (any, error) {
			return p.Manager.Client(acct.ID).LoginDevice(ctx, url)
		})
	case action == "login/poll":
		p.handleLoginPoll(w, acct)
	case action == "rewarm":
		p.managerAction(w, acct, func(ctx context.Context, url string) (any, error) {
			if err := p.Manager.Client(acct.ID).Rewarm(ctx, url); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		})
	case action == "checkin":
		p.managerAction(w, acct, func(ctx context.Context, url string) (any, error) {
			return p.Manager.Client(acct.ID).Checkin(ctx, url)
		})
	case action == "autocheckin/on" || action == "autocheckin/off":
		// 账号级「自动签到」开关（照 trae-free 的同名动作）。
		enabled := action == "autocheckin/on"
		updated, err := p.Store.Update(id, func(a *accounts.Account) { a.AutoCheckin = enabled })
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		// 刚打开开关就立刻生效，不必等调度器睡到下一个签到点。
		if enabled {
			p.wakeCheckinLoop()
		}
		writeJSON(w, http.StatusOK, updated)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown_action"})
	}
}

func (p *Panel) handleAccountGet(w http.ResponseWriter, r *http.Request, id, resource string) {
	acct, ok := p.Store.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "account_not_found"})
		return
	}
	switch resource {
	case "quota":
		refresh := r.URL.Query().Get("refresh") == "1"
		p.managerAction(w, acct, func(ctx context.Context, url string) (any, error) {
			quota, err := p.Manager.Client(acct.ID).Quota(ctx, url, refresh)
			if err == nil && quota != nil {
				p.quotaMu.Lock()
				p.quotaCache[acct.ID] = quota
				p.quotaAt[acct.ID] = time.Now()
				p.quotaMu.Unlock()
			}
			return quota, err
		})
	case "login":
		p.handleLoginPoll(w, acct)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown_resource"})
	}
}

func (p *Panel) handleLoginPoll(w http.ResponseWriter, acct accounts.Account) {
	p.managerAction(w, acct, func(ctx context.Context, url string) (any, error) {
		state, err := p.Manager.Client(acct.ID).LoginStatus(ctx, url)
		if err != nil {
			return nil, err
		}
		if state.Status == "ok" && acct.AuthType != "oauth" {
			if _, err := p.Store.Update(acct.ID, func(a *accounts.Account) { a.AuthType = "oauth" }); err == nil {
				acct.AuthType = "oauth"
			}
		}
		return state, nil
	})
}

type managerFunc func(ctx context.Context, url string) (any, error)

func (p *Panel) managerAction(w http.ResponseWriter, acct accounts.Account, fn managerFunc) {
	if err := p.ensureRunning(acct); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	url, ok := p.Manager.URL(acct.ID)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "worker not running"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	result, err := fn(ctx, url)
	if err != nil {
		status := http.StatusBadGateway
		if werr, ok := err.(*worker.WorkerError); ok && werr.Status >= 400 && werr.Status < 600 {
			status = werr.Status
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ensureRunning starts the worker and waits until its HTTP server answers.
func (p *Panel) ensureRunning(acct accounts.Account) error {
	if p.Manager.Running(acct.ID) {
		return nil
	}
	maxInFlight := acct.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = p.Cfg.MaxInFlight
	}
	if err := p.Manager.Start(acct.ID, acct.Region, acct.Home, maxInFlight); err != nil {
		return err
	}
	_, err := p.Manager.WaitHealthy(context.Background(), acct.ID, false, 120*time.Second)
	return err
}

// syncAccount warms a freshly created/enabled account in the background.
func (p *Panel) syncAccount(acct accounts.Account) {
	go func() {
		if !acct.Enabled {
			return
		}
		maxInFlight := acct.MaxInFlight
		if maxInFlight <= 0 {
			maxInFlight = p.Cfg.MaxInFlight
		}
		if err := p.Manager.Start(acct.ID, acct.Region, acct.Home, maxInFlight); err != nil {
			return
		}
		if health, err := p.Manager.WaitHealthy(context.Background(), acct.ID, false, 120*time.Second); err == nil {
			p.Pool.SetReady(acct.ID, health.HasAuthManager, health.UID)
		}
	}()
}

func (p *Panel) syncItems() []pool.SyncItem {
	list := p.Store.List()
	items := make([]pool.SyncItem, 0, len(list))
	for _, acct := range list {
		maxInFlight := acct.MaxInFlight
		if maxInFlight <= 0 {
			maxInFlight = p.Cfg.MaxInFlight
		}
		items = append(items, pool.SyncItem{
			ID: acct.ID, Region: acct.Region, Enabled: acct.Enabled,
			MaxInFlight: maxInFlight, Priority: acct.Priority,
		})
	}
	return items
}

// ---- models ----

func (p *Panel) handleModels(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID          string `json:"id"`
		Region      string `json:"region"`
		AccountID   string `json:"account_id"`
		DisplayName string `json:"display_name"`
		Reasoning   bool   `json:"is_reasoning"`
		Free        bool   `json:"free"`
		PriceLabel  string `json:"price_label"`
	}
	rows := []row{}
	for _, acct := range p.Store.List() {
		if !acct.Enabled || !p.Manager.Running(acct.ID) {
			continue
		}
		url, ok := p.Manager.URL(acct.ID)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		entries, err := p.Manager.Client(acct.ID).Models(ctx, url, false)
		cancel()
		if err != nil {
			continue
		}
		for _, entry := range entries {
			rows = append(rows, row{
				ID: entry.ID, Region: acct.Region, AccountID: acct.ID,
				DisplayName: entry.DisplayName, Reasoning: entry.IsReasoning,
				Free:       entry.IsFree(),
				PriceLabel: entry.CreditsLabel(),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": rows})
}

// ---- config ----

func (p *Panel) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	updated := p.Cfg
	if v, ok := body["listen"].(string); ok && strings.TrimSpace(v) != "" {
		updated.Listen = strings.TrimSpace(v)
	}
	if v, ok := body["proxy_url"].(string); ok {
		updated.ProxyURL = strings.TrimSpace(v)
	}
	if v, ok := body["max_in_flight"].(float64); ok && v > 0 {
		updated.MaxInFlight = int(v)
	}
	if v, ok := body["max_retry_accounts"].(float64); ok && v > 0 {
		updated.MaxRetryAccounts = int(v)
	}
	if v, ok := body["cooldown_soft_seconds"].(float64); ok && v > 0 {
		updated.CooldownSoftSeconds = int(v)
	}
	if v, ok := body["session_sticky"].(bool); ok {
		updated.SessionSticky = v
	}
	if v, ok := body["stats_enabled"].(bool); ok {
		updated.StatsEnabled = v
	}
	if v, ok := body["stats_keep_days"].(float64); ok && v > 0 {
		updated.StatsKeepDays = int(v)
	}
	// 每日自动签到时间（本地小时）。**0 是合法值**（= 过零点就允许自动签到），
	// 所以只在 0~23 区间内才接受 —— 不能用 `v > 0`，否则用户显式设的 0
	// 会被当成无效值丢掉、又被兜底逻辑顶回 10。
	if v, ok := body["checkin_hour_local"].(float64); ok && v >= 0 && v <= 23 {
		updated.CheckinHourLocal = int(v)
	}
	// 上下文窗口：0 是**合法值**（= 不注入，走上游目录默认的 20 万），
	// 所以不能用 `v > 0` 过滤，否则这个开关永远关不回去。
	if v, ok := body["context_window"].(float64); ok && v >= 0 {
		updated.ContextWindow = int(v)
	}
	// api_key / listen are server-managed: shown read-only, edits go through config.json + restart.
	if err := config.Save(p.CfgPath, updated); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p.Cfg = updated
	// 签到时间可能变了，叫醒调度器按新时间重算。
	p.wakeCheckinLoop()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": updated})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
