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
	return &Panel{Cfg: cfg, Store: st}
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
