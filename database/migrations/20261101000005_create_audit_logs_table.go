package migrations

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

// M20261101000005CreateAuditLogsTable 建 audit_logs 表。
//
// 之前 auditAction/auditRoleChange 只往 stdout 打日志，而 stdout 日志由
// config/logging.go 定为 7 天轮转，且只覆盖三处操作（删用户、改角色、系统
// 更新）。项目增删改、权限授予撤销、日志清理（它会删证据）、改个人资料都没
// 有痕迹。落库之后这些记录与文件日志的保留期脱钩，管理后台也能直接检索。
//
// 幂等：main.go 的 runMigrations 每次启动都遍历全部迁移、不看是否执行过。
type M20261101000005CreateAuditLogsTable struct{}

func (m *M20261101000005CreateAuditLogsTable) Signature() string {
	return "20261101000005_create_audit_logs_table"
}

func (m *M20261101000005CreateAuditLogsTable) Up() error {
	if facades.Schema().HasTable("audit_logs") {
		return nil
	}
	return facades.Schema().Create("audit_logs", func(table schema.Blueprint) {
		table.ID()
		table.UnsignedBigInteger("actor_id").Default(0)
		// 只存脱敏后的用户名：审计表会被导出与翻看，完整账号名没必要。
		table.String("actor_username", 64).Default("")
		table.String("action", 64)
		table.String("target_type", 32).Default("")
		table.String("target_id", 64).Default("")
		table.Text("detail").Nullable()
		table.String("ip", 64).Default("")
		table.Timestamp("created_at").Nullable()
		// 后台主查询是「按时间倒序翻页」，其次是「按操作类型筛选」。
		table.Index("created_at")
		table.Index("action")
		table.Index("actor_id")
	})
}

func (m *M20261101000005CreateAuditLogsTable) Down() error {
	return facades.Schema().DropIfExists("audit_logs")
}
