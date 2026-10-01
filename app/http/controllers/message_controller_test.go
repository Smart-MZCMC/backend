package controllers

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestParseTimeFilter_AcceptsDatetimeLocalWithoutSeconds 防的是「界面上选完
// 时间一查询就 400」这一类问题：浏览器的 <input type="datetime-local"> 默认
// 提交的是 `2026-05-20T09:30`，**没有秒**。少了这个 layout，管理后台的时间
// 筛选控件提交上来的值一律解析失败，而错误信息只会说「时间格式不正确」，
// 完全指不到真正的原因。
func TestParseTimeFilter_AcceptsDatetimeLocalWithoutSeconds(t *testing.T) {
	got, ok := parseTimeFilter("2026-05-20T09:30")
	if !ok {
		t.Fatal("datetime-local 的分钟精度写法必须能解析")
	}
	if got.Minute() != 30 {
		t.Fatalf("分钟数应保留: %v", got)
	}
	if got.Location() != time.UTC {
		t.Fatalf("结果时区应是 UTC，实际 %v", got.Location())
	}
}

// TestParseTimeFilter_ConvertsWallClockToUTC 是这次修复的核心断言。
//
// 必须在非 UTC 的时区里验证，因为开发机很可能正好在 UTC —— 那样墙上时间与
// UTC 时刻相同，换算做没做都测不出来。子进程里显式把 time.Local 指向
// Asia/Shanghai：「09:30」应当被理解成东八区的 09:30，即 UTC 的 01:30。
//
// 为什么用子进程而不是 t.Setenv("TZ", ...)：Windows 上 Go **不读** TZ 环境
// 变量（实测设了也没效果），而直接改 time.Local 这个全局变量会让并行测试
// 互相污染。
func TestParseTimeFilter_ConvertsWallClockToUTC(t *testing.T) {
	if os.Getenv("GO_TEST_TZ_CHILD") == "1" {
		loc, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			fmt.Fprintln(os.Stderr, "SKIP: 本机没有时区数据库")
			os.Exit(0)
		}
		time.Local = loc

		got, ok := parseTimeFilter("2026-05-20T09:30")
		if !ok {
			fmt.Fprintln(os.Stderr, "FAIL: 应能解析")
			os.Exit(1)
		}
		// 东八区 09:30 == UTC 01:30
		if formatted := got.UTC().Format("2006-01-02T15:04"); formatted != "2026-05-20T01:30" {
			fmt.Fprintf(os.Stderr, "FAIL: 得到 %s，期望 2026-05-20T01:30\n", formatted)
			os.Exit(1)
		}
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestParseTimeFilter_ConvertsWallClockToUTC")
	cmd.Env = append(os.Environ(), "GO_TEST_TZ_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程失败（期望墙上时间 09:30 被换算成 UTC 01:30）:\n%s", out)
	}
}

// TestParseTimeFilter_AcceptsOtherForms 保证其余几种写法没被顺手改坏。
func TestParseTimeFilter_AcceptsOtherForms(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Time
	}{
		// 墙上时间：按本地时区理解用户意图，再换算成 UTC。
		{"2026-05-20", time.Date(2026, 5, 20, 0, 0, 0, 0, time.Local).UTC()},
		{"2026-05-20 09:30:15", time.Date(2026, 5, 20, 9, 30, 15, 0, time.Local).UTC()},
		{"2026-05-20T09:30:15", time.Date(2026, 5, 20, 9, 30, 15, 0, time.Local).UTC()},
		// 带偏移的按字面时刻解析，不当成本地时间。
		{"2026-05-20T09:30:00+08:00", time.Date(2026, 5, 20, 1, 30, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, ok := parseTimeFilter(c.raw)
		if !ok {
			t.Fatalf("%q 应该能解析", c.raw)
		}
		if !got.Equal(c.want) {
			t.Fatalf("%q 解析成 %v，期望 %v", c.raw, got, c.want)
		}
	}
}

// TestParseTimeFilter_AlwaysReturnsUTC 锁住「返回值一律是 UTC」这条约定。
//
// 之前这里是 ParseInLocation(..., time.Local) 直接返回，于是条件值带着本地
// 时区，而 messages.created_at 存的是 UTC —— 东八区恒定差 8 小时。
// 实测：库里 30 条记录，用本地时间窗口查 total=0，用 UTC 窗口查 total=30。
// 接口照样返回 200、total=0，看起来像「这段时间没有日志」，非常难查。
func TestParseTimeFilter_AlwaysReturnsUTC(t *testing.T) {
	for _, raw := range []string{
		"2026-05-20T09:30",
		"2026-05-20 09:30:15",
		"2026-05-20T09:30:15",
		"2026-05-20",
		"2026-05-20T09:30:00+08:00",
		"2026-05-20T01:30:00Z",
	} {
		got, ok := parseTimeFilter(raw)
		if !ok {
			t.Fatalf("%q 应该能解析", raw)
		}
		if got.Location() != time.UTC {
			t.Errorf("%q 解析结果的时区应是 UTC，实际 %v", raw, got.Location())
		}
	}
}

// TestParseTimeFilter_EquivalentInstantsAgree 保证「带时区偏移」与「不带偏移」
// 描述的是同一时刻时，结果相同。这条是上面那个 bug 的根因：
// 用户界面给的是本地墙上时间，库里存的是 UTC，两者必须落到同一个时间轴上。
func TestParseTimeFilter_EquivalentInstantsAgree(t *testing.T) {
	withOffset, ok := parseTimeFilter("2026-05-20T09:30:00+08:00")
	if !ok {
		t.Fatal("带偏移的 RFC3339 应能解析")
	}
	// 同一个时刻写成 UTC：09:30+08:00 == 01:30Z
	inUTC, ok := parseTimeFilter("2026-05-20T01:30:00Z")
	if !ok {
		t.Fatal("UTC 的 RFC3339 应能解析")
	}
	if !withOffset.Equal(inUTC) {
		t.Fatalf("同一时刻的两种写法应解析成相同结果: %v vs %v", withOffset, inUTC)
	}
}

// TestParseTimeUpperBound_IncludesWholeMinute 防的是「刚刚做的操作在审计页
// 查不到」这一类问题。
//
// datetime-local 只能填到分钟，「2026-10-02T00:24」被解析成 00:24:00.000，
// 而管理后台把 to 默认成「现在」。于是 `created_at <= 00:24:00` 会把
// 00:24:35 写入的记录挡掉——最新那条必定不显示，看起来像根本没留痕。
//
// 判据用「同一分钟内的秒数」而不是具体时刻：只要 00:24:35 被包含即可。
func TestParseTimeUpperBound_IncludesWholeMinute(t *testing.T) {
	for _, raw := range []string{"2026-10-02T00:24", "2026-10-02 00:24"} {
		bound, ok := parseTimeUpperBound(raw)
		if !ok {
			t.Fatalf("%q 应该能解析", raw)
		}
		// 该分钟内的最后一点必须落在上界之内（留 1ms 容差给纳秒截断）。
		lateInMinute := time.Date(2026, 10, 2, 0, 24, 35, 0, time.Local).UTC()
		if lateInMinute.After(bound) {
			t.Errorf("%q 的上界 %v 把同分钟内的记录 %v 排除在外了",
				raw, bound, lateInMinute)
		}
	}
}

// TestParseTimeUpperBound_DateOnlyCoversWholeDay 保留原有的纯日期语义：
// 选到 2 月 1 日期望包含 2 月 1 日整天。
func TestParseTimeUpperBound_DateOnlyCoversWholeDay(t *testing.T) {
	bound, ok := parseTimeUpperBound("2026-05-20")
	if !ok {
		t.Fatal("纯日期应该能解析")
	}
	endOfDay := time.Date(2026, 5, 20, 23, 59, 59, 0, time.Local).UTC()
	if endOfDay.After(bound) {
		t.Fatalf("纯日期的上界 %v 应覆盖当天 23:59:59", bound)
	}
}

// TestParseTimeUpperBound_SecondsPrecisionLeftAlone 保证带秒的取值仍是精确
// 截止点——不能无脑补到分钟末尾，那会把用户明确排除的时间段又放回来。
func TestParseTimeUpperBound_SecondsPrecisionLeftAlone(t *testing.T) {
	bound, ok := parseTimeUpperBound("2026-05-20T09:30:15")
	if !ok {
		t.Fatal("带秒的时间应该能解析")
	}
	want := time.Date(2026, 5, 20, 9, 30, 15, 0, time.Local).UTC()
	if !bound.Equal(want) {
		t.Fatalf("带秒时不应扩展，得到 %v，期望 %v", bound, want)
	}
}

// TestParseTimeUpperBound_InvalidStillRejected 保证错误信息那一条路没被改坏。
func TestParseTimeUpperBound_InvalidStillRejected(t *testing.T) {
	for _, raw := range []string{"", "昨天", "2026/10/02 00:24"} {
		if _, ok := parseTimeUpperBound(raw); ok {
			t.Errorf("%q 不该被当成合法时间", raw)
		}
	}
}

// TestParseTimeFilter_AcceptsOtherForms 保证其余几种写法没被顺手改坏。
