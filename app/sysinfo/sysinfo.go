// Package sysinfo 采集服务自身的运行指标，供管理后台的系统监控页展示。
//
// 为什么单独一个包而不是塞进 controller：这里全是「采集 + 归一化」逻辑，不依赖
// http 上下文，可以直接写单元测试。controller 只负责鉴权与序列化。
//
// 刻意不引 gopsutil 之类的依赖：需要的几项 runtime.MemStats、Statfs、目录求和
// 标准库都有，而这类依赖一旦引入就要跟着升级、跟着审 CVE，现场服务器未必有外网
// 去拉。宁可自己写那几十行。
package sysinfo

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Snapshot 是一次采集的完整结果。
//
// 所有字节数统一用 int64 字节，前端自己换算成 MB/GB——后端换算就要决定用 1024
// 还是 1000，两边不一致时会出现「后端说 1GB、前端显示 0.95GB」这种查半天的问题。
type Snapshot struct {
	// Runtime 进程与运行环境的基本信息。
	Runtime Runtime `json:"runtime"`
	// Memory Go 堆与内存分配统计。
	Memory Memory `json:"memory"`
	// Disk 承载数据文件的文件系统用量。
	Disk Disk `json:"disk"`
	// Database 数据库的连通性与库文件大小。
	Database Database `json:"database"`
	// Components 各组件的健康状态位。
	Components []Component `json:"components"`
	// CollectedAt 采集时刻。
	CollectedAt time.Time `json:"collected_at"`
}

// Runtime 进程与运行环境。
type Runtime struct {
	Version       string `json:"version"`
	GoVersion     string `json:"go_version"`
	Platform      string `json:"platform"`
	NumCPU        int    `json:"num_cpu"`
	Goroutines    int    `json:"goroutines"`
	PID           int    `json:"pid"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	StartedAt     string `json:"started_at"`
	Executable    string `json:"executable"`
	WorkingDir    string `json:"working_dir"`
	OnlineCount   int    `json:"online_count"`
}

// Memory Go 运行时内存统计。字段名与 runtime.MemStats 对齐，便于对照排查。
type Memory struct {
	AllocBytes      uint64  `json:"alloc_bytes"`
	TotalAllocBytes uint64  `json:"total_alloc_bytes"`
	SysBytes        uint64  `json:"sys_bytes"`
	HeapAllocBytes  uint64  `json:"heap_alloc_bytes"`
	HeapInuseBytes  uint64  `json:"heap_inuse_bytes"`
	StackInuseBytes uint64  `json:"stack_inuse_bytes"`
	NumGC           uint32  `json:"num_gc"`
	LastGC          string  `json:"last_gc"`
	GCCPUFraction   float64 `json:"gc_cpu_fraction"`
}

// UsedPercent 堆占用率：HeapAlloc 占「堆可用部分」的比例。
//
// 分母用 Sys 减去 StackInuse 而不是直接用 Sys：Sys 还包含代码段、栈、以及向 OS
// 要来但还没还给 OS 的 reserved 部分。拿它当分母，服务刚启动时算出来的「内存
// 使用率」就会显得很高，而实际占用其实很少——那种数字会让人白折腾一轮。
func (m Memory) UsedPercent() float64 {
	heapSys := m.SysBytes - m.StackInuseBytes
	if heapSys == 0 {
		return 0
	}
	return float64(m.HeapAllocBytes) / float64(heapSys) * 100
}

// Disk 承载数据文件的文件系统用量。
//
// 单独列出来是因为「磁盘满」是现场最常见的故障：数据库写不进去、WebSocket 断连，
// 而数据库、日志、程序往往都在同一个分区上。
type Disk struct {
	Path           string    `json:"path"`
	Supported      bool      `json:"supported"`
	TotalBytes     uint64    `json:"total_bytes"`
	FreeBytes      uint64    `json:"free_bytes"`
	UsedBytes      uint64    `json:"used_bytes"`
	UsedPercent    float64   `json:"used_percent"`
	AppBytes       uint64    `json:"app_bytes"`
	AppBytesDetail []DirSize `json:"app_bytes_detail"`
	// Note 在拿不到文件系统用量时说明原因（前端直接显示，别让人对着一片 0 猜）。
	Note string `json:"note"`
}

// DirSize 单个目录的递归大小。
type DirSize struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
	Error string `json:"error,omitempty"`
}

// PingResult 一次数据库探活的结果。
type PingResult struct {
	Connected bool   `json:"connected"`
	Version   string `json:"version"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error"`
}

// Database 数据库连通性与库文件大小。
//
// 不暴露连接池计数：Goravel 的 db.DB 接口不提供底层 *sql.DB，拿不到 Stats()；
// 而 SQLite 是文件型数据库，「打开连接数」本身也没有诊断价值。这里改成真正执行
// 一次 SELECT 1——「现在能不能查到」比池里的数字更能说明问题。
type Database struct {
	Driver    string `json:"driver"`
	Path      string `json:"path"`
	SizeBytes uint64 `json:"size_bytes"`
	Ping      PingResult
}

// Component 单个组件的健康状态位。
type Component struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// 组件状态取值。刻意用字符串而不是布尔：前端要区分「正常」与「降级」，
// 布尔只能表示正常/不正常两种状态，而「连得上但很慢」是最容易被漏掉的前兆状态。
const (
	StatusRunning   = "running"
	StatusConnected = "connected"
	StatusDegraded  = "degraded"
	StatusError     = "error"
	StatusDisabled  = "disabled"
)

// Params 是采集所需的外部输入。
//
// 刻意全部显式传入而不在包内读全局状态（启动时刻、版本、在线数、探活函数…），
// 这样测试能构造出确定的输入，断言才有意义。
type Params struct {
	Version      string
	StartedAt    time.Time
	OnlineCount  int
	Driver       string
	DatabasePath string
	// DataDir 是统计文件系统用量的目录，通常取 DatabasePath 所在目录。
	DataDir string
	// AppDirs 需要单独统计大小的目录，键是展示名。
	AppDirs map[string]string
	// Ping 执行一次探活。
	Ping func(databasePath string) PingResult
	// UpdateEnabled 决定「在线更新」这个组件显示成什么状态。
	UpdateEnabled bool
}

// Collect 执行一次采集。
//
// 探活失败**不会**让整次采集失败——监控页恰恰在数据库出问题时最需要显示内容，
// 这时候因为一次探活失败就返回 500，页面直接什么都看不到。失败只体现在
// Database.Ping.Connected=false 与 Components 的状态位上。
func Collect(p Params) Snapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	wd, _ := os.Getwd()

	mem := Memory{
		AllocBytes:      ms.Alloc,
		TotalAllocBytes: ms.TotalAlloc,
		SysBytes:        ms.Sys,
		HeapAllocBytes:  ms.HeapAlloc,
		HeapInuseBytes:  ms.HeapInuse,
		StackInuseBytes: ms.StackInuse,
		NumGC:           ms.NumGC,
		GCCPUFraction:   ms.GCCPUFraction,
	}
	if ms.LastGC > 0 {
		mem.LastGC = time.Unix(0, int64(ms.LastGC)).Format(time.RFC3339)
	}

	db := Database{Driver: p.Driver, Path: p.DatabasePath}
	if info, err := os.Stat(p.DatabasePath); err == nil {
		db.SizeBytes = uint64(info.Size())
	}
	if p.Ping != nil {
		db.Ping = p.Ping(p.DatabasePath)
	} else {
		db.Ping = PingResult{Error: "未接入探活"}
	}

	disk := collectDisk(p.DataDir, p.AppDirs)

	return Snapshot{
		Runtime: Runtime{
			Version:       p.Version,
			GoVersion:     runtime.Version(),
			Platform:      runtime.GOOS + "/" + runtime.GOARCH,
			NumCPU:        runtime.NumCPU(),
			Goroutines:    runtime.NumGoroutine(),
			PID:           os.Getpid(),
			UptimeSeconds: int64(time.Since(p.StartedAt).Seconds()),
			StartedAt:     p.StartedAt.Format(time.RFC3339),
			Executable:    exe,
			WorkingDir:    wd,
			OnlineCount:   p.OnlineCount,
		},
		Memory:      mem,
		Disk:        disk,
		Database:    db,
		Components:  components(p, db.Ping, disk),
		CollectedAt: time.Now(),
	}
}

func components(p Params, ping PingResult, disk Disk) []Component {
	// 直接用 Collect 已经拿到的那次探活结果，不再重新探一次活：
	// 探一次活要真的走一遍数据库，这里重复调用会让监控接口自己多花一份延迟。
	dbState := StatusConnected
	dbDetail := p.Driver
	switch {
	case !ping.Connected:
		dbState = StatusError
		dbDetail = "探活失败"
		if ping.Error != "" {
			dbDetail += "：" + ping.Error
		}
	case ping.LatencyMS > slowPingMS:
		// 连得上但很慢：这种状态最容易漏掉，而它往往就是下一次故障的前兆。
		dbState = StatusDegraded
		dbDetail = fmt.Sprintf("%s，探活 %dms 偏慢", p.Driver, ping.LatencyMS)
	}

	diskState, diskDetail := diskState(disk)

	update := Component{
		Name:   "在线更新",
		Status: StatusDisabled,
		Detail: "未在 .env 里开启 UPDATE_ENABLED",
	}
	if p.UpdateEnabled {
		update = Component{Name: "在线更新", Status: StatusRunning, Detail: "已开启"}
	}

	return []Component{
		{
			Name:    "api",
			Status:  StatusRunning,
			Version: p.Version,
			Detail:  fmt.Sprintf("PID %d，工作目录 %s", os.Getpid(), p.DataDir),
		},
		{Name: "database", Status: dbState, Detail: dbDetail, Version: ping.Version},
		{Name: "storage", Status: diskState, Detail: diskDetail},
		{Name: "websocket", Status: StatusRunning, Detail: onlineDetail(p.OnlineCount)},
		update,
	}
}

// slowPingMS 探活超过这个毫秒数就标记为降级。
//
// SQLite 读一条常量理论上在微秒级；到毫秒百位以上通常意味着磁盘或文件系统开始
// 不妙，正是「还能用但已经不对」的那一段——等它彻底失败就晚了。
const slowPingMS = 500

func onlineDetail(n int) string {
	if n <= 0 {
		return "正常，当前没有客户端连接"
	}
	return "正常"
}

func diskState(d Disk) (state, detail string) {
	switch {
	case !d.Supported:
		return StatusDisabled, d.Note
	case d.UsedPercent >= 90:
		return StatusError, fmt.Sprintf("%s 已用 %.0f%%，数据库将无法写入", d.Path, d.UsedPercent)
	case d.UsedPercent >= 80:
		return StatusDegraded, fmt.Sprintf("%s 已用 %.0f%%", d.Path, d.UsedPercent)
	default:
		return StatusRunning, d.Path
	}
}
