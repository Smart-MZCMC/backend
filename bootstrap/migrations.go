package bootstrap

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/goravel/framework/contracts/database/schema"

	"smart-mzcmc/app/facades"
	"smart-mzcmc/database/migrations"
)

// RunMigrations 依次执行所有已注册迁移。
//
// 每次启动都跑，不看是否执行过——所有迁移都写成幂等的（建表前判 HasTable、
// 加字段前判 HasColumn、数据清理用 DELETE），重复执行安全。
//
// 整批迁移外面套一把跨进程的互斥锁：迁移本身幂等，但「判断存在性」与「建出来」
// 之间不是原子的，两个实例同时启动会撞车并让其中一个拒绝启动。理由与实测见
// bootstrap/lock.go 的 acquireMigrationLock。
//
// 放在 bootstrap 而不是 main：ORM 在 Build() 末尾才装配好，而自动迁移必须
// 紧接着它跑（见 app.go 的 WithCallback），那时候 main 里的位置都已经过去了。
func RunMigrations() error {
	if err := ensureDatabaseDir(); err != nil {
		return err
	}

	release := acquireMigrationLock()
	defer release()

	for _, m := range Migrations() {
		log.Printf("[Migrate] 执行: %s", m.Signature())
		if err := m.Up(); err != nil {
			return &MigrationError{Signature: m.Signature(), Err: err}
		}
	}
	return nil
}

// MigrationError 指明「是哪一条迁移失败了」。
//
// 为什么要单独一个类型而不是 fmt.Errorf 拼字符串：自动迁移失败之后服务拒绝
// 启动，而启动失败时人在日志里能看到的只有这一条消息。带着签名回去，调用方才能
// 把「哪条迁移」「原始错误」「接下来该干什么」分成三行讲清楚，而不是一长句里
// 让人自己找。Error() 的文本与之前完全一致，老的日志解析不受影响。
type MigrationError struct {
	Signature string
	Err       error
}

func (e *MigrationError) Error() string {
	return fmt.Sprintf("%s: %v", e.Signature, e.Err)
}

func (e *MigrationError) Unwrap() error { return e.Err }

// ensureDatabaseDir 建出 SQLite 文件所在的目录。
//
// 为什么不靠 main.ensureRuntimeDirs：那个函数在 main 包里，而任何嵌入
// bootstrap.Boot() 的调用方（测试、将来的运维工具）都不会经过 main，于是
// SQLite 会报一句 "unable to open database file"——那句话里没有「目录不存在」
// 这几个字，排查的人通常会先去查文件权限，而实际原因只是父目录压根没建。
//
// 目录建不出来不直接返回错误：那属于「磁盘满 / 路径不可写」，而下面第一条
// 迁移会给出更具体的失败信息，这里再报一次只会把真正的原因往后挤。
func ensureDatabaseDir() error {
	path := facades.Config().GetString("database.connections.sqlite.database")
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[Migrate] 创建数据库目录 %s 失败（交给下面的迁移报错）: %v", dir, err)
	}
	return nil
}

func Migrations() []schema.Migration {
	return []schema.Migration{
		// 保留原有的 jobs 表
		&migrations.M20210101000001CreateJobsTable{},
		// 核心业务表
		&migrations.M20260901000001CreateUsersTable{},
		&migrations.M20260901000002CreateProjectsTable{},
		&migrations.M20260901000003CreateUserProjectsTable{},
		&migrations.M20260901000004CreateProjectLocksTable{},
		&migrations.M20260901000005CreateInterviewStatusTable{},
		&migrations.M20260901000006CreateMessagesTable{},
		// 数据清理
		&migrations.M20260926000001PurgeHeartbeatMessages{},
		// 引入超级管理员后，为存量部署补一个，否则没人能授予该角色
		&migrations.M20261001000001EnsureSuperAdmin{},
		// 用户中心：邮箱（WeAvatar 头像来源）与令牌版本（改密码即失效）
		&migrations.M20261002000001AddUserProfileFields{},
		// 切台状态落库：让中途连上来的解说端/包装端能立刻拿到当前状态
		&migrations.M20261101000001CreateProjectStatesTable{},
		// 切台流水：时间线、机位/时段筛选、次数与停留时长报表
		&migrations.M20261101000002CreateShotCutsTable{},
		// 项目日程与状态流转字段
		&migrations.M20261101000003AddProjectScheduleFields{},
		// 机位预设由项目自己配置，不再硬编码在导播端
		&migrations.M20261101000004CreateProjectCamerasTable{},
		// 敏感操作审计落库，不再只依赖 7 天轮转的 stdout 日志
		&migrations.M20261101000005CreateAuditLogsTable{},
		// 把 v1.4.0 / v1.4.1 期间写入的本地时区时间戳统一成 UTC。
		// 写入侧早在 v1.4.2 就改成 time.Now().UTC()，但历史行还留在列里，
		// 而审计页的日期筛选走的是字符串比较——不归一就表现为「明明有记录，
		// 页面却是空的」。这条迁移写完之后一直没被注册，等于从未运行过。
		&migrations.M20261101000006NormalizeAuditCreatedAt{},
		// 权限策略的运行时存储：播种一次之后表即唯一事实来源，policy.csv 退为兜底
		&migrations.M20261102000001CreateRolePermissionsTable{},
	}
}
