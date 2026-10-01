package migrations

import (
	"log"

	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
	"smart-mzcmc/app/models"
)

// M20261101000004CreateProjectCamerasTable 建 project_cameras 表并给存量项目
// 播下默认机位。
//
// 导播端的 10 个预设按钮此前硬编码在 Dart 里，换个场地就得改代码重新构建。
// 迁移不仅建表，还把原有那 10 个名字灌进每个还没有机位的项目——否则升级后
// 导播端会拿到一个空列表，界面上一个按钮都没有，比硬编码还糟。
//
// 幂等：main.go 的 runMigrations 每次启动都遍历全部迁移、不看是否执行过，
// 所以灌数据那步必须先判断「这个项目有没有机位」。
type M20261101000004CreateProjectCamerasTable struct{}

func (m *M20261101000004CreateProjectCamerasTable) Signature() string {
	return "20261101000004_create_project_cameras_table"
}

// defaultCameraNames 与导播端原先硬编码的机位名保持一致，顺序也照抄，
// 这样升级后按钮的位置不会变，现场的人不用重新适应。
var defaultCameraNames = []string{
	"全景", "50米", "100米", "1000米", "20×50接力",
	"跳远", "跳高", "跳长绳", "韵律操", "领导讲话",
}

func (m *M20261101000004CreateProjectCamerasTable) Up() error {
	if !facades.Schema().HasTable("project_cameras") {
		if err := facades.Schema().Create("project_cameras", func(table schema.Blueprint) {
			table.ID()
			table.UnsignedBigInteger("project_id")
			table.String("name", 100)
			table.Integer("sort_order").Default(0)
			table.Timestamps()
			table.Index("project_id")
		}); err != nil {
			return err
		}
		log.Printf("[Migration] 已创建 project_cameras 表")
	}

	if !facades.Schema().HasTable("projects") {
		return nil
	}

	var projects []models.Project
	if err := facades.Orm().Query().Select("id").Find(&projects); err != nil {
		return err
	}

	seeded := 0
	for _, project := range projects {
		count, err := facades.Orm().Query().Model(&models.ProjectCamera{}).
			Where("project_id = ?", project.ID).Count()
		if err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		for i, name := range defaultCameraNames {
			camera := models.ProjectCamera{
				ProjectID: project.ID,
				Name:      name,
				SortOrder: i,
			}
			if err := facades.Orm().Query().Create(&camera); err != nil {
				return err
			}
		}
		seeded++
	}
	if seeded > 0 {
		log.Printf("[Migration] 已为 %d 个项目播下默认机位预设", seeded)
	}
	return nil
}

func (m *M20261101000004CreateProjectCamerasTable) Down() error {
	return facades.Schema().DropIfExists("project_cameras")
}
