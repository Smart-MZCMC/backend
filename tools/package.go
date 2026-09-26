// 打包 Linux amd64 发布包。用法: go run tools/package.go
// 产物: ../dist/backend-linux-amd64.tar.gz
package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	pkgName   = "backend-linux-amd64"
	binName   = "smart-mzcmc"
	binaryOut = "smart-mzcmc-linux-amd64"
)

// execFiles 需要 0755 权限的文件（相对发布包根目录）。
var execFiles = map[string]bool{
	binName:    true,
	"start.sh": true,
}

var t0 = time.Now()

func log(format string, args ...any) {
	fmt.Printf("  [%6.1fs] %s\n", time.Since(t0).Seconds(), fmt.Sprintf(format, args...))
}

func main() {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		// 交叉编译本身在 Windows/macOS 上跑；Linux 上直接构建即可
		log("当前系统 %s，将直接构建", runtime.GOOS)
	}

	dist := filepath.Join("..", "dist")
	stage := filepath.Join(dist, pkgName)

	// ---- 1. 清理并创建暂存目录 ----
	// 先确认没有进程占用旧暂存目录。Windows 上被占用的目录删不掉，
	// 报错信息很难看懂，这里主动挡一下并给出可操作的提示。
	if _, err := os.Stat(filepath.Join(stage, "database")); err == nil {
		if _, err := os.Stat(filepath.Join(stage, "database", "smart-mzcmc.db")); err == nil {
			fatal("暂存目录 %s 里存在真实数据库文件。\n"+
				"这说明之前有人在这个目录里跑过后端。它绝不能被打进发布包——\n"+
				"部署时会覆盖服务器上的数据。\n"+
				"请先停掉占用它的进程，再执行 go run ./tools", stage)
		}
	}
	must(os.RemoveAll(stage))
	must(os.RemoveAll(filepath.Join(dist, binaryOut)))
	must(os.MkdirAll(stage, 0o755))
	must(os.MkdirAll(dist, 0o755))
	log("暂存目录: %s", filepath.Clean(stage))

	// ---- 2. 交叉编译 ----
	log("交叉编译 linux/amd64 (CGO_ENABLED=0)...")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w",
		"-o", filepath.Join(dist, binaryOut), ".")
	cmd.Env = append(os.Environ(),
		"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("交叉编译失败: %v", err)
	}
	assertELF(filepath.Join(dist, binaryOut))
	log("编译完成: %s (%.1f MB)", binaryOut, mb(filepath.Join(dist, binaryOut)))

	// ---- 3. 组装发布包 ----
	// 只放运行期必需的内容。源码、测试、air 配置、数据库文件都不进包。
	items := []struct {
		src, dst string
		mode     os.FileMode
	}{
		{filepath.Join(dist, binaryOut), binName, 0o755},
		{".env.example", ".env.example", 0o644},
		{"start.sh", "start.sh", 0o755},
		{"smart-mzcmc.service", "smart-mzcmc.service", 0o644},
		{"DEPLOY.md", "DEPLOY.md", 0o644},
	}
	for _, it := range items {
		copyFile(it.src, filepath.Join(stage, it.dst), it.mode)
		log("+ %s (%o)", it.dst, it.mode)
	}

	// 三个静态站点
	for _, site := range []string{"public/admin", "public/docs", "public/interviewer"} {
		if _, err := os.Stat(site); err != nil {
			fatal("缺少站点产物 %s，请先构建：\n"+
				"  admin        -> cd ../admin && pnpm run build:deploy\n"+
				"  docs         -> cd ../docs  && pnpm run docs:build:deploy\n"+
				"  interviewer  -> cd ../interviewer && build-web.bat", site)
		}
		copyTree(site, filepath.Join(stage, site))
		n := countFiles(filepath.Join(stage, site))
		log("+ %s (%d 个文件)", site, n)
	}

	// 采访端两处「构建能过、线上白屏」的坑，在这里拦掉。
	// 见 interviewer/README.md。
	verifyInterviewer(filepath.Join(stage, "public/interviewer"))

	// public/index.html（首页）
	copyFile("public/index.html", filepath.Join(stage, "public", "index.html"), 0o644)
	log("+ public/index.html")

	// resources（视图模板）
	if _, err := os.Stat("resources"); err == nil {
		copyTree("resources", filepath.Join(stage, "resources"))
		log("+ resources (%d 个文件)", countFiles(filepath.Join(stage, "resources")))
	}

	// 运行期目录：放 .keep 占位，避免 tar 丢空目录；
	// 实际上主程序启动时也会自动创建，这里是双保险。
	for _, d := range []string{"database", "storage/logs", "storage/framework/sessions"} {
		must(os.MkdirAll(filepath.Join(stage, d), 0o755))
		must(os.WriteFile(filepath.Join(stage, d, ".keep"), []byte("运行期目录，请勿删除或纳入版本控制。\n"), 0o644))
	}
	log("+ database/ storage/logs/ storage/framework/sessions/ (含 .keep)")

	// ---- 4. 打包 ----
	out := filepath.Join(dist, pkgName+".tar.gz")
	if err := os.RemoveAll(out); err != nil {
		fatal("清理旧包失败: %v", err)
	}
	log("压缩 -> %s", filepath.Clean(out))
	if err := writeTarGz(out, stage, pkgName); err != nil {
		fatal("压缩失败: %v", err)
	}

	log("")
	log("完成: %s (%.1f MB)", filepath.Clean(out), mb(out))
	log("解压后: tar -xzf %s && cd %s", filepath.Base(out), pkgName)
}

// assertELF 校验产物确实是 Linux x86-64 可执行文件。
func assertELF(path string) {
	data, err := os.ReadFile(path)
	must(err)
	if len(data) < 20 || string(data[:4]) != "\x7fELF" {
		fatal("产物不是 ELF 文件：%s", path)
	}
	if data[18] != 0x3e {
		fatal("产物不是 x86-64（e_machine=%#x）", data[18])
	}
}

// verifyInterviewer 校验采访端产物不会在部署后白屏。
//
// 这两个坑的共同点是「flutter build 成功、CI 全绿，但线上打不开」，
// 所以只能在打包这一关拦：
//
//  1. base-href 不是 /interviewer/ —— 应用挂在子路径下，默认的 "/" 会让
//     所有资源解析到域名根目录并 404。
//  2. 缺本地 canvaskit —— 默认从 gstatic.com 拉，校园内网没有外网就卡死。
//
// 修复方式都是用 interviewer/build-web.bat 重新构建。
func verifyInterviewer(dir string) {
	// 1) base href
	indexPath := filepath.Join(dir, "index.html")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		fatal("采访端缺少 index.html: %v", err)
	}
	if !strings.Contains(string(data), `<base href="/interviewer/"`) {
		fatal("采访端 base-href 不对：%s/index.html 里不是 /interviewer/。\n"+
			"应用挂在 /interviewer/ 下，默认的 \"/\" 会让所有资源解析到域名根目录\n"+
			"并 404 —— 构建过程不报错，但线上是白屏。\n"+
			"请用 interviewer/build-web.bat 重新构建（它带 --base-href）。",
			filepath.ToSlash(dir))
	}
	log("  base-href = /interviewer/ ✓")

	// 2) 运行期配置
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		fatal("采访端缺少 config.json，现场就没法改服务器地址。\n" +
			"请用 interviewer/build-web.bat 重新构建。")
	}
	log("  config.json ✓")

	// 3) 本地 canvaskit
	canvaskit := filepath.Join(dir, "canvaskit", "canvaskit.js")
	if _, err := os.Stat(canvaskit); err != nil {
		fatal("采访端缺少本地 canvaskit/%s。\n"+
			"默认构建会从 gstatic.com 拉 CanvasKit，校园内网通常没有外网，\n"+
			"结果就是白屏。请用 interviewer/build-web.bat 重新构建\n"+
			"（它带 --no-web-resources-cdn）。", "canvaskit.js")
	}
	log("  本地 canvaskit ✓")
}

// ---------- 打包 ----------

func writeTarGz(out, stage, top string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.Walk(stage, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := top + "/" + filepath.ToSlash(rel)

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		// Windows 文件系统没有 Unix 执行位，FileInfoHeader 拿到的 mode 不含 0o111。
		// 必须在写头之前显式补上，否则 Linux 上 start.sh 不可执行。
		relMode := 0o644
		if execFiles[filepath.ToSlash(rel)] {
			relMode = 0o755
		}
		hdr.Mode = int64(relMode)
		// 去掉时间戳，让相同内容打出相同包，便于比对
		hdr.ModTime = time.Unix(0, 0)
		hdr.AccessTime = time.Unix(0, 0)
		hdr.ChangeTime = time.Unix(0, 0)
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""

		switch {
		case info.IsDir():
			hdr.Name += "/"
			return tw.WriteHeader(hdr)
		case info.Mode()&os.ModeSymlink != 0:
			return nil // 产物里不该有软链
		case info.Mode().IsRegular():
			// 双保险：运行期数据绝不能进包。
			// 上面已拦截暂存目录里的数据库文件，这里再挡一次
			// storage/ 下的日志与会话，防止手工往暂存目录里塞过东西。
			rel := filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "storage/") && !strings.HasSuffix(rel, "/.keep") {
				fatal("暂存目录里出现了运行期数据: %s，不应打进发布包", rel)
			}
			if strings.HasSuffix(rel, ".db") || strings.HasSuffix(rel, ".db-wal") || strings.HasSuffix(rel, ".db-shm") {
				fatal("暂存目录里出现了数据库文件: %s，不应打进发布包", rel)
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			src, err := os.Open(path)
			if err != nil {
				return err
			}
			defer src.Close()
			_, err = io.Copy(tw, src)
			return err
		}
		return nil
	})
}

// ---------- 文件操作 ----------

// copyFile 复制文件并设置权限。mode 为 0 时用 0644。
func copyFile(src, dst string, mode os.FileMode) {
	if _, err := os.Stat(src); err != nil {
		fatal("缺少文件 %s", src)
	}
	if mode == 0 {
		mode = 0o644
	}
	must(os.MkdirAll(filepath.Dir(dst), 0o755))
	data, err := os.ReadFile(src)
	must(err)
	// shell 脚本在 Windows 上签出可能是 CRLF，转成 LF，否则 Linux 上会报
	// 「/bin/sh^M: bad interpreter」。
	if strings.HasSuffix(src, ".sh") {
		data = []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
	}
	must(os.WriteFile(dst, data, mode))
	// Windows 上 os.WriteFile 的权限位不生效，这里显式补一次（仅对非 Windows 有意义）。
	_ = os.Chmod(dst, mode)
}

func copyTree(src, dst string) {
	must(filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// 跳过把 shell 脚本的 CRLF 换成 LF
		if strings.HasSuffix(path, ".sh") {
			data = []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
		}
		return os.WriteFile(target, data, 0o644)
	}))
}

func countFiles(root string) int {
	n := 0
	filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

func mb(path string) float64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return float64(info.Size()) / 1024 / 1024
}

func must(err error) {
	if err != nil {
		fatal("%v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	os.Exit(1)
}
