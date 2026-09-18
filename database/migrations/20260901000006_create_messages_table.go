package migrations

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

type M20260901000006CreateMessagesTable struct{}

func (m *M20260901000006CreateMessagesTable) Signature() string {
	return "20260901000006_create_messages_table"
}

func (m *M20260901000006CreateMessagesTable) Up() error {
	if !facades.Schema().HasTable("messages") {
		return facades.Schema().Create("messages", func(table schema.Blueprint) {
			table.ID()
			table.Integer("project_id")
			table.Integer("sender_id").Nullable()
			table.String("type", 30)
			table.Text("content").Nullable()
			table.Timestamps()
			table.Index("project_id")
		})
	}
	return nil
}

func (m *M20260901000006CreateMessagesTable) Down() error {
	return facades.Schema().DropIfExists("messages")
}
