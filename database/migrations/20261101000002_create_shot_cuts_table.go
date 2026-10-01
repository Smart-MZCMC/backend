package migrations

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

// M20261101000002CreateShotCutsTable 建 shot_cuts 表。
//
// 一次切台动作一行，供切台时间线、按机位/时段筛选、切台次数与平均停留时长
// 报表、彩排与正式分场统计使用。此前这些信息只能从 messages.content 的
// JSON 文本里反解，既慢又不可靠。
//
// 幂等：main.go 的 runMigrations 每次启动都遍历全部迁移、不看是否执行过。
type M20261101000002CreateShotCutsTable struct{}

func (m *M20261101000002CreateShotCutsTable) Signature() string {
	return "20261101000002_create_shot_cuts_table"
}

func (m *M20261101000002CreateShotCutsTable) Up() error {
	if facades.Schema().HasTable("shot_cuts") {
		return nil
	}
	return facades.Schema().Create("shot_cuts", func(table schema.Blueprint) {
		table.ID()
		table.UnsignedBigInteger("project_id")
		table.String("from_shot", 100).Default("")
		table.String("to_shot", 100).Default("")
		table.UnsignedBigInteger("director_id").Default(0)
		table.String("mode", 20).Default("live")
		table.Timestamp("cut_at").Nullable()
		// 报表的主查询路径是「某项目某时间段，按时间正序」，这个复合索引
		// 正好覆盖它；单独的 project_id 索引就多余了。
		table.Index("project_id", "cut_at")
		table.Index("to_shot")
	})
}

func (m *M20261101000002CreateShotCutsTable) Down() error {
	return facades.Schema().DropIfExists("shot_cuts")
}
