package bootstrap

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"smart-mzcmc/app/facades"
)

// 迁移锁的三个时间参数。
//
// 数字来自实测：16 条迁移跑在一个全新库上约 2 秒，跑在已迁移的库上不足 1 秒。
// 所以「连续 60 秒还没有人把锁让出来」只可能是持有者已经死了（被 kill、断电、
// OOM），而不是它还在慢慢跑。留 30 倍余量是为了不给慢盘、杀毒软件全盘扫描、
// 网络盘之类的情况制造假阳性。
const (
	migrateLockStale   = 60 * time.Second // 连续等这么久仍拿不到，判定为残留锁并接管
	migrateLockGiveUp  = 120 * time.Second
	migrateLockPollGap = 100 * time.Millisecond
)

// runLock 挡的是同一个进程内的并发调用：初始化向导能被连点，而它自己也会跑
// 迁移（app/setup.RunMigrations）。跨进程那一层由迁移锁文件负责。
var runLock = make(chan struct{}, 1)

// acquireMigrationLock 保证同一时刻只有一个进程在跑这批迁移。
//
// 为什么需要它：每条迁移都是「先 HasTable 判断、再 Create」，这两步之间没有
// 原子性。两个实例同时启动时（systemd 配重了单元、有人在旁边手动起了一个
// 二进制、容器编排起了两个副本），两边会同时看到「表不存在」，然后同时 Create，
// 后到的那个拿到 `table "users" already exists`，RunMigrations 返回错误，
// WithCallback 里 log.Fatalf —— **一个误启动的副本把正常服务干掉了**。
// 实测：两个进程同时对空库跑迁移，6 次里 6 次有一方 Fatalf 退出。
//
// 光让 DDL 认下这个错还不够：20261101000004_create_project_cameras_table 是
// 「查这个项目的机位数，为 0 就灌 10 个默认机位」，两个进程同时判定为 0 就会
// 灌出 20 个，机位按钮在导播端直接翻倍，而且两边退出码都是 0、日志里一句异常
// 都没有。所以只能靠互斥，不能靠「把错误当成功」。
//
// 这里用的是「O_EXCL 建锁文件 + 轮询等待」而不是内核文件锁（flock /
// LockFileEx）：后者在标准库里拿不到，要用就得把已经躺在 go.mod 里的
// golang.org/x/sys 提成直接依赖。代价是持有者被 kill -9 时锁文件会残留，
// 所以必须有 stale 判定兜底——见 migrateLockStale 的说明。
//
// 拿不到锁时的选择是「吵醒然后照常跑」，而不是拒绝启动：
//   - 迁移本身全部幂等，等锁只是为了在绝大多数情况下不撞车，不是正确性的前提；
//   - 等待超时意味着「要么对方卡死，要么锁是残留的」，此时拒绝启动只会让现场
//     变成「服务起不来 + 日志里只有一句拿不到锁」，比照常跑更难查。
func acquireMigrationLock() func() {
	runLock <- struct{}{}

	lockPath, ok := migrateLockPath()
	if !ok {
		// 不是 SQLite（路径为空）。这属于「少一道保险」，不属于「不能启动」。
		log.Printf("[Migrate] 未配置 SQLite 路径，跳过迁移锁")
		return func() { <-runLock }
	}
	// 三条分支都要留日志：这几行只在「有人同时在迁移」时才出现，而它们恰恰是
	// 「启动卡了十几秒」这类现场唯一的线索。走「等到了」那一支时也必须说一句，
	// 否则日志里只有「另一个进程正在执行迁移」却再无下文，看着像卡死了。
	switch {
	case tryCreateLock(lockPath):
		log.Printf("[Migrate] 已获得迁移锁 %s", lockPath)
	case takeMigrationLock(lockPath, migrateLockGiveUp):
		log.Printf("[Migrate] 迁移锁 %s 曾被别的进程占用，已等到它释放", lockPath)
	default:
		log.Printf("[Migrate] 等待迁移锁 %s 超过 %s 仍未拿到，继续执行迁移（迁移幂等，"+
			"最坏情况是与对方撞上一次建表冲突）", lockPath, migrateLockGiveUp)
	}
	return func() {
		os.Remove(lockPath)
		<-runLock
	}
}

// takeMigrationLock 尝试独占建出锁文件；wait>0 时轮询等待，直到拿到或超时。
// 返回是否拿到了锁。
func takeMigrationLock(lockPath string, wait time.Duration) bool {
	if tryCreateLock(lockPath) {
		return true
	}
	if wait == 0 {
		return false
	}

	waited := time.Duration(0)
	// 到点仍然没人让出，就把它当成残留锁删掉再抢一次。
	for waited < wait {
		time.Sleep(migrateLockPollGap)
		waited += migrateLockPollGap
		if tryCreateLock(lockPath) {
			return true
		}
		// 必须真的删。只喊一句「接管」然后再来一次 O_EXCL 是空操作——文件还在，
		// create 照样失败，于是每次判定都白判一次、再白等一个 stale 周期。
		if waited >= migrateLockStale && dropStaleLock(lockPath, waited) {
			log.Printf("[Migrate] 迁移锁 %s 已存在 %s 且无人释放，判定为残留锁"+
				"（持有者多半已被 kill），删除后接管。", lockPath, waited.Round(time.Second))
			if tryCreateLock(lockPath) {
				return true
			}
		}
	}
	return false
}

// dropStaleLock 删掉一个确认无人持有的残留锁文件，返回是否删掉了。
//
// 删之前重新 stat 一次：判定与删除之间隔着一个 PollGap，中间可能有人正常释放了
// 锁并让另一个人重新建起来，这时候 mtime 是新的、自然就删不掉。
//
// age >= waited 这条不是多余的：文件比我们的等待时间还年轻，说明它是别人刚刚
// 建起来的、对方真的在干活，这时候抢锁才是误判。反过来，文件越老越可能是
// 上一任留下的，因此「越老越敢抢」。
func dropStaleLock(lockPath string, waited time.Duration) bool {
	info, err := os.Stat(lockPath)
	if err != nil {
		return false
	}
	age := time.Since(info.ModTime())
	if age < migrateLockStale || age < waited {
		return false
	}
	return os.Remove(lockPath) == nil
}

func tryCreateLock(lockPath string) bool {
	// O_EXCL 是这一整套机制里唯一的原子操作：内核保证「不存在」与「建出来」
	// 之间没有缝，两个进程同时调只有一个能成功。
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	// 内容只为了让人手工排查时看得出这是谁留下的。判活不靠它——跨平台查进程
	// 存活没法只用标准库，靠的是 mtime + migrateLockStale。
	fmt.Fprintf(f, "pid=%d started=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	_ = f.Close()
	return true
}

// migrateLockPath 返回迁移锁文件路径，ok=false 表示这套机制用不上。
//
// 锁文件紧挨数据库文件（`<db>.migrate.lock`）而不是放在系统临时目录：临时目录
// 全机共享，同一台机器上两个**不同的**部署会被同一把锁卡住；只有紧挨数据库
// 才能保证「同一个库 = 同一把锁」。
func migrateLockPath() (string, bool) {
	path := facades.Config().GetString("database.connections.sqlite.database")
	if path == "" {
		return "", false
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			// 建不出目录等于锁文件也放不下，如实说出来，后面会走「不加锁照跑」。
			log.Printf("[Migrate] 创建锁文件所在目录 %s 失败: %v", dir, err)
			return "", false
		}
	}
	return path + ".migrate.lock", true
}
