package migrations

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

type M20210101000001CreateJobsTable struct{}

// Signature The unique signature for the migration.
func (r *M20210101000001CreateJobsTable) Signature() string {
	// 与文件名、类型名对齐。脚手架默认值是 20210101000002，与本文件的编号
	// 差一位，一直没人发现——因为项目没有 migrations 记账表，Signature()
	// 只用于日志标签，不参与「是否已执行」的判断，于是对不上也没有任何症状。
	// bootstrap 有一条测试要求文件名与签名严格一致，不一致时它会指到这里。
	return "20210101000001_create_jobs_table"
}

// Up Run the migrations.
func (r *M20210101000001CreateJobsTable) Up() error {
	if !facades.Schema().HasTable("jobs") {
		if err := facades.Schema().Create("jobs", func(table schema.Blueprint) {
			table.ID()
			table.String("queue")
			table.LongText("payload")
			table.UnsignedTinyInteger("attempts").Default(0)
			table.DateTimeTz("reserved_at").Nullable()
			table.DateTimeTz("available_at")
			table.DateTimeTz("created_at").UseCurrent()
			table.Index("queue")
		}); err != nil {
			return err
		}
	}

	if !facades.Schema().HasTable("failed_jobs") {
		if err := facades.Schema().Create("failed_jobs", func(table schema.Blueprint) {
			table.ID()
			table.String("uuid")
			table.Text("connection")
			table.Text("queue")
			table.LongText("payload")
			table.LongText("exception")
			table.DateTimeTz("failed_at").UseCurrent()
			table.Unique("uuid")
		}); err != nil {
			return err
		}
	}

	return nil
}

// Down Reverse the migrations.
func (r *M20210101000001CreateJobsTable) Down() error {
	if err := facades.Schema().DropIfExists("jobs"); err != nil {
		return err
	}

	if err := facades.Schema().DropIfExists("failed_jobs"); err != nil {
		return err
	}

	return nil
}
