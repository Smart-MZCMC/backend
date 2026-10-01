package plugins

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestParseTimeParam_AcceptsDatetimeLocalWithoutSeconds 防的是「界面上选完
// 时间一导出就 400」。导出接口的 from/to 是必填的，而管理后台提交的是
// <input type="datetime-local"> 的默认格式 `2026-05-20T09:30`——**没有秒**。
// 少这一个 layout，导出按钮就永远点不动，错误信息还只会说「时间格式不正确」。
//
// 断言的是「等于把本地 09:30 换算成 UTC」，而不是「小时数就是 9」：参数是
// 用户所在时区的墙上时间，而 created_at 存 UTC，返回值必须已经换算。后者那种
// 断言在开发机位于 UTC 时恰好也成立，等于什么都没验。
func TestParseTimeParam_AcceptsDatetimeLocalWithoutSeconds(t *testing.T) {
	got, err := parseTimeParam("2026-05-20T09:30")
	if err != nil {
		t.Fatalf("datetime-local 的分钟精度写法必须能解析: %v", err)
	}
	want := time.Date(2026, 5, 20, 9, 30, 0, 0, time.Local).UTC()
	if !got.Equal(want) {
		t.Fatalf("应等于把本地 09:30 换算成 UTC: 得到 %v，期望 %v", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("结果时区应是 UTC，实际 %v", got.Location())
	}
	if got.Day() != 20 || got.Minute() != 30 {
		t.Fatalf("日期与分钟数应保留: %v", got)
	}
}

// TestParseTimeParam_ConvertsWallClockToUTC 锁住时区换算确实发生了。
//
// 在非 UTC 的时区里用子进程验证：Windows 上 Go 不读 TZ 环境变量（实测设了
// 也没用），直接改 time.Local 全局变量又会污染并行测试，所以只能起子进程
// 并在里面显式赋值。
func TestParseTimeParam_ConvertsWallClockToUTC(t *testing.T) {
	if os.Getenv("GO_TEST_TZ_CHILD") == "1" {
		loc, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			fmt.Fprintln(os.Stderr, "SKIP: 本机没有时区数据库")
			os.Exit(0)
		}
		time.Local = loc

		got, err := parseTimeParam("2026-05-20T09:30")
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
			os.Exit(1)
		}
		// 东八区 09:30 == UTC 01:30
		if formatted := got.UTC().Format("2006-01-02T15:04"); formatted != "2026-05-20T01:30" {
			fmt.Fprintf(os.Stderr, "FAIL: 得到 %s，期望 2026-05-20T01:30\n", formatted)
			os.Exit(1)
		}
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestParseTimeParam_ConvertsWallClockToUTC")
	cmd.Env = append(os.Environ(), "GO_TEST_TZ_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程失败（期望墙上时间 09:30 被换算成 UTC 01:30）:\n%s", out)
	}
}

// TestParseTimeParam_EmptyMeansAbsent 保证空串返回零值 + 无错误：
// 「没传」与「传了但格式错」是两种不同的情况，前者由调用方报「必须提供
// 时间范围」，后者报格式错误。
func TestParseTimeParam_EmptyMeansAbsent(t *testing.T) {
	got, err := parseTimeParam("")
	if err != nil {
		t.Fatalf("空串不该报格式错误: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("空串应返回零值，实际是 %v", got)
	}
}

// TestParseTimeParam_RejectsGarbage 保证格式错的值被判成错误，而不是悄悄
// 当成零值——那会让 from/to 校验失效，导出重新变成「无条件全表查询」。
func TestParseTimeParam_RejectsGarbage(t *testing.T) {
	if _, err := parseTimeParam("上周"); err == nil {
		t.Fatal("无法解析的写法必须报错")
	}
}

// TestIsDateOnlyParam 防的是「选到某一天却拿不到那天的数据」。
func TestIsDateOnlyParam(t *testing.T) {
	if !isDateOnlyParam("2026-05-20") {
		t.Fatal("纯日期应判定为 date-only")
	}
	if isDateOnlyParam("2026-05-20T09:30") {
		t.Fatal("带具体时刻的不该判定为 date-only")
	}
}

// TestLogArchive_DefaultPresenceTimings 保证掉线扫描的默认值与注释一致。
//
// 这两项决定「采访端走出 WiFi 后多久导演播端会变红」，配错了会直接表现为
// 界面在撒谎：值太大就长时间显示绿色「就绪」。
func TestLogArchive_DefaultPresenceTimings(t *testing.T) {
	l := NewLogArchive(LogArchiveConfig{Enabled: true})
	if l.presenceInterval != 60*time.Second {
		t.Fatalf("掉线扫描间隔默认应为 60s，实际 %v", l.presenceInterval)
	}
	if l.presenceTimeout != 90*time.Second {
		t.Fatalf("掉线判定阈值默认应为 90s，实际 %v", l.presenceTimeout)
	}
	if l.hasPresenceScanner() {
		t.Fatal("没注入扫描函数时不该声称有扫描器")
	}

	l.SetPresenceScanner(func(time.Duration) {})
	if !l.hasPresenceScanner() {
		t.Fatal("注入后应报告有扫描器")
	}
}
