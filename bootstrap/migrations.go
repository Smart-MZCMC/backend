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
	}
}
