package migrations

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

type M20260901000004CreateProjectLocksTable struct{}

func (m *M20260901000004CreateProjectLocksTable) Signature() string {
	return "20260901000004_create_project_locks_table"
}

func (m *M20260901000004CreateProjectLocksTable) Up() error {
	if !facades.Schema().HasTable("project_locks") {
		return facades.Schema().Create("project_locks", func(table schema.Blueprint) {
			table.ID()
			table.Integer("project_id")
			table.Integer("user_id")
			table.Timestamp("locked_at")
			table.Timestamp("expire_at")
			table.Unique("project_id")
		})
	}
	return nil
}

func (m *M20260901000004CreateProjectLocksTable) Down() error {
	return facades.Schema().DropIfExists("project_locks")
}
