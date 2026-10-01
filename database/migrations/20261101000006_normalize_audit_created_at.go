package migrations

import (
	"log"

	"smart-mzcmc/app/facades"
)

// M20261101000006NormalizeAuditCreatedAt 把 audit_logs.created_at 统一成 UTC。
//
// 背景：app/audit.Write 之前用 time.Now() 显式赋 CreatedAt，带上了本地时区，
// 于是列里存的是 `2026-10-02T00:13:19.6237187+08:00`；而其余靠 GORM 自动
// 时间戳的表存的是 `2026-10-01T16:13:19Z`。两种格式混在同一张表里。
//
// 为什么这会让审计页查不出东西：created_at 在 SQLite 里是 TEXT，`>=` 与 `<=`
// 走的是**字符串**比较，不做任何时间语义解析。筛选条件经 parseTimeFilter
// 归一成 UTC 之后是 `...16:14:00Z`，而 `'2026-10-02T00:13:19+08:00'` 按字符
// 比较大于它，于是 `created_at <= to` 一条都不成立。
// 管理后台的审计页默认就带「最近 7 天」，所以表现为「明明有记录，页面却是空的」，
// 且不带筛选时又能看到——这也是它一直没被发现的原因。
//
// 写入侧已在 app/audit/audit.go 改成 time.Now().UTC()；这个迁移负责把
// 已经写进去的历史行修好，否则改完代码老的记录依然查不出来。
//
// 幂等：runMigrations 每次启动都遍历全部迁移、不看是否执行过。
// WHERE 只匹配仍带时区偏移的行，修完一次之后这条 UPDATE 匹配 0 行。
type M20261101000006NormalizeAuditCreatedAt struct{}

func (m *M20261101000006NormalizeAuditCreatedAt) Signature() string {
	return "20261101000006_normalize_audit_created_at"
}

func (m *M20261101000006NormalizeAuditCreatedAt) Up() error {
	if !facades.Schema().HasTable("audit_logs") {
		return nil
	}
	if !facades.Schema().HasColumn("audit_logs", "created_at") {
		return nil
	}

	// strftime 能识别 ISO8601 的偏移量并换算成 UTC。
	//
	// 只改仍带偏移的行：以 Z 结尾的（GORM 自动时间戳写的）已经是 UTC，
	// 没有偏移的裸时间戳则可能是历史遗留的本地墙上时间，含义不确定，
	// 宁可不猜也不要改错——改错等于凭空造出一条错误时间的证据。
	//
	// 负偏移（`-05:00`）也要覆盖，不能只判 '+'。
	if err := facades.DB().Statement(
		"UPDATE audit_logs SET created_at = strftime('%Y-%m-%dT%H:%M:%f', created_at) || 'Z'" +
			" WHERE created_at LIKE '%+__:__' OR created_at LIKE '%-__:__'",
	); err != nil {
		// 迁移失败不应该把整个服务卡在启动阶段：审计数据的格式问题不影响
		// 直播链路，记一条日志让运维能查到，剩下的新记录本来就是 UTC。
		log.Printf("[迁移] 归一 audit_logs.created_at 失败（不影响服务启动）: %v", err)
	}
	return nil
}

func (m *M20261101000006NormalizeAuditCreatedAt) Down() error {
	// 无法还原：偏移量一旦丢掉就找不回来了，而「猜」回去只会造出错误的时间。
	return nil
}
