package plugins

import (
	"testing"
	"time"
)

// TestParseTimeParam_AcceptsDatetimeLocalWithoutSeconds 防的是「界面上选完
// 时间一导出就 400」。导出接口的 from/to 是必填的，而管理后台提交的是
// <input type="datetime-local"> 的默认格式 `2026-05-20T09:30`——**没有秒**。
// 少这一个 layout，导出按钮就永远点不动，错误信息还只会说「时间格式不正确」。
func TestParseTimeParam_AcceptsDatetimeLocalWithoutSeconds(t *testing.T) {
	got, err := parseTimeParam("2026-05-20T09:30")
	if err != nil {
		t.Fatalf("datetime-local 的分钟精度写法必须能解析: %v", err)
	}
	if got.Hour() != 9 || got.Minute() != 30 || got.Day() != 20 {
		t.Fatalf("解析出的时刻不对: %v", got)
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
