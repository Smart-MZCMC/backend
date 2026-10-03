package rbac

import (
	"fmt"
	"log"

	"github.com/goravel/framework/contracts/database/orm"

	"smart-mzcmc/app/facades"
	"smart-mzcmc/app/models"
)

// Matrix 是「角色 → 权限 → 是否授予」的完整矩阵。
//
// 存完整矩阵而不是「只存被授予的行」是有意的（见 models.RolePermission 的
// 说明）。读出来时**两个方向都要有值**：某个角色没出现在矩阵里，说明这一行
// 数据没写过，与「写了但 enabled=false」必须能区分开——前者是异常，
// 后者是一个明确的「不给」。
type Matrix map[models.Role]map[string]bool

// Store 是策略矩阵的持久化口。
//
// 抽成接口只有一个理由：app/rbac 需要的 IO 就这三件事，把它写成接口之后，
// 「校验 → 写库 → 重载 → 重载不通过就回滚」这条链可以在没有数据库的情况下
// 被完整测一遍。本仓库没有数据库测试脚手架（tests/test_case.go 那套会 Boot
// 整个应用，并在仓库里留下真实的 database/smart-mzcmc.db，AGENTS.md 明确禁止），
// 而这一条链恰恰是最不能靠人工点一遍就算测过的——它决定的是「系统会不会被锁死」。
//
// 实现只有一个（ormStore），接口本身不导出任何 Casbin 类型，因此它不能被
// 拿去绕过 protect.go 的校验。
type Store interface {
	// Load 读出整张矩阵。表不存在时返回错误，调用方据此退回 embedded。
	Load() (Matrix, error)
	// Seed 用给定的矩阵整表重写一遍（事务）。
	Seed(Matrix) error
	// ReplaceRole 把某个角色的权限集合整体改成 granted 列出的那些。
	// 语义是「集合改成这样」，不是「追加」——所以内部是事务里先删后插。
	ReplaceRole(role models.Role, granted []string) error
}

// store 是进程内生效的存储实现，由 Bootstrap 装配。
//
// nil 表示「还没接数据库」，此时所有读操作走 embedded policy.csv，
// 写操作直接报错（见 Reload 与 ApplyRolePermissions）。
var store Store

// SetStore 装配策略存储。
//
// 导出是因为装配点在本包之外（bootstrap/app.go 的 WithCallback），而测试
// 需要注入一个假实现。它**不是**一条写入旁路：它换掉的是「策略存在哪」，
// 不是「能不能绕过 Validate*」——换进来什么，就仍然只能通过 Reload /
// ApplyRolePermissions 这两个出口改动策略，那两个出口都会先过校验。
func SetStore(s Store) {
	store = s
}

func currentStore() Store {
	return store
}

// rolePermissionsTable 是表名的单一来源。
//
// 迁移里也写着这个字面量（那边不能 import app/rbac，否则会成环）。两处
// 分叉的后果是「代码写进 A 表、迁移建 B 表」，而它只在播种那一刻才暴露
// ——表现是策略永远退回 embedded，且日志里只有一句「no such table」。
// app/rbac/store_test.go 的 TestStore_迁移与代码用的是同一个表名 会盯住这一侧。
const rolePermissionsTable = "role_permissions"

// ormStore 是唯一实现：读写 role_permissions 表。
type ormStore struct{}

// ormStore 满足 Store。
var _ Store = ormStore{}

// DefaultStore 返回基于 ORM 的策略存储。
func DefaultStore() Store { return ormStore{} }

func (ormStore) Load() (Matrix, error) {
	var rows []models.RolePermission
	// Model 必须显式给：GORAVEL 的 ORM 依赖模型元数据解析表名，靠 dest
	// 推断在某些路径上会失败（表现是「记录不存在」，见 AGENTS.md 陷阱第 3 条）。
	if err := facades.Orm().Query().Model(&models.RolePermission{}).Find(&rows); err != nil {
		return nil, err
	}
	matrix := Matrix{}
	for _, row := range rows {
		role := models.Role(row.Role)
		cells, ok := matrix[role]
		if !ok {
			cells = map[string]bool{}
			matrix[role] = cells
		}
		cells[row.Permission] = row.Enabled
	}
	return matrix, nil
}

func (ormStore) Seed(matrix Matrix) error {
	return writeAll(matrix)
}

func (ormStore) ReplaceRole(role models.Role, granted []string) error {
	want := make(map[string]bool, len(granted))
	for _, perm := range granted {
		want[perm] = true
	}
	return replaceRole(role, want)
}

// writeAll 整表重写一次矩阵。
//
// 走事务不是为了「快」，是为了「要么全在要么全不在」：写到一半失败留下半张
// 矩阵的话，缺掉的那几行读出来就是「没授予」，而现场没有任何东西能说明
// 刚才那次播种进行到哪儿了。
func writeAll(matrix Matrix) error {
	return facades.Orm().Transaction(func(tx orm.Query) error {
		// 走原始 SQL 而不是 tx.Delete：Goravel 的 Delete 强制要求带 WHERE
		// （没有就报 "WHERE conditions required"），而「清空整张表」正是
		// 一个不带 WHERE 的操作。这不是绕过什么保护——播种时表必然是空的，
		// 而且它在一个事务里，前面任何失败都会让整个事务回滚。
		if _, err := tx.Exec("DELETE FROM " + rolePermissionsTable); err != nil {
			return err
		}
		for _, role := range models.AllRoles() {
			if err := writeRole(tx, role, matrix[role]); err != nil {
				return err
			}
		}
		return nil
	})
}

// replaceRole 只重写一个角色，其余行一个字节都不动。
//
// 逐角色写而不是整表重写，是为了让「改一个人的权限」不会因为并发而把别人
// 正在改的那部分一起抹掉。
func replaceRole(role models.Role, want map[string]bool) error {
	return facades.Orm().Transaction(func(tx orm.Query) error {
		if _, err := tx.Model(&models.RolePermission{}).
			Where("role = ?", string(role)).
			Delete(&models.RolePermission{}); err != nil {
			return err
		}
		return writeRole(tx, role, want)
	})
}

// writeRole 把一个角色的 12 格全部写出来（授予与不授予都写）。
func writeRole(tx orm.Query, role models.Role, want map[string]bool) error {
	for _, perm := range AllPermissions() {
		row := models.RolePermission{
			Role:       string(role),
			Permission: perm,
			Enabled:    want[perm],
		}
		if err := tx.Create(&row); err != nil {
			return fmt.Errorf("写入 %s/%s：%w", role, perm, err)
		}
	}
	return nil
}

// seedIfEmpty 在矩阵为空时用 policy.csv 播种一次。
//
// 「空表」只有一种解释：这张表从来没被初始化过。它不可能是「故意不放任何
// 权限」——那种状态一旦生效，全员 403，而它只能靠手改数据库才能走出去。
// 与其让服务带着一个必然锁死的状态起来，不如把 policy.csv 当成模板补上，
// 并在日志与 GET /api/rbac/policy 的 warnings 里说清「表是刚播种的」。
func seedIfEmpty(s Store, matrix Matrix) (Matrix, error) {
	if len(matrix) > 0 {
		return matrix, nil
	}
	seeded, err := embeddedMatrix()
	if err != nil {
		return nil, fmt.Errorf("内嵌策略不可用，无法播种：%w", err)
	}
	log.Printf("[RBAC] role_permissions 为空，用内嵌 policy.csv 播种 %d 个角色的权限矩阵",
		len(models.AllRoles()))
	if err := s.Seed(seeded); err != nil {
		return nil, fmt.Errorf("播种策略矩阵：%w", err)
	}
	return s.Load()
}
