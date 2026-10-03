package rbac

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"smart-mzcmc/app/models"
)

// 这一组用例钉的是**并发**。策略从「进程启动时读一次文件」变成「运行中可整体
// 替换」之后，下面三件事同时成立：每个受守卫的请求都会读它、界面会把它整份
// 读出来渲染、管理员会在另一个 goroutine 上把它整体换掉。
//
// 这里只回答一个问题：**换策略的那一瞬间，会不会有人看到半份。**
// 分成两半分别钉：
//
//   - TestPolicyRuntime_并发读写无数据竞争 —— 让 `go test -race` 证明读路径
//     与写路径之间没有裸指针竞争。
//   - TestView_快照不掺半份策略 —— 让断言证明没有**逻辑上**的半份。
//     这一半 race detector 看不见，而它才是这套系统真正怕的：一次「保存成功」
//     之后界面显示与实际放行对不上，现场会去反复勾选，而真相是它已经生效了。

// flipStore 在两份都合法的矩阵之间交替，让「换策略」这件事一直有事可做。
//
// 为什么需要它：只 Reload 一次的测试里，读写根本撞不上，-race 报不报错都
// 证明不了任何事。这里让每次 Load 都换一代，读侧与写侧于是全程有交叠。
//
// 两份矩阵都过受保护规则核对——否则 Reload 会拒绝装载，测试会悄悄退化成
// 在测 fail-closed，而那已经有别的用例在管了。
type flipStore struct {
	inner *memStore
	off   Matrix
	on    Matrix
	n     atomic.Int64
}

func newFlipStore(off, on Matrix) *flipStore {
	s := &flipStore{inner: newMemStore(), off: off, on: on}
	s.inner.setAll(off)
	return s
}

func (s *flipStore) Load() (Matrix, error) {
	// 刻意先换一代再读：读到的必须是刚写进去的那一代，否则「读侧拿到的是
	// 旧策略」这件事就不成立了，测试会变成在测一个不会撕裂的时序。
	if s.n.Add(1)%2 == 0 {
		s.inner.setAll(s.on)
	} else {
		s.inner.setAll(s.off)
	}
	return s.inner.Load()
}

func (s *flipStore) Seed(m Matrix) error { return s.inner.Seed(m) }

func (s *flipStore) ReplaceRole(role models.Role, granted []string) error {
	return s.inner.ReplaceRole(role, granted)
}

var _ Store = (*flipStore)(nil)

// swapMatrix 返回一份只把导播的某一格翻过来的矩阵。
//
// 刻意只差**一格**而不是换一整套：差得越多，一次 View() 里各字段取自哪一代
// 策略就越难判定，测试就会退化成「跑够多次大概能撞上」。差一格时任何一次
// 撕裂都能被下面那个不变量直接抓到，不靠概率。
//
// 导播的那一格同时满足两个条件：它本来就**没有**这行（所以「翻成有」是一个
// 真实的改动），且与受保护规则无关（所以不会让 Reload 拒绝装载）。
func swapMatrix(t *testing.T, perm string) Matrix {
	t.Helper()
	if Can(models.RoleDirector, perm) {
		t.Fatalf("前置条件不成立：导播本来不该持有 %s", perm)
	}
	m, err := embeddedMatrix()
	if err != nil {
		t.Fatalf("读内嵌策略失败：%v", err)
	}
	m[models.RoleDirector][perm] = true
	return m
}

// viewConsistent 报告一次 View() 是不是自洽的。
//
// 判据是「两个出口必须说同一件事」：界面渲染角色列用的是 PermissionsOf
// （策略里给这个角色记了什么），渲染权限列的持有者用的是 Can（他能不能做）。
// 两者问的应该是同一份策略，答案就必须一致——不一致只有一种解释：这份快照
// 里的字段取自**不同代的策略**。
//
// 这条判据不依赖任何具体数值，所以它对「哪一格被翻了」不敏感，也不需要猜
// 哪一次 View 会撕裂。
func viewConsistent(view PolicyView) (string, bool) {
	grantsOf := map[string]map[string]bool{}
	for _, r := range view.Roles {
		set := map[string]bool{}
		for _, p := range r.Grants {
			set[p] = true
		}
		grantsOf[r.Value] = set
	}
	yes := map[bool]string{true: "有", false: "没有"}
	for _, p := range view.Permissions {
		holders := map[string]bool{}
		for _, h := range p.Holders {
			holders[h] = true
		}
		for role := range grantsOf {
			if holders[role] != grantsOf[role][p.Name] {
				return fmt.Sprintf("权限 %s 的持有者里%s%s，但角色列说%s",
					p.Name, yes[holders[role]], role, yes[grantsOf[role][p.Name]]), false
			}
		}
	}
	return "", true
}

// TestView_快照不掺半份策略 是这一组里最该有的一条。
//
// 场景：管理员 A 正在改策略（PUT 触发一次 Reload），管理员 B 同一时刻打开了
// 权限编辑页（GET /api/rbac/policy）。如果 View() 是一次次分别取 Source()、
// Warnings()、Can()、PermissionsOf()，那么 B 拿到的那份 JSON 里就可能有
// 「source 是旧策略的、grants 是新策略的」这种组合——它没有任何自检能发现，
// 而界面正是照着它渲染、并让管理员据此提交的。
//
// 所以这条不是在测「有没有数据竞争」（那是 -race 的活），而是测**快照是否
// 原子**：一份快照里的每一个字段都必须来自同一代策略。
func TestView_快照不掺半份策略(t *testing.T) {
	silenceLog(t)
	off := swapMatrix(t, PermLogExport)
	on := swapMatrix(t, PermUserView)
	useStore(t, newFlipStore(off, on))
	if err := Reload(); err != nil {
		t.Fatalf("装载第一代策略失败：%v", err)
	}

	var sawOff, sawOn atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 写侧：不停地把两代策略换上去。
	//
	// 收尾挂在 t.Cleanup 上而不是只挂在 happy path 的收尾上：用例中途
	// t.Fatalf 之后，这个 goroutine 若还在 Reload，就会把包级状态改成
	// 「正在翻转的策略」，后面几条用例于是莫名其妙地红——那种失败比
	// 这条用例本身红更费时间排查。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = Reload()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
	})

	// 读侧：不停地把整份快照读出来，并且必须自洽。
	for i := 0; i < 400; i++ {
		view := View()
		if reason, ok := viewConsistent(view); !ok {
			t.Fatalf("第 %d 次读取拿到半份策略：%s"+
				"（source=%q，warnings=%d 条）——View 必须一次性取快照，"+
				"不能一次取一个字段", i, reason, view.Source, len(view.Warnings))
		}
		// 记录这次读到的是哪一代。判据本身与代无关，但这两个计数是防
		// 「这条用例什么都没跑到」的：必须两代都真的被读到过。
		for _, perm := range []string{PermLogExport, PermUserView} {
			if grantsOf(view, models.RoleDirector)[perm] {
				if perm == PermLogExport {
					sawOn.Add(1)
				} else {
					sawOff.Add(1)
				}
			}
		}
	}

	if sawOn.Load() == 0 || sawOff.Load() == 0 {
		t.Fatalf("两代策略都应被读到过（否则撕裂判据没被真正检验）："+
			"读到「导播有 log.export」%d 次、读到「导播有 user.view」%d 次",
			sawOn.Load(), sawOff.Load())
	}
}

// TestPolicyRuntime_并发读写无数据竞争 让 race detector 去证明「没有裸指针竞争」。
//
// 现状是干净的，理由是结构性的而不是巧合：
//
//	Reload 每次都**新构造**一个 *casbin.Enforcer 再整体换指针，换下来的那个
//	从此不再被写；读路径在读锁下取出指针、放开读锁之后再 Enforce——它读的
//	是一个此后只读的对象。
//
// Enforcer 内部唯一会在读时变化的状态是 matcher 表达式的惰性缓存，而它是
// sync.Map；FunctionMap.GetFunctions 返回的是副本。所以第一次并发 Enforce
// 也不会争用。（这两条是 casbin v2.135 的实现细节，升级大版本时要重看。）
//
// 这条用例的作用是把上面那段推理**钉住**：将来有人为了「省一点开销」让
// Enforcer 变成复用可变对象，或者去掉读锁，-race 会立刻报出来。
func TestPolicyRuntime_并发读写无数据竞争(t *testing.T) {
	silenceLog(t)
	off := swapMatrix(t, PermLogExport)
	on := swapMatrix(t, PermUserView)
	useStore(t, newFlipStore(off, on))
	if err := Reload(); err != nil {
		t.Fatalf("装载失败：%v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 写侧一：整体重载。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = Reload()
		}
		close(stop)
	}()

	// 写侧二：走完整写入路径（校验 → 写库 → 重载）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			next := append(PermissionsOf(models.RoleDirector), PermLogExport)
			if i%2 == 0 {
				next = removeString(next, PermLogExport)
			}
			_, _ = ApplyRolePermissions(models.RoleDirector, next)
		}
	}()

	// 读侧：受守卫的请求每个都会走 Can。
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, role := range models.AllRoles() {
					for _, perm := range AllPermissions() {
						_ = Can(role, perm)
					}
				}
			}
		}()
	}

	// 读侧：界面的三个只读视图。它们与 Can 读的是同一份内存策略。
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = View()
				_ = Holders(PermSwitchOperate)
				_ = DeniedMessage(PermProjectMember, models.RoleDirector)
				_ = PermissionsOf(models.RoleLeader)
			}
		}()
	}

	wg.Wait()
}

// TestApplyRolePermissions_并发改动同一个角色时增删与实际生效一致。
//
// 两个管理员同时基于同一份旧快照提交同一个角色。后写的那份赢，这一点本身
// 可以接受（策略不是协同编辑的文档）。**不能接受的是审计说谎**：后写的那次
// 如果按一份已经过期的基线去算 granted/revoked，审计里就会记下与实际发生
// 的事对不上的「谁多了一项能力」，而出事故时要回答的第一个问题正是这个。
//
// 所以这条钉的是：每一次改动算增删时用的基线，必须是**它真正替换掉的那一份**。
//
// 结果是完全确定的：先写的那次看到的是起点，什么都不该被取消；后写的那次
// 看到的是「起点 + 前一次的改动」，于是它必须把前一次加的那一项算成取消。
// 两次都不报告取消，就说明它们算的是同一份旧快照。
func TestApplyRolePermissions_并发改动同一个角色时增删与实际生效一致(t *testing.T) {
	silenceLog(t)
	useStore(t, newMemStore())
	if err := Reload(); err != nil {
		t.Fatalf("播种失败：%v", err)
	}

	base := append([]string(nil), PermissionsOf(models.RoleDirector)...)
	// 两份提交都基于同一个起点，各加一项自己独有的权限。
	own := []string{PermLogExport, PermUserView}
	snapshots := [][]string{
		append(append([]string(nil), base...), own[0]),
		append(append([]string(nil), base...), own[1]),
	}

	changes := make([]*Change, len(own))
	errs := make([]error, len(own))
	var wg sync.WaitGroup
	for i := range own {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			changes[idx], errs[idx] = ApplyRolePermissions(models.RoleDirector, snapshots[idx])
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 次并发改动失败：%v", i, err)
		}
		if len(changes[i].Granted) != 1 || changes[i].Granted[0] != own[i] {
			t.Errorf("第 %d 次应只报告新增 %s，实际 %v", i, own[i], changes[i].Granted)
		}
	}

	// 找出后写的那次：它看到的基线里有前一次的改动。
	var loser int
	winner := -1
	for i := range own {
		if len(changes[i].Revoked) == 1 && changes[i].Revoked[0] == own[1-i] {
			winner = i
			loser = 1 - i
			break
		}
	}
	if winner < 0 {
		t.Fatalf("后写的那次必须把前一次加的 %v 算成取消，实际两次的 revoked 分别是 %v、%v"+
			"——它们算的是同一份旧快照，于是审计里的增删与实际生效对不上",
			own, changes[0].Revoked, changes[1].Revoked)
	}
	if len(changes[loser].Revoked) != 0 {
		t.Errorf("先写的那次基于起点，不该报告任何取消，实际 %v", changes[loser].Revoked)
	}

	// 最终生效的必须恰好是后写那次提交的集合。
	final := PermissionsOf(models.RoleDirector)
	want := map[string]bool{}
	for _, p := range snapshots[winner] {
		want[p] = true
	}
	if len(final) != len(want) {
		t.Fatalf("最终生效的集合应与获胜那次提交一致：提交 %v，生效 %v",
			snapshots[winner], final)
	}
	for _, p := range final {
		if !want[p] {
			t.Fatalf("最终生效的集合应与获胜那次提交一致：提交 %v，生效 %v",
				snapshots[winner], final)
		}
	}
}

// TestStore_未装配存储时写路径不崩 防的是 panic，值得单列。
//
// 存在这样一条路径：Source() 恰好是 database（刚装过存储）而 store 又是 nil
// （装配遗漏），此时 ApplyRolePermissions 若不先确认存储可用，就会一路走到
// currentStore().ReplaceRole —— 一个 nil 接口上的方法调用，直接 panic。
// 写路径必须在**任何**情况下返回一个可判别的错误。
func TestStore_未装配存储时写路径不崩(t *testing.T) {
	silenceLog(t)
	saved := currentStore()
	useStore(t, newMemStore())
	if err := Reload(); err != nil {
		t.Fatalf("播种失败：%v", err)
	}
	// 制造那个危险组合：来源写着 database，存储却是 nil。
	if Source() != SourceDatabase {
		t.Fatalf("前置条件不成立：来源应为 database，实际 %q", Source())
	}
	SetStore(nil)
	t.Cleanup(func() { SetStore(saved) })

	_, err := ApplyRolePermissions(models.RoleDirector, PermissionsOf(models.RoleDirector))
	if err == nil {
		t.Fatal("存储未装配时不应接受权限改动")
	}
	if got := CodeOf(err); got != ErrCodeUnavailable {
		t.Errorf("错误类别应为 %q，实际 %q", ErrCodeUnavailable, got)
	}
}

// grantsOf 从快照里取出某个角色的权限集合。
func grantsOf(view PolicyView, role models.Role) map[string]bool {
	out := map[string]bool{}
	for _, r := range view.Roles {
		if models.Role(r.Value) != role {
			continue
		}
		for _, p := range r.Grants {
			out[p] = true
		}
	}
	return out
}
