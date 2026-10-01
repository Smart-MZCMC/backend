package migrations

import (
	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

// M20261101000001CreateProjectStatesTable 建 project_states 表。
//
// 背景：切台状态此前只存在于 messages.content 的不透明 JSON 里，靠
// 「新的 shot_state 到来」触发广播。中途连上来的解说端/包装端因此收不到
// 任何状态，只能停在「等待导播指令」，重连也一样——ScheduleReconnect
// 只是重连，没有「把当前状态给我」这一步。
//
// 有这张表之后，WebSocket 握手完成时的欢迎消息就能带上 current_shot。
//
// 幂等：main.go 的 runMigrations 每次启动都遍历全部迁移、不看是否执行过。
type M20261101000001CreateProjectStatesTable struct{}

func (m *M20261101000001CreateProjectStatesTable) Signature() string {
	return "20261101000001_create_project_states_table"
}

func (m *M20261101000001CreateProjectStatesTable) Up() error {
	if facades.Schema().HasTable("project_states") {
		return nil
	}
	return facades.Schema().Create("project_states", func(table schema.Blueprint) {
		// project_id 既是业务外键也是主键：一个项目只保留「当前」状态，
		// 历史轨迹在 shot_cuts 里，不需要在这里留多行。
		table.UnsignedBigInteger("project_id")
		table.String("current_shot", 100).Default("")
		table.String("next_shot", 100).Default("")
		table.Timestamp("updated_at").Nullable()
		table.Primary("project_id")
	})
}

func (m *M20261101000001CreateProjectStatesTable) Down() error {
	return facades.Schema().DropIfExists("project_states")
}
