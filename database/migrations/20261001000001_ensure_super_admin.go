package migrations

import (
	"fmt"
	"log"

	"smart-mzcmc/app/facades"
	"smart-mzcmc/app/models"
)

// M20261001000001EnsureSuperAdmin 保证系统里至少存在一个超级管理员。
//
// 为什么必须有这一步：引入 super_admin 后，只有超级管理员能授予超级管理员
// 角色（见 controllers/authz.go 的 guardGrant）。而全新部署的第一个账号由
// 引导流程直接建成超级管理员，所以没问题；但**存量部署**里第一个账号当初
// 建的是 admin。
//
// 结果是升级后一个超级管理员都没有：没人能授予超管、没人能做系统更新、
// 没人能改角色——系统停在「谁也管不了谁」的状态，只能回到服务器手改数据库。
//
// 做法：如果当前没有超级管理员，就把最早创建的那个 admin 升上去。
// 已经有超管时什么都不做，所以可以反复执行（main.go 的 runMigrations
// 每次启动都会遍历全部迁移，不看是否已执行过）。
type M20261001000001EnsureSuperAdmin struct{}

func (m *M20261001000001EnsureSuperAdmin) Signature() string {
	return "20261001000001_ensure_super_admin"
}

func (m *M20261001000001EnsureSuperAdmin) Up() error {
	if !facades.Schema().HasTable("users") {
		return nil
	}

	superCount, err := facades.Orm().Query().Model(&models.User{}).
		Where("role = ?", string(models.RoleSuperAdmin)).Count()
	if err != nil {
		return err
	}
	if superCount > 0 {
		return nil
	}

	// 必须先用 Count 确认存在管理员，再取第一条。
	//
	// 不能直接靠 First 返回的 error 判断「有没有」：First 在结果为空时是否
	// 返回 ErrRecordNotFound 取决于驱动实现，不可靠。这里实测就是空表上
	// First 不报错，于是拿着零值 User（id=0）继续往下走，UPDATE 影响 0 行，
	// 却打印出「已将最早的管理员(#0) 升为超级管理员」——一句彻头彻尾的假消息。
	adminCount, err := facades.Orm().Query().Model(&models.User{}).
		Where("role = ?", string(models.RoleAdmin)).Count()
	if err != nil {
		return err
	}
	if adminCount == 0 {
		// 一个管理员都没有。全新部署属于正常情况：第一个账号会由引导流程
		// 直接建成超级管理员。
		return nil
	}

	var target models.User
	if err := facades.Orm().Query().
		Where("role = ?", string(models.RoleAdmin)).
		Order("id").
		First(&target); err != nil {
		return err
	}
	if target.ID == 0 {
		return fmt.Errorf("查到管理员但 id 为 0，请检查 users 表")
	}

	result, err := facades.Orm().Query().Where("id = ?", target.ID).
		Update(&models.User{Role: string(models.RoleSuperAdmin)})
	if err != nil {
		return err
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("升级 %s(#%d) 时没有更新任何行", target.Username, target.ID)
	}

	log.Printf("[Migration] 系统原本没有超级管理员，已将最早的管理员 %s(#%d) 升为超级管理员。"+
		"请尽快在管理后台指定其他超级管理员。", target.Username, target.ID)
	return nil
}

func (m *M20261001000001EnsureSuperAdmin) Down() error {
	// 降级会把管理员变成超级管理员，这个方向没法自动撤销：
	// 不知道该把哪一个降回去，也没有「降权」这个安全动作。
	// 交给人在后台调整。
	return nil
}
