package migrations

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

type M20260901000003CreateUserProjectsTable struct{}

func (m *M20260901000003CreateUserProjectsTable) Signature() string {
	return "20260901000003_create_user_projects_table"
}

func (m *M20260901000003CreateUserProjectsTable) Up() error {
	if !facades.Schema().HasTable("user_projects") {
		return facades.Schema().Create("user_projects", func(table schema.Blueprint) {
			table.ID()
			table.Integer("user_id")
			table.Integer("project_id")
			table.Timestamps()
			table.Index("user_id", "project_id")
		})
	}
	return nil
}

func (m *M20260901000003CreateUserProjectsTable) Down() error {
	return facades.Schema().DropIfExists("user_projects")
}
