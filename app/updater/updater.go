// Package updater 实现后端的在线更新。
//
// 更新源是 GitHub Release：CI 打 tag 后会把后端发布包与 checksums.txt 一起
// 挂上去，更新流程就是拿这里的产物做校验后替换自身可执行文件。
//
// 安全上的取舍：这是一个「下载网络内容并执行」的流程，所以
//   - 总开关默认关闭（UPDATE_ENABLED）
//   - 校验和不匹配一律拒绝，且查不到条目同样视为不通过
//   - 解压防路径穿越与压缩炸弹
//   - 替换前留备份，迁移失败自动回滚
//   - 自动替换默认关闭（UPDATE_ALLOW_REPLACE），因为替换后本进程要退出，
//     只有托管在 systemd 之下才会被重新拉起
package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 是更新功能的运行参数，来自 config/update.go。
type Config struct {
	Enabled      bool
	Server       string
	Repo         string
	Asset        string
	AllowReplace bool
	Token        string
	Timeout      time.Duration
}

func (c Config) apiBase() string {
	if c.Server != "" {
		return strings.TrimRight(c.Server, "/")
	}
	return "https://api.github.com"
}

// release 是 GitHub release payload 里我们用得到的字段。
type release struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
	Body        string `json:"body"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

// CheckResult 是检查更新的结果。
//
// 注意：拉取失败时仍然返回正常结构（而不是 error），因为「连不上更新源」是
// 运维需要看到的日常状态，不该让整个接口变成 500。
type CheckResult struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version,omitempty"`
	HasUpdate      bool   `json:"has_update"`
	Enabled        bool   `json:"enabled"`
	AllowReplace   bool   `json:"allow_replace"`
	Source         string `json:"source"`
	AssetName      string `json:"asset_name"`
	PublishedAt    string `json:"published_at,omitempty"`
	ReleaseURL     string `json:"release_url,omitempty"`
	Size           int64  `json:"size,omitempty"`
	Error          string `json:"error,omitempty"`
}

// ApplyResult 是一次更新的执行结果。
type ApplyResult struct {
	Version    string   `json:"version"`
	Bytes      int64    `json:"bytes"`
	SHA256     string   `json:"sha256"`
	Arch       string   `json:"arch"`
	Staged     bool     `json:"staged"`
	Replaced   bool     `json:"replaced"`
	Migrated   bool     `json:"migrated"`
	BackupPath string   `json:"backup_path,omitempty"`
	StagedPath string   `json:"staged_path,omitempty"`
	Steps      []string `json:"steps"`
	Restart    string   `json:"restart_hint,omitempty"`
}

type Updater struct {
	cfg  Config
	http *http.Client
	log  func(string, ...any)
}

func New(cfg Config, logf func(string, ...any)) *Updater {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Updater{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
		log:  logf,
	}
}

// Check 查询最新版本。
func (u *Updater) Check(current string) CheckResult {
	res := CheckResult{
		CurrentVersion: current,
		Enabled:        u.cfg.Enabled,
		AllowReplace:   u.cfg.AllowReplace,
		Source:         u.cfg.apiBase() + "/" + u.cfg.Repo,
		AssetName:      u.cfg.Asset,
	}
	if !u.cfg.Enabled {
		res.Error = "在线更新未启用（需设置 UPDATE_ENABLED=true）"
		return res
	}

	rel, err := u.fetchLatest()
	if err != nil {
		res.Error = err.Error()
		return res
	}

	latest := strings.TrimPrefix(rel.TagName, "v")
	res.LatestVersion = latest
	res.PublishedAt = rel.PublishedAt
	res.ReleaseURL = rel.HTMLURL

	asset := findAsset(rel, u.cfg.Asset)
	if asset == nil {
		res.Error = fmt.Sprintf("版本 %s 的发布包里没有 %s", rel.TagName, u.cfg.Asset)
		return res
	}
	res.Size = asset.Size
	res.HasUpdate = isNewer(latest, current)
	return res
}

// fetchLatest 拉取最新的非草稿、非预发布版本。
func (u *Updater) fetchLatest() (*release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", u.cfg.apiBase(), u.cfg.Repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// 没配 token 时 GitHub 限流是每小时 60 次/IP。内网多台机器共用一个出口 IP
	// 时很容易撞上，配了 token 是 5000 次/小时。
	if u.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.cfg.Token)
	}

	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上更新源 %s: %w", u.cfg.apiBase(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("更新源上没有可用的 Release（仓库可能不存在或没有任何发布）")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("更新源返回 HTTP %d", resp.StatusCode)
	}

	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 Release 信息失败: %w", err)
	}
	return &rel, nil
}

// Apply 下载并校验新版本。
//
// current 必须是当前运行的版本号；传空串表示不做「是否更新」的判断，
// 直接下载 latest。allowTarget 非空时只接受该版本，防止「检查时看到的是
// v1.2.0，下载时变成了 v1.3.0」这种竞态——版本必须由人确认过。
func (u *Updater) Apply(current, allowTarget, binaryName string) (*ApplyResult, error) {
	if !u.cfg.Enabled {
		return nil, errors.New("在线更新未启用（需设置 UPDATE_ENABLED=true）")
	}

	var steps []string
	step := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		steps = append(steps, msg)
		u.log("%s", msg)
	}

	rel, err := u.fetchLatest()
	if err != nil {
		return nil, err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")

	if allowTarget != "" && allowTarget != latest {
		return nil, fmt.Errorf("版本已变化：你确认的是 %s，但更新源上现在是 %s，请重新检查后再操作", allowTarget, latest)
	}
	if allowTarget == "" && !isNewer(latest, current) {
		return nil, fmt.Errorf("当前已是 %s，没有更新的版本（更新源最新为 %s）", current, latest)
	}

	asset := findAsset(rel, u.cfg.Asset)
	if asset == nil {
		return nil, fmt.Errorf("版本 %s 的发布包里没有 %s", rel.TagName, u.cfg.Asset)
	}
	step("目标版本 %s（%.1f MB）", latest, float64(asset.Size)/(1<<20))

	dir := updateDir(binaryName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	archivePath := filepath.Join(dir, u.cfg.Asset)

	// 1. 下载发布包
	if err := u.download(asset.BrowserDownloadURL, archivePath); err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	info, _ := os.Stat(archivePath)
	var size int64
	if info != nil {
		size = info.Size()
	}
	step("已下载 %.1f MB", float64(size)/(1<<20))

	// 2. 校验和
	sum, err := fileSHA256FromFile(archivePath)
	if err != nil {
		return nil, err
	}
	checksums, err := u.fetchAsset(rel, "checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("无法获取 checksums.txt，拒绝在无法校验的情况下继续: %w", err)
	}
	if err := verifyChecksum(checksums, u.cfg.Asset, sum); err != nil {
		return nil, err
	}
	step("sha256 校验通过 %s", sum[:16])

	// 3. 解出可执行文件
	staged := filepath.Join(dir, binaryName+".new")
	if err := extractBinary(archivePath, binaryName, staged); err != nil {
		return nil, fmt.Errorf("解压失败: %w", err)
	}
	arch, err := elfFingerprint(staged)
	if err != nil {
		os.Remove(staged)
		return nil, fmt.Errorf("产物校验失败: %w", err)
	}
	step("已解出 %s（ELF %s）", binaryName, arch)

	res := &ApplyResult{
		Version:    latest,
		Bytes:      size,
		SHA256:     sum,
		Arch:       arch,
		Staged:     true,
		StagedPath: staged,
		Steps:      steps,
	}

	if !u.cfg.AllowReplace {
		res.Restart = "UPDATE_ALLOW_REPLACE 未开启，新版本已就绪但未替换当前程序；确认无误后可手工替换并重启。"
		return res, nil
	}

	// 4. 备份并替换
	live, err := os.Executable()
	if err != nil {
		return nil, err
	}
	live, _ = filepath.EvalSymlinks(live)
	backup := live + ".bak"
	if err := copyFile(live, backup, 0o755); err != nil {
		return nil, fmt.Errorf("备份当前程序失败: %w", err)
	}
	res.BackupPath = backup
	step("已备份当前程序到 %s", backup)

	// 先写同目录临时文件再 rename：跨文件系统 rename 会失败，同目录则原子。
	tmp := live + ".incoming"
	if err := copyFile(staged, tmp, 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, live); err != nil {
		os.Remove(tmp)
		// 替换失败：把备份放回去，保证磁盘上始终有一个可运行的程序
		copyFile(backup, live, 0o755)
		return nil, fmt.Errorf("替换程序失败，已回滚: %w", err)
	}
	res.Replaced = true
	step("已替换为新版本 %s", latest)

	// 5. 先跑迁移再退出。迁移失败就把备份换回去——否则进程一退出，
	// systemd 拉起一个起不来的版本，故障就从「更新失败」变成「服务不可用」。
	if err := u.runMigrate(live); err != nil {
		copyFile(backup, live, 0o755)
		return nil, fmt.Errorf("新版本数据库迁移失败，已回滚到备份版本，请检查迁移日志: %w", err)
	}
	res.Migrated = true
	step("数据库迁移成功")

	res.Restart = "本进程即将退出，请由 systemd 自动拉起新版本。"
	return res, nil
}

// runMigrate 用新二进制跑一次 migrate。
//
// 直接调子命令而不是等下次启动：迁移失败要能当场发现并回滚，
// 否则等 systemd 拉起时已经晚了。
func (u *Updater) runMigrate(binary string) error {
	cmd := exec.Command(binary, "migrate")
	cmd.Dir = filepath.Dir(binary)
	out, err := cmd.CombinedOutput()
	u.log("[Update] migrate 输出: %s", strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (u *Updater) download(url, dest string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 HTTP %d", resp.StatusCode)
	}

	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxArtifactBytes)); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func (u *Updater) fetchAsset(rel *release, name string) ([]byte, error) {
	asset := findAsset(rel, name)
	if asset == nil {
		return nil, fmt.Errorf("发布包 %s 里没有 %s", rel.TagName, name)
	}
	req, err := http.NewRequest(http.MethodGet, asset.BrowserDownloadURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 返回 HTTP %d", name, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func findAsset(rel *release, name string) *struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
} {
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			return &rel.Assets[i]
		}
	}
	return nil
}

func fileSHA256FromFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return fileSHA256(f)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// updateDir 返回暂存目录：<程序所在目录>/update。
func updateDir(binaryPath string) string {
	dir := filepath.Dir(binaryPath)
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "update")
}

// isNewer 判断 candidate 是否比 current 新。
//
// 只比较三段数字：版本号由我们自己控制（release.yml 会校验格式），
// 而 git tag 上可能带前缀，预发布标记（-rc1 之类）本项目不使用。
// 段数不同时按较短的那个比，剩下的段视作 0，因此 1.2 == 1.2.0。
// 格式非法时一律返回 false——宁可说「没有更新」让人手动核对，
// 也不要因为解析失败而误判成需要更新。
func isNewer(candidate, current string) bool {
	if !validVersion(candidate) || !validVersion(current) {
		return false
	}
	c, cur := parseVersion(candidate), parseVersion(current)
	for i := 0; i < 3; i++ {
		if c[i] != cur[i] {
			return c[i] > cur[i]
		}
	}
	return false
}

// validVersion 判断是否形如 1 / 1.2 / 1.2.3（可带 v 前缀）。
func validVersion(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

func parseVersion(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// 去掉预发布/构建元数据
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return [3]int{0, 0, 0}
		}
		out[i] = n
	}
	return out
}
