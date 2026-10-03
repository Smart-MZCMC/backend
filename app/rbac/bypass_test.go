package rbac

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
//	   导出声明，任何一个的参数/返回值/字段提到 casbin 就失败。
//	2. TestPolicyExport_只读视图的方法集里没有写方法 —— 运行时反射。
//
// 两条都需要：静态扫描管住「将来有人加一个导出函数把 Enforcer 传出去」，
// 反射管住「有人给已有的只读类型加了一个带 casbin 参数的方法」。
//
// ⚠️ 这份扫描曾经有三个**真的会绿**的漏洞，是靠 TestDetector_ 能找出真实的
// 旁路 里的反例钉住的（见那几条用例的说明）：带接收者的方法、函数值形式的
// 导出变量、以及 import 别名。写任何新的静态检查之前，先照着那些反例写一条
// 「检测器自己能不能发现它」的用例——否则下一条漏洞还是绿的。
//
// 已知它**管不到**的两件事，明写出来免得有人误以为它是完备的：
//
//   - 类型别名（`type E = casbin.Enforcer`）：本扫描只看 SelectExpr，
//     而别名藏在 Assign 的右边同一个 SelectExpr 里，所以能发现；
//     但 `func Dangerous() E`（别名当返回类型）发现不了——那需要真正做类型
//     解析（go/types），代价与收益不成比例。
//   - 通过接口把 Enforcer 传出去（`type Leaker interface{ AddPolicy(...)}`，
//     返回一个已实现的 Enforcer）：方法名清单管得住 AddPolicy 这类写方法，
//     但一个恰好叫 `Dangerous` 的接口方法管不住。

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

// casbinImportDefaults 是本包（或将来）可能引入的 casbin 子包，以及它们在
// **没有显式别名**时被 Go 绑定的包名。
//
// 为什么要这张表而不是「看到 casbin 字样就算」：`import c "github.com/casbin/
// casbin/v2"` 之后源码里一个 casbin 字样都没有，只认包名会漏掉它。而反过来，
// 认「路径里含 casbin」又拿不到本地标识符。两者只能一起用。
var casbinImportDefaults = map[string]string{
	"github.com/casbin/casbin/v2":                        "casbin",
	"github.com/casbin/casbin/v2/model":                  "model",
	"github.com/casbin/casbin/v2/persist":                "persist",
	"github.com/casbin/casbin/v2/persist/string-adapter": "stringadapter",
}

// casbinAliases 返回这个文件里所有指向 casbin 的本地包名。
func casbinAliases(file *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		def, isCasbin := casbinImportDefaults[path]
		if !isCasbin {
			// 没在表里也认：只要路径里带 casbin/ 就当成 casbin 家族
			// （将来新增子包时不必改这张表），但本地名仍要靠表或别名。
			if !strings.Contains(path, "casbin/") {
				continue
			}
		}
		if spec.Name != nil {
			out[spec.Name.Name] = true
			continue
		}
		if def != "" {
			out[def] = true
		}
	}
	return out
}

// mentionsCasbin 报告类型表达式里是否出现了 casbin 家族的类型。
//
// aliases 由 casbinAliases 给出——**不能**在这里写死包名，否则
// `import c "github.com/casbin/casbin/v2"` + `func X() *c.Enforcer` 会被漏掉。
func mentionsCasbin(expr ast.Expr, aliases map[string]bool) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && aliases[ident.Name] {
			found = true
		}
		return !found
	})
	return found
}

// leaksInFile 列出这个文件里所有「把 casbin 类型泄露到导出面」的位置。
//
// 覆盖面（每一条都有一个「检测器自己能不能发现它」的用例盯着，见文件末尾）：
//   - 导出函数/方法的**返回值与参数**（带接收者的方法也算——它同样是包外可调用的）
//   - 导出类型（含结构体字段与接口方法签名）里出现的 casbin 类型
//   - 导出变量，且**包括没有显式类型**的那些（`var X = func() *casbin.Enforcer`）
//
// 返回的是给人看的一句话，调用方拿去报错。
func leaksInFile(file *ast.File) []string {
	aliases := casbinAliases(file)
	if len(aliases) == 0 {
		return nil
	}
	var out []string
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			// 带接收者的方法同样算：它在导出面上，包外拿得到值就能调。
			// 旧扫描在这里直接 `if d.Recv != nil { continue }`，于是
			// `func (c *Change) Leak() *casbin.Enforcer` 是绿的。
			if !d.Name.IsExported() {
				continue
			}
			if mentionsCasbin(d.Type, aliases) {
				kind := "函数"
				if d.Recv != nil {
					kind = "方法"
				}
				out = append(out, fmt.Sprintf("导出的%s %s 的签名里出现了 casbin 类型"+
					"——包外只要能拿到这个类型就可能调它的写方法。"+
					"要读策略请用 View / PermissionsOf / Can / Holders 这些只读入口",
					kind, d.Name.Name))
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !s.Name.IsExported() {
						continue
					}
					if mentionsCasbin(s.Type, aliases) {
						out = append(out, fmt.Sprintf("导出类型 %s 里出现了 casbin 类型——"+
							"它等同于把 Enforcer 重新漏出去", s.Name.Name))
					}
				case *ast.ValueSpec:
					for i, name := range s.Names {
						if !name.IsExported() {
							continue
						}
						// 显式写了类型就查类型；没写就查右边的值——
						// `var X = func() *casbin.Enforcer {...}` 与
						// `var X = casbin.NewEnforcer` 都藏在右边那一侧。
						// 早先只查 s.Type，于是这两种写法全部漏过。
						hit := false
						if s.Type != nil {
							hit = mentionsCasbin(s.Type, aliases)
						}
						if !hit && i < len(s.Values) {
							hit = mentionsCasbin(s.Values[i], aliases)
						}
						if hit {
							out = append(out, fmt.Sprintf("导出变量 %s 的类型里出现了 casbin 类型"+
								"——它就是一条能拿到 Enforcer 的旁路", name.Name))
						}
					}
				}
			}
		}
	}
	return out
}

// parsePackageFiles 解析本包的**非测试**源文件。
//
// 解析失败一律 t.Fatal 而不是当空结果：原来 funcNamesFromSource 在解析失败时
// 返回 nil，于是 hasExportedFunc 恒为 false，那条「Default 不许回来」的用例
// 会**一直绿着**——扫描器坏了，坏掉的还是唯一那道防线。
func parsePackageFiles(t *testing.T) (*token.FileSet, map[string]*ast.Package) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatalf("解析 app/rbac 源码失败：%v", err)
	}
	return fset, pkgs
}

// TestPolicyExport_导出符号里不能出现Casbin类型 防的是「换个名字再漏一次」。
//
// 之前的旁路不是靠「有人故意导出 Enforcer」，而是靠「导出它对当时唯一的用途
// （测试要读策略）来说很自然」。将来还会出现同样自然的理由：给某个中间件加
// 一个调试接口、给监控加一个计数。所以在**导出面**上直接把 casbin 挡死，
// 而不是逐个复核每个导出函数的用途。
func TestPolicyExport_导出符号里不能出现Casbin类型(t *testing.T) {
	fset, pkgs := parsePackageFiles(t)
	pkg, ok := pkgs["rbac"]
	if !ok {
		t.Fatal("没有解析出 rbac 包")
	}
	if len(pkg.Files) < 3 {
		t.Errorf("只解析到 %d 个源文件，用例可能没真的在扫东西", len(pkg.Files))
	}

	leaks := 0
	for _, file := range pkg.Files {
		for _, leak := range leaksInFile(file) {
			t.Errorf("%s：%s", filepath.Base(fset.Position(file.Pos()).Filename), leak)
			leaks++
		}
	}
	if leaks == 0 {
		// 反向自检：确保扫描器确实认得 casbin 这家人，而不是因为
		// 「一个 casbin 的本地名都没解析出来」而空转。
		found := false
		for _, file := range pkg.Files {
			if len(casbinAliases(file)) > 0 {
				found = true
			}
		}
		if !found {
			t.Error("没有任何源文件被认出来引入了 casbin——扫描器可能整体失效了")
		}
	}
}

// TestPolicyExport_只读视图的方法集里没有写方法 是第二条独立防线。
//
// 静态扫描管的是「导出面」，这条管的是「已经导出的类型自己的方法集」：
// 反射能看见静态扫描看不见的东西（方法集里所有签名，包括类型别名展开后的），
// 而且它不依赖任何源码解析。
//
// 用例自己枚举**全包**的导出类型，而不是列一张硬编码的表：硬编码的表必然
// 落后，而「忘了把新类型加进表」正是这条用例最该防的那件事。
func TestPolicyExport_只读视图的方法集里没有写方法(t *testing.T) {
	_, pkgs := parsePackageFiles(t)
	pkg, ok := pkgs["rbac"]
	if !ok {
		t.Fatal("没有解析出 rbac 包")
	}

	exported := map[string]bool{}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.IsExported() {
					exported[ts.Name.Name] = true
				}
			}
		}
	}
	if len(exported) < 3 {
		t.Fatalf("只认出 %d 个导出类型（%v），枚举多半失效了", len(exported), keysOf(exported))
	}

	withMethods := 0
	unmapped := []string{}
	for name := range exported {
		// 运行时只认编译进二进制的符号，所以从 ast 拿到的名字要靠一张手工
		// 映射换成反射类型（见 exportedTypeByName）。漏映射必须在这里喊出来，
		// 否则新加的导出类型会静悄悄地不被检查。
		t2, ok := exportedTypeByName(name)
		if !ok {
			unmapped = append(unmapped, name)
			continue
		}
		for i := 0; i < t2.NumMethod(); i++ {
			method := t2.Method(i)
			withMethods++
			if isForbidden(method.Name) {
				t.Errorf("%s 上出现了写方法 %s——"+
					"包外拿到它就能绕过 protect.go 的校验", t2.Name(), method.Name)
			}
			if mentionsCasbinType(method.Type) {
				t.Errorf("%s.%s 的签名里出现了 casbin 类型——"+
					"它等同于把 Enforcer 重新漏出去", t2.Name(), method.Name)
			}
		}
	}
	if len(unmapped) > 0 {
		t.Errorf("这些导出类型没有登记到 exportedTypeByName，方法集不会被检查：%v", unmapped)
	}
	if withMethods == 0 {
		t.Error("一个方法都没枚举到——这条用例很可能什么都没检查")
	}
}

// exportedTypeByName 把导出类型名映射到运行时类型。
//
// 刻意手写而不是用反射去猜：Go 的 runtime 拿不到「包里的类型列表」，
// 硬凑出来的结果会让人以为枚举过了其实没有。新增导出类型时必须在这里补一行——
// 而漏补会被上面那个 withMethods 的计数与下面这条用例抓住。
func exportedTypeByName(name string) (reflect.Type, bool) {
	switch name {
	case "PolicyView":
		return reflect.TypeOf(PolicyView{}), true
	case "PermissionView":
		return reflect.TypeOf(PermissionView{}), true
	case "RoleView":
		return reflect.TypeOf(RoleView{}), true
	case "Change":
		return reflect.TypeOf(Change{}), true
	case "PolicyError":
		return reflect.TypeOf(PolicyError{}), true
	case "PolicyErrorCode":
		return reflect.TypeOf(PolicyErrorCode("")), true
	case "Matrix":
		return reflect.TypeOf(Matrix{}), true
	case "Store":
		return reflect.TypeOf((*Store)(nil)).Elem(), true
	}
	return nil, false
}

// mentionsCasbinType 报告反射拿到的类型里有没有 casbin 的类型。
func mentionsCasbinType(t reflect.Type) bool {
	if t == nil {
		return false
	}
	found := false
	walkType(t, func(x reflect.Type) bool {
		if found {
			return false
		}
		if x.PkgPath() == "github.com/casbin/casbin/v2" {
			found = true
		}
		return !found
	})
	return found
}

func walkType(t reflect.Type, visit func(reflect.Type) bool) bool {
	if !visit(t) {
		return false
	}
	switch t.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Array, reflect.Map, reflect.Chan:
		return walkType(t.Elem(), visit)
	case reflect.Func:
		// 可变参数的最后一个参数在反射里已经是切片，原样遍历即可。
		for i := 0; i < t.NumIn(); i++ {
			if !walkType(t.In(i), visit) {
				return false
			}
		}
		for i := 0; i < t.NumOut(); i++ {
			if !walkType(t.Out(i), visit) {
				return false
			}
		}
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if !walkType(t.Field(i).Type, visit) {
				return false
			}
		}
	case reflect.Interface:
		for i := 0; i < t.NumMethod(); i++ {
			if !walkType(t.Method(i).Type, visit) {
				return false
			}
		}
	}
	return true
}

// TestPolicyExport_Default已经不存在 防的是「为了方便测试把它加回来」。
//
// 这条不是形式主义。Default() 当初被导出时的理由是「测试要读策略」，
// 而这个理由今天依然成立（loadedPolicy 就是同一件事的包内版本）。
// 也就是说，只要包外还需要读策略，它就随时可能再被导出一次——
// 而一旦导出，它带的就是写方法。
func TestPolicyExport_Default已经不存在(t *testing.T) {
	names, err := funcNamesFromSource(t)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(names) == 0 {
		t.Fatal("一个导出函数都没枚举到——这条用例会永远绿着")
	}
	for _, name := range []string{"Default", "Current", "Enforcer", "NewEnforcer", "GetEnforcer"} {
		for _, got := range names {
			if got == name {
				t.Errorf("rbac 包又导出了 %s"+
					"（哪怕返回的是只读接口也不必）：读策略用 View / PermissionsOf / Can / Holders",
					name)
			}
		}
	}
}

// funcNamesFromSource 列出本包导出的包级函数名。
//
// 不用反射列举：Go 的 runtime 拿不到「包里的导出函数」这个列表
// （runtime.FuncForPC 只认 pc），硬凑出来的结果会让人以为扫过了其实没有。
// go/ast 至少能看到真实的声明。
//
// 解析失败**必须**冒出来。原来的写法在失败时返回 nil，于是那条
// 「Default 不许回来」的用例会安静地一直绿。
func funcNamesFromSource(t *testing.T) ([]string, error) {
	t.Helper()
	_, pkgs := parsePackageFiles(t)
	pkg, ok := pkgs["rbac"]
	if !ok {
		return nil, errors.New("没有解析出 rbac 包")
	}
	var names []string
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.IsExported() && fn.Recv == nil {
				names = append(names, fn.Name.Name)
			}
		}
	}
	return names, nil
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

// ############################################################################
// # 下面这几条是**检测器自己的**用例。
// #
// # 它们存在的唯一理由：bypass_test.go 上半部分的静态检查曾有过三个**真的会绿**
// # 的漏洞——扫描只覆盖包级函数、只查显式类型、只认包名。每一个漏洞都是加进去
// # 之后跑一遍发现「绿的」，才补上对应的检测代码与这里的反例。
// #
// # 没有它们，「静态扫描」这四个字只是一句自我评价：扫描器自己坏了（比如
// # ast 结构改版、解析器行为变化）时，测试同样会绿。
// ############################################################################

// TestDetector_能找出带接收者的旁路 对应漏洞一。
//
// 反例长这样，它在旧扫描下是**绿的**（旧扫描对 d.Recv != nil 直接 continue）：
//
//	func (c *Change) Leak() *casbin.Enforcer { ... }
//
// 为什么它危险：包外拿到 Change（它是导出类型，也是 ApplyRolePermissions 的返回值）
// 就能调这个方法，于是 Enforcer 又回到了包外，AddPolicy 一路畅通。
func TestDetector_能找出带接收者的旁路(t *testing.T) {
	src := "package rbac\n" +
		"import \"github.com/casbin/casbin/v2\"\n" +
		"type Change struct{}\n" +
		"func (c *Change) Leak() *casbin.Enforcer { return nil }\n"
	if leaks := leaksInSynthetic(t, src); len(leaks) != 1 {
		t.Errorf("带接收者的旁路应被查出 1 处，实际 %d 处：%v", len(leaks), leaks)
	}
}

// TestDetector_能找出函数值形式的旁路 对应漏洞二。
//
// 反例：`var DangerousVar = func() *casbin.Enforcer {...}` 与
// `var DangerousRef = casbin.NewEnforcer`。旧扫描只查 ValueSpec 的 Type，
// 而这两种写法 Type 都是 nil，于是全绿。
func TestDetector_能找出函数值形式的旁路(t *testing.T) {
	src := "package rbac\n" +
		"import \"github.com/casbin/casbin/v2\"\n" +
		"var DangerousVar = func() *casbin.Enforcer { return nil }\n" +
		"var DangerousRef = casbin.NewEnforcer\n" +
		"var harmless = func() int { return 0 }\n"
	leaks := leaksInSynthetic(t, src)
	if len(leaks) != 2 {
		t.Errorf("函数值形式的旁路应被查出 2 处，实际 %d 处：%v", len(leaks), leaks)
	}
}

// TestDetector_能认出import别名 对应漏洞三。
//
// 反例：`import c "github.com/casbin/casbin/v2"` 之后源码里一个 casbin
// 字样都没有，只认包名的扫描会漏掉它。
func TestDetector_能认出import别名(t *testing.T) {
	src := "package rbac\n" +
		"import c \"github.com/casbin/casbin/v2\"\n" +
		"func Dangerous() *c.Enforcer { return nil }\n"
	if leaks := leaksInSynthetic(t, src); len(leaks) != 1 {
		t.Errorf("import 别名下的旁路应被查出 1 处，实际 %d 处：%v", len(leaks), leaks)
	}
}

// TestDetector_干净源码不会被误报 防的是假警报。
//
// 假警报与漏报同样致命：一条永远红的用例会被加 //nolint 或者干脆删掉，
// 于是连它本来能挡住的东西也一起没了。
func TestDetector_干净源码不会被误报(t *testing.T) {
	src := "package rbac\n" +
		"import (\n\t\"log\"\n\t\"sync\"\n\n\t\"smart-mzcmc/app/models\"\n)\n" +
		"type Matrix map[models.Role]map[string]bool\n" +
		"type Store interface{ Load() (Matrix, error) }\n" +
		"var register struct{ mu sync.Mutex }\n" +
		"var names = []string{\"a\", \"b\"}\n" +
		"func Can(role models.Role, perm string) bool { log.Print(perm); return role.Valid() }\n" +
		"func (m Matrix) Count() int { return len(m) }\n" +
		"func unexported() *casbin.Enforcer { return nil }\n"
	if leaks := leaksInSynthetic(t, src); len(leaks) != 0 {
		t.Errorf("干净的源码不该被报出旁路，实际 %v", leaks)
	}
}

// leaksInSynthetic 把一段源码当成本包的一个文件跑一遍扫描。
//
// 走的是与真实用例**完全相同**的那段代码——只有源码来源不同。这样「反例」
// 与「检测器」之间不会出现「测试里另写了一份弱化版的检查」这种假阳性来源。
func leaksInSynthetic(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("解析合成源码失败：%v\n%s", err, src)
	}
	return leaksInFile(file)
}

func isForbidden(name string) bool {
	for _, f := range forbiddenExportedMethods {
		if f == name {
			return true
		}
	}
	return false
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
