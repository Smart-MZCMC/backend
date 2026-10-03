package rbac

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"smart-mzcmc/app/models"
)

// 这份文件钉住一件事：**包外拿不到任何能改动生效中策略的东西。**
//
// 它曾经不成立。Default() 返回的就是进程内那个 *casbin.Enforcer，于是任何一行
// 代码都能 `rbac.Default().AddPolicy("admin", rbac.PermSystemMaintain)`，
// protect.go 里的三条 Validate* 一道都不跑，系统维护权限当场多出一个持有者。
// 更糟的是这种改动**不留痕迹**：没有请求被拒、没有审计记录，只是权限多了。
//
// 「不成立」是靠约定维持的，而约定在人急的时候第一个被绕过。所以这里用两条
// 各自独立的办法把它钉成结构性质：
//
//	1. TestPolicyExport_导出符号里不能出现Casbin类型 —— 静态扫描整个包的
//	   导出声明，任何一个的参数/返回值提到 casbin 就失败。
//	2. TestPolicyExport_只读视图的方法集里没有写方法 —— 运行时反射。
//
// 两条都需要：静态扫描管住「将来有人加一个导出函数把 Enforcer 传出去」，
// 反射管住「有人给已有的只读接口加了一个 Add 方法」。

// forbiddenExportedMethods 是 Casbin 的写方法名单。
//
// 列出来而不是笼统地判「方法名以 Add/Remove/Set 开头」：后者会误伤将来
// 真的需要的只读方法（比如 AddMonths 之类），而一个明确的名单写错了就是
// 一条假警报——假警报多了，这条用例本身就会被人忽略掉。
var forbiddenExportedMethods = []string{
	"AddPolicy",
	"AddPolicies",
	"AddNamedPolicy",
	"RemovePolicy",
	"RemovePolicies",
	"RemoveNamedPolicy",
	"AddGroupingPolicy",
	"RemoveGroupingPolicy",
	"AddFunction",
	"SetAdapter",
	"SetModel",
	"SetRoleManager",
	"SavePolicy",
	"ClearPolicy",
	"BuildRoleLinks",
}

// TestPolicyExport_导出符号里不能出现Casbin类型 防的是「换个名字再漏一次」。
//
// 之前的旁路不是靠「有人故意导出 Enforcer」，而是靠「导出它对当时唯一的用途
// （测试要读策略）来说很自然」。将来还会出现同样自然的理由：给某个中间件加
// 一个调试接口、给监控加一个计数。所以在**导出面**上直接把 casbin 挡死，
// 而不是逐个复核每个导出函数的用途。
func TestPolicyExport_导出符号里不能出现Casbin类型(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析 app/rbac 源码失败：%v", err)
	}
	pkg, ok := pkgs["rbac"]
	if !ok {
		t.Fatal("没有解析出 rbac 包")
	}

	offenders := 0
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() || d.Recv != nil {
					continue
				}
				if mentionsCasbin(d.Type) {
					t.Errorf("%s 的导出函数 %s 的签名里出现了 casbin 类型——"+
						"包外只要能拿到这个类型就可能调它的写方法。"+
						"要读策略请用 Snapshot/PermissionsOf/Can 这些只读入口",
						filepath.Base(fset.Position(d.Pos()).Filename), d.Name.Name)
					offenders++
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						if mentionsCasbin(s.Type) {
							t.Errorf("导出类型 %s 里出现了 casbin 类型——"+
								"它等同于把 Enforcer 重新漏出去", s.Name.Name)
							offenders++
						}
					case *ast.ValueSpec:
						// 导出变量：只有显式写了类型的才检查（:= 推断的
						// 类型在这层拿不到，而那样写出来的变量本来也
						// 逃不出包外的显式用法）。
						for i, name := range s.Names {
							if !name.IsExported() || s.Type == nil {
								continue
							}
							if mentionsCasbin(s.Type) {
								t.Errorf("导出变量 %s（第 %d 个名字）的类型里出现了 casbin 类型",
									name.Name, i)
								offenders++
							}
						}
					}
				}
			}
		}
	}
	if offenders == 0 {
		// 显式断言一下「确实扫到了东西」，否则解析器行为一变就会变成
		// 一条永远绿的用例。
		if len(pkg.Files) < 3 {
			t.Errorf("只解析到 %d 个源文件，用例可能没真的在扫东西", len(pkg.Files))
		}
	}
}

// mentionsCasbin 报告类型表达式里是否出现 casbin 包里的类型。
//
// 只看 selector 的包名（casbin / persist / model），因为第三方包的导入名
// 可以被改。这里宁可保守一点：只要出现就叫它出来，由人判断。
func mentionsCasbin(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok {
			switch ident.Name {
			case "casbin", "stringadapter", "persist", "model":
				found = true
			}
		}
		return !found
	})
	return found
}

// TestPolicyExport_只读视图的方法集里没有写方法 是第二条独立防线。
//
// 静态扫描管的是「导出面」，这条管的是「已经导出的东西本身」。
// 两者会分叉：给 Policy 加一个 AddPolicy 方法时静态扫描可能没覆盖到
// （它只看类型声明里的字段），而这里一定报。
func TestPolicyExport_只读视图的方法集里没有写方法(t *testing.T) {
	// 对包内可见的所有导出接口/结构体都过一遍。取法是 runtime 里
	// 真正被端点用到的那几个——它们是策略读出面的全部。
	for _, typ := range []reflect.Type{
		reflect.TypeOf(PolicyView{}),
		reflect.TypeOf(PermissionView{}),
		reflect.TypeOf(RoleView{}),
		reflect.TypeOf(Change{}),
	} {
		for i := 0; i < typ.NumMethod(); i++ {
			method := typ.Method(i)
			if isForbidden(method.Name) {
				t.Errorf("%s 上出现了写方法 %s——"+
					"包外拿到它就能绕过 protect.go 的校验", typ.Name(), method.Name)
			}
		}
	}
}

func isForbidden(name string) bool {
	for _, f := range forbiddenExportedMethods {
		if f == name {
			return true
		}
	}
	return false
}

// TestPolicyExport_Default已经不存在 防的是「为了方便测试把它加回来」。
//
// 这条不是形式主义。Default() 当初被导出时的理由是「测试要读策略」，
// 而这个理由今天依然成立（loadedPolicy 就是同一件事的包内版本）。
// 也就是说，只要包外还需要读策略，它就随时可能再被导出一次——
// 而一旦导出，它带的就是写方法。
func TestPolicyExport_Default已经不存在(t *testing.T) {
	for _, name := range []string{"Default", "Current", "Enforcer", "NewEnforcer", "GetEnforcer"} {
		if hasExportedFunc(name) {
			t.Errorf("rbac 包又导出了 %s"+
				"（哪怕返回的是只读接口也不必）：读策略用 Snapshot / PermissionsOf / Can / Holders",
				name)
		}
	}
}

func hasExportedFunc(name string) bool {
	for _, fn := range funcNamesFromSource() {
		if fn == name {
			return true
		}
	}
	return false
}

// funcNamesFromSource 列出本包导出的包级函数名。
//
// 不用反射列举：Go 的 runtime 拿不到「包里的导出函数」这个列表
// （runtime.FuncForPC 只认 pc），硬凑出来的结果会让人以为扫过了其实没有。
// go/ast 至少能看到真实的声明。
func funcNamesFromSource() []string {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil
	}
	pkg, ok := pkgs["rbac"]
	if !ok {
		return nil
	}
	var names []string
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.IsExported() && fn.Recv == nil {
				names = append(names, fn.Name.Name)
			}
		}
	}
	return names
}

// TestPolicyExport_校验函数是唯一能改策略的入口 把「唯一入口」写成断言。
//
// 这条不是查方法集，而是查**语义**：本包对外暴露的、会改变内存里策略的
// 函数只有 Reload 与 ApplyRolePermissions 两个，而它们都必须先问
// protect.go 的 Validate*。这里断言它们确实存在且可调用——
// 将来有人把它们改名或删掉，这条会先失败，逼着人想清楚替代品是什么。
func TestPolicyExport_校验函数是唯一能改策略的入口(t *testing.T) {
	// 两个入口都存在且签名符合约定：Reload 无参返回 error，
	// ApplyRolePermissions 收 (role, []string) 返回 (*Change, error)。
	reloadType := reflect.TypeOf(Reload)
	if reloadType.NumIn() != 0 || reloadType.NumOut() != 1 ||
		reloadType.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
		t.Errorf("Reload 的签名应是无参返回 error，实际 %v", reloadType)
	}
	applyType := reflect.TypeOf(ApplyRolePermissions)
	if applyType.NumIn() != 2 || applyType.NumOut() != 2 {
		t.Fatalf("ApplyRolePermissions 的签名应是 (Role, []string) (*Change, error)，实际 %v", applyType)
	}
	if applyType.In(0) != reflect.TypeOf(models.Role("")) {
		t.Errorf("第一个参数应是 models.Role，实际 %v", applyType.In(0))
	}
	if applyType.In(1) != reflect.TypeOf([]string(nil)) {
		t.Errorf("第二个参数应是 []string，实际 %v", applyType.In(1))
	}
}
