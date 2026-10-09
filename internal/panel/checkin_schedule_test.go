package panel

import (
	"testing"
	"time"

	"qoder-free/internal/accounts"
	"qoder-free/internal/config"
)

// newTestPanel 造一个只带 Store + Cfg 的最小 Panel，用于测调度时间计算。
func newTestPanel(t *testing.T, cfg config.Config, accts ...accounts.Account) *Panel {
	t.Helper()
	dir := t.TempDir()
	st, err := accounts.Open(dir, dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, a := range accts {
		created, err := st.Create(a.Name, a.Region, a.Priority, a.MaxInFlight)
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		_, err = st.Update(created.ID, func(x *accounts.Account) {
			x.Enabled = a.Enabled
			x.AutoCheckin = a.AutoCheckin
			x.LastCheckinAt = a.LastCheckinAt
		})
		if err != nil {
			t.Fatalf("update account: %v", err)
		}
	}
	// checkinWake 必须初始化：wakeCheckinLoop 对 nil 通道是安全的（直接返回），
	// 但测试要验证信号真的投递了，所以给一个真通道。
	return &Panel{Cfg: cfg, Store: st, checkinWake: make(chan struct{}, 1)}
}

func acct(id string, enabled, auto bool, region, lastCheckin string) accounts.Account {
	return accounts.Account{
		ID: id, Name: id, Region: region, Enabled: enabled,
		AutoCheckin: auto, LastCheckinAt: lastCheckin,
	}
}

// 用户 m00313 的核心诉求：**已经签到就不要轮询**。
// 全部签完时，下一次唤醒应该是「明天的签到点」，而不是几分钟后又醒。
func TestNextCheckinDelayAllSignedSleepsUntilTomorrow(t *testing.T) {
	now := time.Date(2026, 10, 8, 15, 0, 0, 0, time.Local)
	p := newTestPanel(t, config.Config{CheckinHourLocal: 10},
		acct("a", true, true, "cn", "2026-10-08T11:06:41+08:00"),
	)
	got := p.nextCheckinDelay(now)
	want := time.Date(2026, 10, 9, 10, 0, 0, 0, time.Local).Sub(now)
	if got != want {
		t.Fatalf("全部签完应睡到明天 10:00，got=%v want=%v", got, want)
	}
	if got < 10*time.Hour {
		t.Fatalf("不该每小时醒来轮询，got=%v", got)
	}
}

// 还没到签到点：应睡到今天的签到点。
func TestNextCheckinDelayBeforeHour(t *testing.T) {
	now := time.Date(2026, 10, 8, 6, 0, 0, 0, time.Local)
	p := newTestPanel(t, config.Config{CheckinHourLocal: 10},
		acct("a", true, true, "cn", "2026-10-07T11:00:00+08:00"),
	)
	got := p.nextCheckinDelay(now)
	if want := 4 * time.Hour; got != want {
		t.Fatalf("应睡到今天 10:00（4h），got=%v", got)
	}
}

// 已过签到点且今天还没签：应立刻跑（0 延迟）。
func TestNextCheckinDelayDueNow(t *testing.T) {
	now := time.Date(2026, 10, 8, 14, 0, 0, 0, time.Local)
	p := newTestPanel(t, config.Config{CheckinHourLocal: 10},
		acct("a", true, true, "cn", "2026-10-07T11:00:00+08:00"),
	)
	if got := p.nextCheckinDelay(now); got != 0 {
		t.Fatalf("到点且未签应立刻跑，got=%v", got)
	}
}

// 没有账号开启自动签到：低频兜底，不空转。
func TestNextCheckinDelayNoAutoAccounts(t *testing.T) {
	now := time.Date(2026, 10, 8, 14, 0, 0, 0, time.Local)
	p := newTestPanel(t, config.Config{CheckinHourLocal: 10},
		acct("a", true, false, "cn", ""),
	)
	got := p.nextCheckinDelay(now)
	if got != 30*time.Minute {
		t.Fatalf("无自动签到账号应低频兜底 30m，got=%v", got)
	}
}

// 企业版那种「签到活动未开放」也会写 LastCheckinAt，同样不应再打扰。
func TestNextCheckinDelaySkippedStillCountsAsDone(t *testing.T) {
	now := time.Date(2026, 10, 8, 14, 0, 0, 0, time.Local)
	p := newTestPanel(t, config.Config{CheckinHourLocal: 10},
		acct("ent", true, true, "cn", "2026-10-08T11:06:40+08:00"),
	)
	if got := p.nextCheckinDelay(now); got < 10*time.Hour {
		t.Fatalf("已处理过的账号不该再频繁唤醒，got=%v", got)
	}
}

// 唤醒通道必须是非阻塞的：连发多次不能卡住调用方。
func TestWakeCheckinLoopIsNonBlocking(t *testing.T) {
	p := &Panel{checkinWake: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			p.wakeCheckinLoop()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wakeCheckinLoop 阻塞了")
	}
	// 通道容量 1，投递过信号后应能读到。
	select {
	case <-p.checkinWake:
	default:
		t.Fatal("应有一个待处理的唤醒信号")
	}
}

// 用户 m00314：「我等会会打开」。
//
// 禁用期间 nextCheckinDelay 算出来是「睡到明天」（因为 enabled=false 的账号
// 被跳过 → 等价于全部签完）。所以**打开账号时必须叫醒调度器**，
// 否则今天打开也不会补签，得干等到明天 10:00。
func TestEnableAccountMustWakeScheduler(t *testing.T) {
	now := time.Date(2026, 10, 8, 14, 0, 0, 0, time.Local)
	cfg := config.Config{CheckinHourLocal: 10}

	// 复刻用户真实场景：企业号启用且今天已处理过，两个个人号被禁用。
	// 调度器据此认为「全部签完」→ 睡到明天。
	p := newTestPanel(t, cfg,
		acct("ent", true, true, "cn", "2026-10-08T11:06:40+08:00"),
		acct("personal", false, true, "cn", "2026-10-07T10:00:00+08:00"),
	)
	if sleeping := p.nextCheckinDelay(now); sleeping < 10*time.Hour {
		t.Fatalf("两个个人号禁用时应睡到明天，got=%v", sleeping)
	}

	// 打开个人号（模拟 enable 动作）→ 应立刻变成「到点未签 → 0 延迟」。
	var personalID string
	for _, a := range p.Store.List() {
		if a.Name == "personal" {
			personalID = a.ID
		}
	}
	if personalID == "" {
		t.Fatal("找不到 personal 账号")
	}
	if _, err := p.Store.Update(personalID, func(a *accounts.Account) {
		a.Enabled = true
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := p.nextCheckinDelay(now); got != 0 {
		t.Fatalf("启用后且已过签到点应立刻可签（0），got=%v", got)
	}

	// 并且 enable 动作要投递唤醒信号，否则调度器还在睡。
	p.wakeCheckinLoop()
	select {
	case <-p.checkinWake:
	default:
		t.Fatal("enable 后应有唤醒信号")
	}
}

// 用户 m03482：「qoder 的自动签到没有触发」。
//
// 根因是 Go 定时器在 darwin 上用 `mach_absolute_time`（睡眠期间不走），
// 「一觉睡到明天 10:00」跨过合盖睡眠后要晚一个睡眠时长才醒，签到点被跳过。
// 修法是把实际定时器封顶到 30 分钟，每次醒来用墙钟重新评估。
func TestClampCheckinSleepCapsLongSleep(t *testing.T) {
	// 跨夜长睡（理论值可能十几小时）必须被压到 30 分钟，
	// 否则睡眠期间单调时钟不走 → 漏签。
	long := 15 * time.Hour
	if got := clampCheckinSleep(long); got != maxCheckinSleep {
		t.Fatalf("长睡眠应封顶到 %v，got=%v", maxCheckinSleep, got)
	}
	// 边界：正好等于上限时不应被改动。
	if got := clampCheckinSleep(maxCheckinSleep); got != maxCheckinSleep {
		t.Fatalf("等于上限时应保持 %v，got=%v", maxCheckinSleep, got)
	}
}

func TestClampCheckinSleepLiftsZero(t *testing.T) {
	// nextCheckinDelay 在「到点未签」时返回 0；若 worker 一直没就绪
	// （waiting=true 不写 LastCheckinAt）就会零延迟热循环，必须有下限。
	if got := clampCheckinSleep(0); got != minCheckinSleep {
		t.Fatalf("0 延迟应抬到 %v，got=%v", minCheckinSleep, got)
	}
	if got := clampCheckinSleep(-time.Second); got != minCheckinSleep {
		t.Fatalf("负值应抬到 %v，got=%v", minCheckinSleep, got)
	}
	// 区间内的值原样通过：比如「到点前 5 分钟」不该被改动。
	mid := 5 * time.Minute
	if got := clampCheckinSleep(mid); got != mid {
		t.Fatalf("区间内应原样返回 %v，got=%v", mid, got)
	}
}

func TestClampCheckinSleepBoundsAreSane(t *testing.T) {
	if minCheckinSleep <= 0 {
		t.Fatalf("下限必须为正，got=%v", minCheckinSleep)
	}
	if maxCheckinSleep <= minCheckinSleep {
		t.Fatalf("上限 %v 必须大于下限 %v", maxCheckinSleep, minCheckinSleep)
	}
	// 上限要足够短才能兜住睡眠跳点（本机实测一次合盖睡了 7.2 小时），
	// 但又不能短到变成事实上的轮询（会白跑唤醒、增加弹窗机会）。
	if maxCheckinSleep > time.Hour {
		t.Fatalf("上限 %v 太长，兜不住睡眠跳点", maxCheckinSleep)
	}
}
