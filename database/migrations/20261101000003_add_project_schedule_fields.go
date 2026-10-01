package migrations

import (
	"log"

	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
	"smart-mzcmc/app/models"
)

// M20261101000003AddProjectScheduleFields 给 projects 补日程与状态字段。
//
// 之前 projects 只有 name/code/description 三个业务字段，导播端「自动选
// 一个项目」只能取 projects.first，也就是 ID 最小的那个——没有任何时间含义，
// 谁先建谁常驻。加上计划时间窗、场地、负责人、状态与模式之后，导播端才能按
// 时间排序把当前/下一场置顶，管理后台也才能做状态流转。
//
// 全部用 HasColumn 单独判断，因为 ALTER TABLE 一次只能加一列，中途失败重跑
// 时要能接着做下去。
//
// 幂等：main.go 的 runMigrations 每次启动都遍历全部迁移、不看是否执行过。
type M20261101000003AddProjectScheduleFields struct{}

func (m *M20261101000003AddProjectScheduleFields) Signature() string {
	return "20261101000003_add_project_schedule_fields"
}

func (m *M20261101000003AddProjectScheduleFields) Up() error {
	if !facades.Schema().HasTable("projects") {
		return nil
	}

	columns := []struct {
		name string
		add  func(schema.Blueprint)
	}{
		{"scheduled_start", func(t schema.Blueprint) { t.Timestamp("scheduled_start").Nullable() }},
		{"scheduled_end", func(t schema.Blueprint) { t.Timestamp("scheduled_end").Nullable() }},
		{"venue", func(t schema.Blueprint) { t.String("venue", 200).Default("") }},
		{"owner_id", func(t schema.Blueprint) { t.UnsignedBigInteger("owner_id").Default(0) }},
		{"status", func(t schema.Blueprint) { t.String("status", 20).Default(models.ProjectStatusPlanned) }},
		{"mode", func(t schema.Blueprint) { t.String("mode", 20).Default(models.ProjectModeLive) }},
	}

	for _, col := range columns {
		if facades.Schema().HasColumn("projects", col.name) {
			continue
		}
		add := col.add
		if err := facades.Schema().Table("projects", func(table schema.Blueprint) {
			add(table)
		}); err != nil {
			return err
		}
		log.Printf("[Migration] 已为 projects 添加 %s 列", col.name)
	}

	// 索引单独建：ColumnDefinition 上的 Index() 在 SQLite 的
	// ALTER TABLE ADD COLUMN 路径上不生效，必须另发 CREATE INDEX。
	if !facades.Schema().HasIndex("projects", "projects_scheduled_start_index") {
		if err := facades.DB().Statement(
			"CREATE INDEX projects_scheduled_start_index ON projects(scheduled_start)",
		); err != nil {
			return err
		}
		log.Printf("[Migration] 已为 projects.scheduled_start 建立索引")
	}
	if !facades.Schema().HasIndex("projects", "projects_owner_id_index") {
		if err := facades.DB().Statement(
			"CREATE INDEX projects_owner_id_index ON projects(owner_id)",
		); err != nil {
			return err
		}
		log.Printf("[Migration] 已为 projects.owner_id 建立索引")
	}

	// 存量行补默认值。SQLite 的 ADD COLUMN 会写 DEFAULT，但这里再兜一次，
	// 免得某些路径下留下空串让前端的判别联合漏档。
	if _, err := facades.Orm().Query().Model(&models.Project{}).
		Where("status = ? OR status IS NULL", "").
		Update(map[string]any{"status": models.ProjectStatusPlanned}); err != nil {
		return err
	}
	if _, err := facades.Orm().Query().Model(&models.Project{}).
		Where("mode = ? OR mode IS NULL", "").
		Update(map[string]any{"mode": models.ProjectModeLive}); err != nil {
		return err
	}

	return nil
}

func (m *M20261101000003AddProjectScheduleFields) Down() error {
	// 删列会丢掉日程与状态，无法恢复，所以什么都不做。
	return nil
}
