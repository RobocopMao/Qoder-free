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
