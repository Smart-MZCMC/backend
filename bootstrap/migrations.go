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
	}
}
