package rbac

import (
	"testing"

	"smart-mzcmc/app/models"
)

// BenchmarkView_在线编辑页面每次打开都全量算一遍 用来回答「这页到底多贵」。
//
// 这条基准存在的理由是 View() 是**每次打开页面都跑**的：12 权限 × 8 角色
// 全量现算。任何在它上面加的东西（新的导出、新的告警）都会乘以 96 倍，
// 所以它值得有一个数字，而不是靠感觉说「不慢」。
func BenchmarkView_在线编辑页面(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		view := View()
		if len(view.Permissions) != len(AllPermissions()) {
			b.Fatalf("快照不完整：%d 项权限", len(view.Permissions))
		}
	}
}

// BenchmarkView_并发读 对照组：8 个 goroutine 同时读。
//
// 并发读是**不受写路径影响的**那一半（读路径不碰数据库，也不碰写锁），
// 所以这里单独量一次，用来确认「策略锁只锁写」这个决定没有把读变慢。
func BenchmarkView_并发读(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = View()
		}
	})
}

// BenchmarkHolders_单次403文案的数据源 是 View() 内部最贵的那个原语。
func BenchmarkHolders_单次403文案的数据源(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Holders(PermProjectMember)
	}
}

// BenchmarkPermissionsOf_单角色权限清单。
func BenchmarkPermissionsOf_单角色权限清单(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = PermissionsOf(models.RoleLeader)
	}
}

// BenchmarkCan_每个受守卫请求都会走这里 是全站最热的一行代码。
//
// 之所以要盯它：Can 在改动之前是「读锁 → 取指针 → 放锁 → Enforce」。
// 后来为了让 View 能一次取到整份快照，加了 snapshot 类型；顺手有人把 Can
// 改成「先取完整快照」的话，每格就多一次 warnings 切片的复制，
// 而这条路径上每个受守卫的请求都要跑一遍。这条基准就是为了让那种改动立刻
// 被看出来。
func BenchmarkCan_每个受守卫请求(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Can(models.RoleDirector, PermProjectMember)
	}
}

// BenchmarkCan_放行与拒绝各一半 拒绝路径上多一行日志之外没有别的差别，
// 单独量是为了确认「拒绝」不是因为别的什么才更快。
func BenchmarkCan_拒绝(b *testing.B) {
	silenceB(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Can(models.RoleLogistics, PermUserManage)
	}
}

// BenchmarkReload_整份策略重装 用来衡量写路径的锁持有时间。
//
// 它是唯一会同时碰数据库与构造 Enforcer 的操作，也是写锁临界区的主体部分。
// 这个数字直接决定「别人读策略会不会被写路径卡住」的上界。
func BenchmarkReload_整份策略重装(b *testing.B) {
	silenceB(b)
	s := newMemStoreFromMatrix(mustEmbeddedMatrix(b))
	SetStore(s)
	b.Cleanup(func() { SetStore(nil) })
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := Reload(); err != nil {
			b.Fatalf("重载失败：%v", err)
		}
	}
}
