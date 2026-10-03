package bootstrap

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/database/migrations"
)

func Migrations() []schema.Migration {
	return []schema.Migration{
		// 保留原有的 jobs 表
		&migrations.M20210101000001CreateJobsTable{},
		// 核心业务表
		&migrations.M20260901000001CreateUsersTable{},
		&migrations.M20260901000002CreateProjectsTable{},
		&migrations.M20260901000003CreateUserProjectsTable{},
		&migrations.M20260901000004CreateProjectLocksTable{},
		&migrations.M20260901000005CreateInterviewStatusTable{},
		&migrations.M20260901000006CreateMessagesTable{},
		// 数据清理
		&migrations.M20260926000001PurgeHeartbeatMessages{},
		// 引入超级管理员后，为存量部署补一个，否则没人能授予该角色
		&migrations.M20261001000001EnsureSuperAdmin{},
		// 用户中心：邮箱（WeAvatar 头像来源）与令牌版本（改密码即失效）
		&migrations.M20261002000001AddUserProfileFields{},
		// 切台状态落库：让中途连上来的解说端/包装端能立刻拿到当前状态
		&migrations.M20261101000001CreateProjectStatesTable{},
		// 切台流水：时间线、机位/时段筛选、次数与停留时长报表
		&migrations.M20261101000002CreateShotCutsTable{},
		// 项目日程与状态流转字段
		&migrations.M20261101000003AddProjectScheduleFields{},
		// 机位预设由项目自己配置，不再硬编码在导播端
		&migrations.M20261101000004CreateProjectCamerasTable{},
		// 敏感操作审计落库，不再只依赖 7 天轮转的 stdout 日志
		&migrations.M20261101000005CreateAuditLogsTable{},
		// 权限策略的运行时存储：播种一次之后表即唯一事实来源，policy.csv 退为兜底
		&migrations.M20261102000001CreateRolePermissionsTable{},
	}
}
