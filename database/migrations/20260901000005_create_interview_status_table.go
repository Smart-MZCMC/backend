package migrations

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

type M20260901000005CreateInterviewStatusTable struct{}

func (m *M20260901000005CreateInterviewStatusTable) Signature() string {
	return "20260901000005_create_interview_status_table"
}

func (m *M20260901000005CreateInterviewStatusTable) Up() error {
	if !facades.Schema().HasTable("interview_status") {
		return facades.Schema().Create("interview_status", func(table schema.Blueprint) {
			table.ID()
			table.Integer("project_id")
			table.String("point_code", 50)
			table.String("point_name", 100)
			table.String("status", 20).Default("offline")
			table.Timestamp("updated_at")
			table.Index("project_id")
		})
	}
	return nil
}

func (m *M20260901000005CreateInterviewStatusTable) Down() error {
	return facades.Schema().DropIfExists("interview_status")
}
