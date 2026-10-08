package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seriesOf 取出 Report 里的 series（[]map[string]any），断言类型并返回。
func seriesOf(t *testing.T, rep map[string]any) []map[string]any {
	t.Helper()
	raw, ok := rep["series"]
	if !ok {
		t.Fatalf("Report 缺少 series 字段")
	}
	series, ok := raw.([]map[string]any)
	if !ok {
		t.Fatalf("series 类型不是 []map[string]any，实际 %T", raw)
	}
	return series
}

func pointsRequests(t *testing.T, p map[string]any) int64 {
	t.Helper()
	v, ok := p["requests"].(int64)
	if !ok {
		t.Fatalf("point.requests 不是 int64，实际 %T", p["requests"])
	}
	return v
}

// 今日档必须是**逐小时**，且铺到当前小时为止（未来小时不铺，否则曲线右端栽回基线）。
func TestReportTodayIsHourly(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Add(Record{Model: "qwen3.8-flash", AccountID: "acct", Requests: 1, Prompt: 10, Completion: 5})

	now := time.Now()
	series := seriesOf(t, r.Report("today"))
	if want := now.Hour() + 1; len(series) != want {
		t.Fatalf("今日点数 = %d，期望 %d（00:00 铺到当前小时 %02d:00）", len(series), want, now.Hour())
	}

	last, ok := series[len(series)-1]["day"].(string)
	if !ok {
		t.Fatalf("point.day 不是 string")
	}
	if want := now.Format("2006-01-02T15"); last != want {
		t.Fatalf("最后一个点 = %q，期望当前小时 %q", last, want)
	}
	if got := pointsRequests(t, series[len(series)-1]); got != 1 {
		t.Fatalf("当前小时 requests = %d，期望 1（Add 的记录应落在当前小时桶）", got)
	}
}

// 横轴必须连续：没有请求的小时也要出点、值为 0，否则曲线会断开。
func TestReportTodayHoursAreContiguous(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	series := seriesOf(t, r.Report("today"))
	if len(series) == 0 {
		t.Fatalf("今日序列为空，至少应有当前小时一个点")
	}
	var prev time.Time
	for i, p := range series {
		key := p["day"].(string)
		at, err := time.ParseInLocation("2006-01-02T15", key, time.Local)
		if err != nil {
			t.Fatalf("第 %d 个点的时间键 %q 无法解析: %v", i, key, err)
		}
		if i == 0 {
			if at.Hour() != 0 || at.Minute() != 0 {
				t.Fatalf("第一个点应为 00:00，实际 %s", key)
			}
		} else if at.Sub(prev) != time.Hour {
			t.Fatalf("第 %d 个点 %s 与上一个 %s 间隔不是 1 小时", i, key, prev.Format("2006-01-02T15"))
		}
		prev = at
	}
}

// 7 天 / 30 天档仍然按天，只把今日切成小时 —— 别把长档也拆成小时（点数会爆炸）。
func TestReportLongRangesStayDaily(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Add(Record{Model: "m", AccountID: "a", Requests: 1})
	for _, rangeName := range []string{"7d", "30d", "all"} {
		series := seriesOf(t, r.Report(rangeName))
		if len(series) == 0 {
			t.Fatalf("%s 档序列为空", rangeName)
		}
		for i, p := range series {
			key := p["day"].(string)
			if len(key) != len("2006-01-02") {
				t.Fatalf("%s 档第 %d 个点 %q 不是按天（应为 2006-01-02 形状）", rangeName, i, key)
			}
		}
	}
}

// 用户 2026-10-03（m07135）：7 天档必须**铺满 7 格**、30 天档铺满 30 格，
// 且横轴连续、右端是今天 —— 只跑过一天时以前只返回 1 个点，柱子被压成一根。
func TestLongRangesPadToCalendarDays(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// 只在今天写一条记录，前几天全是空桶 —— 序列仍应补齐。
	r.Add(Record{Model: "m", AccountID: "a", Requests: 1})

	today := time.Now().Format("2006-01-02")
	for _, tc := range []struct {
		rangeName string
		want      int
	}{{"7d", 7}, {"30d", 30}} {
		series := seriesOf(t, r.Report(tc.rangeName))
		if len(series) != tc.want {
			t.Fatalf("%s 档点数 = %d，期望 %d（按自然日铺满）", tc.rangeName, len(series), tc.want)
		}
		if last := series[len(series)-1]["day"].(string); last != today {
			t.Fatalf("%s 档右端 = %q，期望今天 %q", tc.rangeName, last, today)
		}
		// 横轴连续：相邻点间隔恰好一天，且第一个点 == 今天往前数 want-1 天。
		var prev time.Time
		for i, p := range series {
			at, err := time.ParseInLocation("2006-01-02", p["day"].(string), time.Local)
			if err != nil {
				t.Fatalf("%s 档第 %d 个点 %q 无法解析: %v", tc.rangeName, i, p["day"], err)
			}
			if i > 0 && at.Sub(prev) != 24*time.Hour {
				t.Fatalf("%s 档第 %d 个点 %q 与上一个间隔不是一天", tc.rangeName, i, p["day"])
			}
			prev = at
		}
		first, _ := time.ParseInLocation("2006-01-02", series[0]["day"].(string), time.Local)
		wantFirst, _ := time.ParseInLocation("2006-01-02", today, time.Local)
		wantFirst = wantFirst.AddDate(0, 0, -(tc.want - 1))
		if !first.Equal(wantFirst) {
			t.Fatalf("%s 档左端 = %q，期望 %q", tc.rangeName, series[0]["day"], wantFirst.Format("2006-01-02"))
		}
		// 补出来的空格子值为 0，今日那条记录落在右端。
		if got := pointsRequests(t, series[len(series)-1]); got != 1 {
			t.Fatalf("%s 档今天 requests = %d，期望 1", tc.rangeName, got)
		}
		if got := pointsRequests(t, series[0]); got != 0 {
			t.Fatalf("%s 档左端空日 requests = %d，期望 0", tc.rangeName, got)
		}
	}
}

// 补出来的空日子不能污染 totals（nil 桶在 mergeBucket 里必须被跳过）。
func TestPaddingDoesNotPolluteTotals(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Add(Record{Model: "m", AccountID: "a", Requests: 3, Failures: 1, Prompt: 100, Completion: 50})
	rep := r.Report("30d")
	totals, ok := rep["totals"].(bucket)
	if !ok {
		t.Fatalf("totals 类型不是 bucket，实际 %T", rep["totals"])
	}
	if totals.Requests != 3 || totals.Failures != 1 || totals.Prompt != 100 || totals.Completion != 50 {
		t.Fatalf("totals 被补出的空日子污染：%+v", totals)
	}
}

// 小时桶独立按 keepHours 修剪：它只服务今日档，留多了纯占体积。
func TestHoursArePruned(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now()
	for i := 0; i < keepHours+5; i++ {
		key := now.Add(-time.Duration(i+2) * time.Hour).Format("2006-01-02T15")
		r.data.Hours[key] = &bucket{ByModel: map[string]*cell{}, ByAccount: map[string]*cell{}}
	}
	r.Add(Record{Model: "m", AccountID: "a", Requests: 1})
	if len(r.data.Hours) > keepHours {
		t.Fatalf("小时桶数量 = %d，期望修剪到 ≤ %d", len(r.data.Hours), keepHours)
	}
	if _, ok := r.data.Hours[now.Format("2006-01-02T15")]; !ok {
		t.Fatalf("刚刚 Add 的当前小时桶被误删")
	}
}

// 旧 stats.json（没有 hours 字段）必须能平滑打开，不能因为新增字段就报错或丢数据。
func TestOpenLegacyFileWithoutHours(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	legacy := `{"days":{"2026-10-01":{"requests":7,"failures":1,"prompt_tokens":100,"completion_tokens":50,
		"by_model":{},"by_account":{}}},"grand":{"requests":7,"failures":1,"prompt_tokens":100,"completion_tokens":50,
		"by_model":null,"by_account":null},"saved_at":"2026-10-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("写入旧文件: %v", err)
	}
	r, err := Open(path, 30)
	if err != nil {
		t.Fatalf("Open 旧文件: %v", err)
	}
	if r.data.Hours == nil {
		t.Fatalf("旧文件打开后 Hours 仍为 nil，后续写入会 panic")
	}
	if got := r.data.Grand.Requests; got != 7 {
		t.Fatalf("旧数据丢失：grand.requests = %d，期望 7", got)
	}
	// 旧文件没有今日的小时桶，但今日档仍要给出连续序列（全 0），而不是空数组。
	series := seriesOf(t, r.Report("today"))
	if len(series) != time.Now().Hour()+1 {
		t.Fatalf("旧文件下今日点数 = %d，期望 %d", len(series), time.Now().Hour()+1)
	}
	if got := pointsRequests(t, series[len(series)-1]); got != 0 {
		t.Fatalf("旧文件下当前小时 requests = %d，期望 0", got)
	}
}

// 序列点必须能被 json.Marshal（写入 /panel/api/stats 前会整体序列化）。
func TestReportTodayIsJSONEncodable(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Add(Record{Model: "m", AccountID: "a", Requests: 2, Prompt: 1, Completion: 1})
	raw, err := json.Marshal(r.Report("today"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back struct {
		Series []struct {
			Day      string `json:"day"`
			Requests int64  `json:"requests"`
		} `json:"series"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Series) == 0 {
		t.Fatalf("序列化后再解析，series 为空")
	}
}

// creditOf 取出序列点里的 credits，缺失时返回 (0,false)。
func creditOf(t *testing.T, p map[string]any) (float64, bool) {
	t.Helper()
	raw, ok := p["credits"]
	if !ok {
		return 0, false
	}
	v, ok := raw.(float64)
	if !ok {
		t.Fatalf("point.credits 不是 float64，实际 %T", raw)
	}
	return v, true
}

// 余额是**覆盖写**：同一小时采三次只留最后一次，绝不能累加成三倍。
func TestAddCreditsOverwritesNotAccumulates(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Add(Record{Model: "m", AccountID: "a", Requests: 1})
	r.AddCredits("a", 500, 1000)
	r.AddCredits("a", 600, 1000)
	r.AddCredits("a", 550, 1000)

	series := seriesOf(t, r.Report("today"))
	got, ok := creditOf(t, series[len(series)-1])
	if !ok {
		t.Fatalf("当前小时点缺少 credits 字段")
	}
	if got != 550 {
		t.Fatalf("credits = %v，期望 550（覆盖写取最后一次，累加会得到 1650）", got)
	}
}

// 多账号求和：两个账号各采一次，桶里应是两者之和。
func TestAddCreditsSumsAccounts(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Add(Record{Model: "m", AccountID: "a", Requests: 1})
	r.AddCredits("a", 100, 1000)
	r.AddCredits("b", 250, 1000)

	series := seriesOf(t, r.Report("today"))
	got, ok := creditOf(t, series[len(series)-1])
	if !ok {
		t.Fatalf("缺少 credits")
	}
	if got != 350 {
		t.Fatalf("credits = %v，期望 350（100+250）", got)
	}
}

// 某账号这一轮没采到（worker 没热），它的最后一次已知值必须**留在合计里**：
// 摘掉的话曲线会凭空掉一块，而实际上什么都没发生。
func TestAddCreditsKeepsLastKnownForSilentAccount(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Add(Record{Model: "m", AccountID: "a", Requests: 1})
	r.AddCredits("a", 100, 1000)
	r.AddCredits("b", 250, 1000)
	// 这一轮只有 a 采到，b 缺席。
	r.AddCredits("a", 90, 1000)

	series := seriesOf(t, r.Report("today"))
	got, ok := creditOf(t, series[len(series)-1])
	if !ok {
		t.Fatalf("缺少 credits")
	}
	if got != 340 {
		t.Fatalf("credits = %v，期望 340（90 + b 的最后已知 250）", got)
	}
}

// "没采到"必须与"余额为 0"可分辨：没有任何采样的时段不能长出 credits 键。
//
// 补 0 会被前端画成一条掉到地板的假线 —— 这正是用指针而不是 float64 的理由。
func TestCreditsAbsentBeforeFirstSample(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Add(Record{Model: "m", AccountID: "a", Requests: 1})

	series := seriesOf(t, r.Report("today"))
	if _, ok := creditOf(t, series[len(series)-1]); ok {
		t.Fatalf("未采样过余额，点里不该有 credits 键")
	}
}

// 余额真的归零时**必须**留下 credits: 0，不能被 omitempty 吞掉。
func TestCreditsZeroIsDistinctFromAbsent(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Add(Record{Model: "m", AccountID: "a", Requests: 1})
	r.AddCredits("a", 0, 1000)

	series := seriesOf(t, r.Report("today"))
	got, ok := creditOf(t, series[len(series)-1])
	if !ok {
		t.Fatalf("余额为 0 时也要有 credits 键（用指针就是为了这个）")
	}
	if got != 0 {
		t.Fatalf("credits = %v，期望 0", got)
	}

	// 落盘后再读回来，指针语义必须保持。
	raw, err := json.Marshal(r.Report("today"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back struct {
		Series []struct {
			Credits *float64 `json:"credits"`
		} `json:"series"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	last := back.Series[len(back.Series)-1]
	if last.Credits == nil {
		t.Fatalf("序列化/反序列化后 credits 变成了 nil（0 被 omitempty 吞了）")
	}
	if *last.Credits != 0 {
		t.Fatalf("反序列化后 credits = %v，期望 0", *last.Credits)
	}
}

// 只有余额采样、当天没有任何请求的日子，也必须能画出点来。
//
// 周末 / 闲置的服务就是这个形态，而那正是最需要看清余额有没有被消耗的时候。
func TestCreditsAreRecordedWithoutAnyRequest(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.AddCredits("a", 700, 1000)

	series := seriesOf(t, r.Report("today"))
	got, ok := creditOf(t, series[len(series)-1])
	if !ok {
		t.Fatalf("没有任何请求时也要有 credits（桶应由余额采样创建）")
	}
	if got != 700 {
		t.Fatalf("credits = %v，期望 700", got)
	}
	// 流量字段不能因为这次采样被污染。
	if reqs := pointsRequests(t, series[len(series)-1]); reqs != 0 {
		t.Fatalf("requests = %d，期望 0（余额采样不该增加请求数）", reqs)
	}
}

// 过期账号（超过 creditSampleTTL 没再采到）要从合计里剔除，且被真正删掉。
func TestExpiredCreditAccountIsDropped(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "stats.json"), 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.AddCredits("gone", 900, 1000)
	r.AddCredits("live", 100, 1000)

	// 手动把 gone 的时间戳拨到 TTL 之前，再采一次 live 触发求和。
	r.mu.Lock()
	c := r.data.CreditsAccounts["gone"]
	c.AtUnix = time.Now().Add(-creditSampleTTL - time.Hour).Unix()
	r.data.CreditsAccounts["gone"] = c
	r.mu.Unlock()

	r.AddCredits("live", 90, 1000)

	r.mu.Lock()
	_, stillThere := r.data.CreditsAccounts["gone"]
	sum, _ := r.creditSumLocked(time.Now())
	r.mu.Unlock()
	if stillThere {
		t.Fatalf("过期账号没有被删掉，stats.json 会无界增长")
	}
	if sum != 90 {
		t.Fatalf("合计 = %v，期望 90（只剩 live）", sum)
	}
}

// 余额字段必须真的落盘、并能从盘上读回来（含 AtUnix）。
func TestCreditsSurviveReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	r, err := Open(path, 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.Add(Record{Model: "m", AccountID: "a", Requests: 1})
	r.AddCredits("a", 420, 1000)

	re, err := Open(path, 30)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	series := seriesOf(t, re.Report("today"))
	got, ok := creditOf(t, series[len(series)-1])
	if !ok {
		t.Fatalf("重新打开后 credits 丢了")
	}
	if got != 420 {
		t.Fatalf("重新打开后 credits = %v，期望 420", got)
	}
}

// 老文件（本次改动之前落盘的、没有 credits_accounts 键）必须能正常读。
func TestOpenLegacyFileWithoutCredits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	old := `{"days":{"2026-10-01":{"requests":3,"failures":1,"prompt_tokens":10,"completion_tokens":5}},` +
		`"hours":{},"grand":{"requests":3},"saved_at":"2026-10-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := Open(path, 30)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if r.data.CreditsAccounts == nil {
		t.Fatalf("CreditsAccounts 未初始化，AddCredits 会 panic")
	}
	// 老桶没有 credits，序列化时不能凭空长出 0。
	series := seriesOf(t, r.Report("30d"))
	for _, p := range series {
		if _, ok := creditOf(t, p); ok {
			t.Fatalf("老桶 %v 不该有 credits 键", p["day"])
		}
	}
	// 采一次之后只影响当前桶。
	r.AddCredits("a", 5, 10)
	raw, err := json.Marshal(r.data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	days, ok := back["days"].(map[string]any)
	if !ok {
		t.Fatalf("days 不是对象")
	}
	legacy, ok := days["2026-10-01"].(map[string]any)
	if !ok {
		t.Fatalf("老桶丢了")
	}
	if _, exists := legacy["credits"]; exists {
		t.Fatalf("老桶被写上了 credits，历史数据不该被污染")
	}
}
