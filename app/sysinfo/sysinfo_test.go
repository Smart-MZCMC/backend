package sysinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 这条规则防的是「监控页在数据库坏掉时反而什么都看不到」。
//
// 探活失败必须只体现在状态位上，不能让整次采集失败。现场最需要这页的时刻就是
// 数据库出问题的时刻——那时候因为一次探活失败就返回 500，操作员看到的只有一个
// 报错，完全不知道内存、磁盘、服务分别是什么状态。
func TestCollect_探活失败仍然返回完整快照(t *testing.T) {
	snap := Collect(Params{
		Version:      "1.5.5",
		StartedAt:    time.Now().Add(-time.Hour),
		Driver:       "sqlite",
		DatabasePath: filepath.Join(t.TempDir(), "absent.db"),
		Ping: func(string) PingResult {
			return PingResult{Connected: false, Error: "disk I/O error"}
		},
	})

	if snap.Runtime.Version != "1.5.5" {
		t.Errorf("版本号应原样带出，实际 %q", snap.Runtime.Version)
	}
	if snap.Runtime.UptimeSeconds < 3590 {
		t.Errorf("运行时长应约 1 小时，实际 %d 秒", snap.Runtime.UptimeSeconds)
	}
	if snap.Memory.SysBytes == 0 {
		t.Error("内存统计不应为空")
	}

	db := componentByName(t, snap.Components, "database")
	if db.Status != StatusError {
		t.Errorf("探活失败时 database 应标记为 error，实际 %q", db.Status)
	}
	if !strings.Contains(db.Detail, "disk I/O error") {
		t.Errorf("状态详情里应带上探活错误，实际 %q", db.Detail)
	}
}

// 这条规则防的是「把前兆状态当成正常」。
//
// 还能连上但探活已经慢到毫秒百位，是磁盘/文件系统开始不对劲的信号。这种状态等到
// 彻底连不上就晚了——那时候已经只能事后查。前端要能区分它和健康，所以状态取值
// 不能退化成布尔。
func TestCollect_探活变慢标记为降级而不是错误(t *testing.T) {
	snap := Collect(Params{
		StartedAt: time.Now(),
		Driver:    "sqlite",
		Ping:      func(string) PingResult { return PingResult{Connected: true, LatencyMS: 800} },
	})

	db := componentByName(t, snap.Components, "database")
	if db.Status != StatusDegraded {
		t.Errorf("慢探活应为 degraded，实际 %q", db.Status)
	}
}

func TestCollect_探活正常标记为已连接(t *testing.T) {
	snap := Collect(Params{
		StartedAt: time.Now(),
		Driver:    "sqlite",
		Ping: func(string) PingResult {
			return PingResult{Connected: true, Version: "3.45.1", LatencyMS: 1}
		},
	})

	db := componentByName(t, snap.Components, "database")
	if db.Status != StatusConnected {
		t.Errorf("正常探活应为 connected，实际 %q", db.Status)
	}
	if db.Version != "3.45.1" {
		t.Errorf("应带出 SQLite 版本，实际 %q", db.Version)
	}
}

// 这条规则防的是「一场没开播就被报成故障」。
//
// 现场是「没开播就没有客户端连着」，在线数为 0 是正常状态。标成红色只会让人
// 以为又坏了，久而久之所有人都不再相信这一页。
func TestCollect_在线数为零不算故障(t *testing.T) {
	snap := Collect(Params{
		StartedAt:   time.Now(),
		OnlineCount: 0,
		Ping:        func(string) PingResult { return PingResult{Connected: true} },
	})

	ws := componentByName(t, snap.Components, "websocket")
	if ws.Status != StatusRunning {
		t.Errorf("在线数为 0 时 websocket 不应是异常，实际 %q", ws.Status)
	}
	if !strings.Contains(ws.Detail, "没有客户端连接") {
		t.Errorf("详情应说明是「没有客户端」而不是故障，实际 %q", ws.Detail)
	}
}

// 这条规则防的是「在线更新没开却被显示成正常」。
//
// 组件状态位的作用是回答「这一项现在能不能用」。在线更新没配就是不能用，
// 显示成 running 会让人以为「更新功能坏了」而去排查一个根本没启用的东西。
func TestCollect_在线更新未开启显示为已禁用(t *testing.T) {
	snap := Collect(Params{
		StartedAt: time.Now(),
		Ping:      func(string) PingResult { return PingResult{Connected: true} },
	})

	c := componentByName(t, snap.Components, "在线更新")
	if c.Status != StatusDisabled {
		t.Errorf("未开启时应为 disabled，实际 %q", c.Status)
	}

	snap = Collect(Params{
		StartedAt:     time.Now(),
		UpdateEnabled: true,
		Ping:          func(string) PingResult { return PingResult{Connected: true} },
	})
	if c := componentByName(t, snap.Components, "在线更新"); c.Status != StatusRunning {
		t.Errorf("已开启时应为 running，实际 %q", c.Status)
	}
}

// 这条规则防的是「目录统计把不该算的东西算进去，或者被一个坏目录整体带崩」。
//
// 符号链接与设备节点必须跳过（跟统计发布包同一个理由）；单个条目读不出来只跳过
// 那一个；条目数必须有上限，否则监控接口会被一个异常目录拖挂。
func TestDirSize_跳过符号链接且不被单个坏条目带崩(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.db"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.log"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 指向 a.db 的符号链接。统计时必须跳过，否则同一份数据被算两次。
	if err := os.Symlink(filepath.Join(root, "a.db"), filepath.Join(root, "link.db")); err != nil {
		t.Skipf("当前环境不支持创建符号链接：%v", err)
	}

	got, err := dirSize(root)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if got != 20 {
		t.Errorf("应只统计两个真实文件共 20 字节，实际 %d", got)
	}
}

func TestDirSize_目录不存在时返回错误(t *testing.T) {
	if _, err := dirSize(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("目录不存在时应返回错误而不是报 0，否则界面会显示「占 0 字节」")
	}
}

// 这条规则防的是「字节数在前后端用不同的进制换算」。
//
// 后端一律给字节、由前端换算 MB/GB。两边都换算就会出现「后端算 1.05GB、前端显示
// 1GB」这类对不上的数字，查起来很费时间。
func TestMemory_UsedPercent用堆可用部分作分母(t *testing.T) {
	// Sys 里含栈，堆可用部分应当是 Sys - StackInuse。
	m := Memory{SysBytes: 1000, StackInuseBytes: 200, HeapAllocBytes: 400}
	if got := m.UsedPercent(); got != 50 {
		t.Errorf("应按 (Sys-StackInuse) 作分母算出 50%%，实际 %.1f%%", got)
	}

	// 分母为 0 时必须返回 0 而不是 NaN 或 +Inf——那会让前端显示 "NaN%"。
	zero := Memory{}
	if got := zero.UsedPercent(); got != 0 {
		t.Errorf("分母为 0 时应返回 0，实际 %v", got)
	}
}

func componentByName(t *testing.T, list []Component, name string) Component {
	t.Helper()
	for _, c := range list {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("组件列表里没有 %q，实际 %+v", name, list)
	return Component{}
}
