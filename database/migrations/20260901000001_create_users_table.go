package migrations

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

type M20260901000001CreateUsersTable struct{}

func (m *M20260901000001CreateUsersTable) Signature() string {
	return "20260901000001_create_users_table"
}

func (m *M20260901000001CreateUsersTable) Up() error {
	if !facades.Schema().HasTable("users") {
		return facades.Schema().Create("users", func(table schema.Blueprint) {
			table.ID()
			table.String("username", 50)
			table.String("password")
			table.String("display_name", 100).Nullable()
			table.String("role", 20).Default("director")
			table.Timestamps()
			table.Unique("username")
		})
	}
	return nil
}

func (m *M20260901000001CreateUsersTable) Down() error {
	return facades.Schema().DropIfExists("users")
}
