package controllers

import (
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
	if got.Hour() != 9 || got.Minute() != 30 {
		t.Fatalf("解析出的时刻不对: %v", got)
	}
	if got.Location() != time.Local {
		t.Fatalf("应按本地时区解析，实际是 %v", got.Location())
	}
}

// TestParseTimeFilter_AcceptsOtherForms 保证其余几种写法没被顺手改坏。
func TestParseTimeFilter_AcceptsOtherForms(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Time
	}{
		{"2026-05-20", time.Date(2026, 5, 20, 0, 0, 0, 0, time.Local)},
		{"2026-05-20 09:30:15", time.Date(2026, 5, 20, 9, 30, 15, 0, time.Local)},
		{"2026-05-20T09:30:15", time.Date(2026, 5, 20, 9, 30, 15, 0, time.Local)},
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

// TestParseTimeFilter_RejectsGarbage 保证解析失败的写法不被静默当成零值——
// 那会让筛选条件悄悄失效，返回全表数据。
func TestParseTimeFilter_RejectsGarbage(t *testing.T) {
	for _, raw := range []string{"昨天", "2026/05/20", "20260520"} {
		if _, ok := parseTimeFilter(raw); ok {
			t.Fatalf("%q 不该被解析成功", raw)
		}
	}
	if _, ok := parseTimeFilter(""); ok {
		t.Fatal("空串表示「没传这个筛选条件」，不该当成时间")
	}
}

// TestIsDateOnly 防的是「选到某一天却拿不到那天的数据」：只有日期时结束时间
// 要补到当天 23:59:59，而带具体时刻的（含 datetime-local）是用户指定的截止点，
// 不能擅自往后扩一整天。
func TestIsDateOnly(t *testing.T) {
	if !isDateOnly("2026-05-20") {
		t.Fatal("纯日期应判定为 date-only")
	}
	for _, raw := range []string{"2026-05-20T09:30", "2026-05-20 09:30:15", "2026-05-20T09:30:15"} {
		if isDateOnly(raw) {
			t.Fatalf("%q 带了具体时刻，不该判定为 date-only", raw)
		}
	}
}
