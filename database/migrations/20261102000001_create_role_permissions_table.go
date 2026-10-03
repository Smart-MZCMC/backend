package migrations

import (
	"log"

	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
)

// M20261102000001CreateRolePermissionsTable 建 role_permissions 表：权限策略的
// **运行时**存储。
//
// 背景：权限准入原本只有一份 go:embed 的 policy.csv（app/rbac/policy.csv），
// 现场改不动。这次给它开了在线编辑接口，于是策略有了两个可能的容身之处，
// 必须把归属讲清楚：
//
//   - 首次启动时表为空 → 用 policy.csv **播种一次**（写进表）。
//   - 播种之后，表是唯一事实来源，policy.csv 退化为「播种用的模板 +
//     数据库读不出来时的兜底」。
//
// 播种逻辑刻意**不放在这个迁移里**：迁移只管表结构（DDL），策略内容由
// app/rbac.Bootstrap 负责。理由是「策略是什么」只有 app/rbac 知道，让迁移
// 反向 import 它会把业务判断散进 migrations 目录，而且单元测试也没法在不启
// 动整个框架的前提下覆盖它。
//
// 幂等：main.go 的 runMigrations 每次启动都遍历全部迁移、不看是否执行过，
// 所以建表与建索引各自先判存在性。
type M20261102000001CreateRolePermissionsTable struct{}

func (m *M20261102000001CreateRolePermissionsTable) Signature() string {
	return "20261102000001_create_role_permissions_table"
}

func (m *M20261102000001CreateRolePermissionsTable) Up() error {
	if !facades.Schema().HasTable("role_permissions") {
		if err := facades.Schema().Create("role_permissions", func(table schema.Blueprint) {
			table.ID()
			table.String("role", 20).Default("")
			table.String("permission", 64).Default("")
			table.Boolean("enabled").Default(false)
			table.Timestamps()
			// 唯一约束走部分索引，理由见下面 CREATE UNIQUE INDEX 的注释；
			// 这里先建两个普通索引：读路径是「把整张矩阵拉出来」，不是按单键查。
			table.Index("role")
			table.Index("permission")
		}); err != nil {
			return err
		}
		log.Printf("[Migration] 已创建 role_permissions 表")
	}

	// 唯一索引保证「同一个角色 + 同一项权限」不会出现两行。
	//
	// 没有它的话，一次并发提交（或者一次写了一半的请求重试）会插出两行，
	// 而它们 enabled 可能相反——读出来就是「这一格到底给不给」没有答案。
	// 更麻烦的是它会让「整次拒绝」的承诺失效：调用方以为没写进去，库里却有。
	//
	// 必须用**部分索引**：SQLite 的 UNIQUE 列可以有多个 NULL，但这两列在模型里
	// 是普通 string 而不是 *string，GORM 插入的是空串 ''，两个 '' 会被判成冲突。
	// 于是把「没有角色」与「没有权限」两种空值都排除掉，与 users.email 的
	// 唯一索引同一个理由（见 20261002000001 的说明）。
	if !facades.Schema().HasIndex("role_permissions", "role_permissions_role_permission_unique") {
		if err := facades.DB().Statement(
			"CREATE UNIQUE INDEX role_permissions_role_permission_unique" +
				" ON role_permissions(role, permission)" +
				" WHERE role IS NOT NULL AND role != ''" +
				"   AND permission IS NOT NULL AND permission != ''",
		); err != nil {
			return err
		}
		log.Printf("[Migration] 已为 role_permissions 建立唯一索引（仅约束非空角色与权限）")
	}

	return nil
}

func (m *M20261102000001CreateRolePermissionsTable) Down() error {
	// 表里存的是现场正在生效的策略。回滚会把它删掉，于是所有部署在升级/降级
	// 之后立刻退回 policy.csv——除非有人在降级期间又在线上改过权限。
	// 宁可留着，也不要用一个不可逆的动作去换一次降级。
	return nil
}
