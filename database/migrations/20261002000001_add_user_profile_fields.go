package migrations

import (
	"log"

	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
	"smart-mzcmc/app/models"
)

// M20261002000001AddUserProfileFields 给 users 表加个人资料与令牌版本两列。
//
// 背景：
//   - email：用户中心用来维护，同时是 WeAvatar 头像的取值依据
//     （https://weavatar.com/avatar/<md5(邮箱)>）。
//   - token_version：改密码时递增，令牌里带上签发时的版本号。验签时与库里的值
//     比对，不一致就判定令牌已失效——这样「改完密码立刻让所有旧令牌作废」
//     才成立，否则最多要等 JWT_TTL（默认 60 分钟）过期。
//
// 唯一索引为什么必须单独建：Goravel 的 ColumnDefinition 没有 Unique()，
// 而 SQLite 的 `ALTER TABLE ADD COLUMN` 也不允许同时附加唯一约束，只能另发一条
// CREATE UNIQUE INDEX。这也是本项目第一次用 facades.DB().Statement() 走原始 SQL。
//
// 允许邮箱为空：SQLite 的 UNIQUE 列可以有多个 NULL（NULL 之间互不相等），
// 所以「不填邮箱」的存量用户与新建账号都不会被约束卡住。
//
// 幂等：main.go 的 runMigrations 每次启动都遍历全部迁移、不看是否执行过，
// 所以三步（建列 email、建列 token_version、建索引）都要各自先判断存在性。
type M20261002000001AddUserProfileFields struct{}

func (m *M20261002000001AddUserProfileFields) Signature() string {
	return "20261002000001_add_user_profile_fields"
}

func (m *M20261002000001AddUserProfileFields) Up() error {
	if !facades.Schema().HasTable("users") {
		return nil
	}

	if !facades.Schema().HasColumn("users", "email") {
		if err := facades.Schema().Table("users", func(table schema.Blueprint) {
			table.String("email", 255).Nullable()
		}); err != nil {
			return err
		}
		log.Printf("[Migration] 已为 users 添加 email 列")
	}

	if !facades.Schema().HasColumn("users", "token_version") {
		if err := facades.Schema().Table("users", func(table schema.Blueprint) {
			table.Integer("token_version").Default(0)
		}); err != nil {
			return err
		}
		log.Printf("[Migration] 已为 users 添加 token_version 列")
	}

	// 用**部分索引**，只约束真正填了邮箱的行。
	//
	// 一开始写的是普通唯一索引，结果第二个用户根本建不出来：models.User.Email
	// 是普通 string（不是 *string），GORM 给未填邮箱的账号插入的是空串 ''，
	// 而唯一索引把两个 '' 判成冲突，于是注册接口报出「用户名已存在」——错误
	// 信息还完全指错了方向，排查时很容易被带偏。
	//
	// 原先的推理「SQLite 的 UNIQUE 列允许多个 NULL」本身没错，但只对真正的
	// NULL 成立，而 ORM 写的是空串。WHERE 子句把两种「没有邮箱」的情形都排除
	// 掉，就不必为了迁就索引把模型字段改成指针类型、到处判 nil。
	if !facades.Schema().HasIndex("users", "users_email_unique") {
		if err := facades.DB().Statement(
			"CREATE UNIQUE INDEX users_email_unique ON users(email)" +
				" WHERE email IS NOT NULL AND email != ''",
		); err != nil {
			return err
		}
		log.Printf("[Migration] 已为 users.email 建立唯一索引（仅约束非空邮箱）")
	}

	// 兜底：把历史上写成非规范形式的邮箱（如带首尾空格或大写）归一化。
	//
	// 不做这一步的话，同一个人用 A@x.com 注册过、又用 a@x.com 改一次邮箱就会
	// 撞唯一约束，报出「该邮箱已被使用」这种让人莫名其妙的结果。
	if err := normalizeEmails(); err != nil {
		return err
	}

	return nil
}

// normalizeEmails 把已有邮箱 trim 并转小写。
func normalizeEmails() error {
	var users []models.User
	if err := facades.Orm().Query().
		Select("id", "email").
		Where("email IS NOT NULL AND email != ''").
		Find(&users); err != nil {
		return err
	}

	changed := 0
	for _, u := range users {
		normalized := models.NormalizeEmail(u.Email)
		if normalized == u.Email {
			continue
		}
		if _, err := facades.Orm().Query().Where("id = ?", u.ID).
			Update(&models.User{Email: normalized}); err != nil {
			// 归一化后可能撞上已存在的同一邮箱（历史脏数据）。
			// 这不该让整个迁移失败——把它留原样、后续在界面上改即可，
			// 总比升级直接失败要好。
			log.Printf("[Migration] 用户 #%d 的邮箱归一化失败，保留原值: %v", u.ID, err)
			continue
		}
		changed++
	}
	if changed > 0 {
		log.Printf("[Migration] 已归一化 %d 个历史邮箱为小写去空格形式", changed)
	}
	return nil
}

func (m *M20261002000001AddUserProfileFields) Down() error {
	// 删列会连带丢掉所有人的邮箱，无法恢复，所以这里什么都不做。
	// 唯一索引倒是可以安全删掉，但留着它对回滚没有坏处。
	return nil
}
