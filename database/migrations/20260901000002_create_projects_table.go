package migrations

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

type M20260901000002CreateProjectsTable struct{}

func (m *M20260901000002CreateProjectsTable) Signature() string {
	return "20260901000002_create_projects_table"
}

func (m *M20260901000002CreateProjectsTable) Up() error {
	if !facades.Schema().HasTable("projects") {
		return facades.Schema().Create("projects", func(table schema.Blueprint) {
			table.ID()
			table.String("name", 100)
			table.String("code", 50)
			table.String("description", 500).Nullable()
			table.Timestamps()
			table.Unique("code")
		})
	}
	return nil
}

func (m *M20260901000002CreateProjectsTable) Down() error {
	return facades.Schema().DropIfExists("projects")
}
