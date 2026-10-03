package sysinfo

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 这组用例防的是「字段漏了 json 标签」这类问题。
//
// 已经真实发生过一次：Database.Ping 漏了标签，而 encoding/json 在字段没有
// tag 时会退回用**字段名本身**，于是接口吐出一个 `"Ping"`（大写 P）。前端读的是
// `metrics.database.ping.connected`，取到 undefined，整页在渲染时抛
// TypeError 白屏——一个大小写之差，症状却表现为「监控页打不开」。
//
// 关键在于它不会在编译期或单元测试里暴露：Go 侧一切正常，只有真正序列化之后
// 拿给前端看才知道。所以必须在这里把「键名长什么样」钉死。

func sampleSnapshot(t *testing.T) Snapshot {
	t.Helper()
	return Collect(Params{
		Version:      "1.5.6",
		StartedAt:    time.Now(),
		OnlineCount:  3,
		Driver:       "sqlite",
		DatabasePath: "database/smart-mzcmc.db",
		DataDir:      "database",
		AppDirs:      map[string]string{"database": "database/smart-mzcmc.db"},
		Ping: func(string) PingResult {
			return PingResult{Connected: true, Version: "3.49.1"}
		},
		UpdateEnabled: true,
	})
}

// 顶层与各段的键名。前端 $lib/api/types.ts 的 SystemMetrics 逐字段对应这里，
// 改任何一边都必须同时改另一边，这个用例就是让漏改的那一边先响。
func TestCollect_JSON键名与前端一致(t *testing.T) {
	b, err := json.Marshal(sampleSnapshot(t))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	wantTop := []string{"runtime", "memory", "disk", "database", "components", "collected_at"}
	assertKeys(t, "顶层", got, wantTop)

	section := func(name string) map[string]any {
		v, ok := got[name].(map[string]any)
		if !ok {
			t.Fatalf("%s 不是对象，实际 %T", name, got[name])
		}
		return v
	}

	assertKeys(t, "runtime", section("runtime"), []string{
		"version", "go_version", "platform", "num_cpu", "goroutines", "pid",
		"uptime_seconds", "started_at", "executable", "working_dir", "online_count",
	})
	assertKeys(t, "memory", section("memory"), []string{
		"alloc_bytes", "total_alloc_bytes", "sys_bytes", "heap_alloc_bytes",
		"heap_inuse_bytes", "stack_inuse_bytes", "num_gc", "last_gc", "gc_cpu_fraction",
	})
	assertKeys(t, "disk", section("disk"), []string{
		"path", "supported", "total_bytes", "free_bytes", "used_bytes",
		"used_percent", "app_bytes", "app_bytes_detail", "note",
	})
	assertKeys(t, "database", section("database"), []string{
		"driver", "path", "size_bytes", "ping",
	})

	pingSeg, ok := section("database")["ping"].(map[string]any)
	if !ok {
		t.Fatalf("database.ping 不是对象，实际 %T", section("database")["ping"])
	}
	assertKeys(t, "database.ping", pingSeg, []string{
		"connected", "version", "latency_ms", "error",
	})

	// 单独再钉一次 ping 这个键：它就是当初漏标签的那一个，
	// 且它是页面里唯一被解引用成子对象的（metrics.database.ping.connected）。
	db := section("database")
	if _, ok := db["Ping"]; ok {
		t.Error(`database 里出现了大写的 "Ping" 键：Ping 字段漏了 json 标签，` +
			`encoding/json 退回用了字段名本身，前端读的是小写 ping`)
	}
}

// 回归：探活失败时 ping 这一段仍要完整存在。
//
// 前端无条件渲染 metrics.database.ping.*，所以哪怕探活失败、ping 里全是零值，
// 这一段也不能整个消失——否则监控页恰好在数据库故障时白屏，
// 而那正是最需要它工作的时候。
func TestCollect_探活失败时ping段仍在(t *testing.T) {
	snap := Collect(Params{
		Version:      "1.5.6",
		StartedAt:    time.Now(),
		Driver:       "sqlite",
		DatabasePath: "database/smart-mzcmc.db",
		DataDir:      "database",
		Ping: func(string) PingResult {
			return PingResult{Connected: false, Error: "database is locked"}
		},
	})

	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	db, ok := got["database"].(map[string]any)
	if !ok {
		t.Fatalf("database 不是对象")
	}
	ping, ok := db["ping"].(map[string]any)
	if !ok {
		t.Fatal("探活失败时 database.ping 整段消失了，前端会解引用 undefined 而崩")
	}
	if ping["connected"] != false {
		t.Errorf("探活失败时 connected 应为 false，实际 %v", ping["connected"])
	}
	if ping["error"] != "database is locked" {
		t.Errorf("错误信息应透出，实际 %v", ping["error"])
	}
}

// Params 是 Collect 的入参，从不参与序列化，所以它没有 json 标签是合理的。
//
// 「从不参与」不是靠约定，而是结构上做不到：它带一个 func 字段
// （Ping 探活函数），encoding/json 遇到 func 直接报错。
//
// 这条用例把这件事钉死，因为它同时说明了一个更重要的区别：上面那些输出用的
// 结构体，漏标签时 json.Marshal **不会报错**，只会悄悄把字段名原样吐出去——
// 那才是会一路溜到前端、变成白屏的那种漏法。Params 漏了标签反而是安全的。
func TestParams_结构上无法序列化(t *testing.T) {
	_, err := json.Marshal(Params{Version: "1.0"})
	if err == nil {
		t.Fatal("Params 含 func 字段，序列化本该失败")
	}
	// 必须是「不支持这种类型」而不是别的错：那才说明拦下它的是 func 本身。
	var typeErr *json.UnsupportedTypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("应是 json.UnsupportedTypeError，实际 %T: %v", err, err)
	}
}

func assertKeys(t *testing.T, where string, got map[string]any, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s 的键数量应为 %d，实际 %d（实际键：%v）", where, len(want), len(got), keysOf(got))
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s 缺少键 %q（实际键：%v）", where, k, keysOf(got))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
