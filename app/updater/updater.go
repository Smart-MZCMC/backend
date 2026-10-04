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
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// checksumFile 是校验清单的文件名，单独提出来是因为 fetchAsset 要靠它判断
// 「该不该改用可信源」。
const checksumFile = "checksums.txt"

// Config 是更新功能的运行参数，来自 config/update.go。
type Config struct {
	Enabled      bool
	Server       string
	Repo         string
	Asset        string
	AllowReplace bool
	Token        string
	Timeout      time.Duration

	// DownloadMirror 是资产下载镜像的前缀。为空表示直接从 GitHub 下载。
	//
	// ⚠️ 用了镜像就意味着**包的校验值也来自镜像**（除非另外配了 ChecksumURL）：
	// 校验和与被校验的包由同一方提供，攻破镜像即可同时替换两者，让 sha256
	// 校验形同虚设。这是引入镜像必须一起付的代价，不是可以忽略的实现细节。
	// 要保留真正的完整性保证，把 ChecksumURL 指到一个与镜像无关的可信源。
	DownloadMirror string

	// ChecksumURL 是 checksums.txt 的可信地址。为空表示跟随资产来源
	// （即同样经 DownloadMirror 取回）。
	ChecksumURL string
}

func (c Config) apiBase() string {
	if c.Server != "" {
		return strings.TrimRight(c.Server, "/")
	}
	return "https://api.github.com"
}

// assetURL 返回资产的实际下载地址。
//
// 镜像按「把原始 URL 整体套进前缀」处理，而不是替换域名。原因是各镜像服务
// 的 URL 结构差异很大：`https://ghfast.top/https://github.com/...`、
// `https://gh-proxy.com/https://github.com/...`、自建的 `/github-proxy/…`
// 各不相同，一个前缀配置能覆盖全部，换成「填域名」就得为每种镜像写一套模板。
func (c Config) assetURL(raw string) string {
	if c.DownloadMirror == "" {
		return raw
	}
	return strings.TrimRight(c.DownloadMirror, "/") + "/" + raw
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
	// ReleaseNotes 是该版本的发布正文（Markdown 原文，未加工）。
	//
	// 不可信：来自 UPDATE_API_BASE 指向的更新源，可以是镜像。前端渲染它时
	// 必须先转义再套用白名单，绝不能当 HTML 注入。
	ReleaseNotes string `json:"release_notes,omitempty"`
	Error        string `json:"error,omitempty"`
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

// Progress 是一次更新任务的实时进度。
//
// 为什么要单独一个结构而不是靠日志：更新要下载 26 MB 的包，在校园网里可能
// 要几分钟。原来整个下载/校验/解压/替换都在一个 HTTP 请求里同步跑完，
// 界面上只能显示「更新中…」和一个不动的按钮——用户既不知道卡在哪一步，
// 也不知道是在下载还是在解压，最后只能刷新页面看看服务活着没有。
type Progress struct {
	// Stage 是机器可读的阶段标识，前端据此决定进度条的形态
	// （下载阶段看百分比，其余阶段看阶段名）。
	Stage string `json:"stage"`
	// Message 是给运维看的一句话，与 Steps 里累积的日志并行存在。
	Message string `json:"message,omitempty"`
	// Done / Total 是已处理与总量。下载阶段是字节数，其余阶段 Total 为 0。
	Done    int64   `json:"done"`
	Total   int64   `json:"total"`
	Percent float64 `json:"percent"`
	// Steps 是按时间顺序累积的执行日志，与 ApplyResult.Steps 同源。
	Steps []string `json:"steps,omitempty"`
	// Finished / Failed 是终态标志。二者之一为 true 时前端应停止轮询。
	Finished bool   `json:"finished"`
	Failed   bool   `json:"failed"`
	Error    string `json:"error,omitempty"`
	// Result 在 Finished 且未 Failed 时有值，内容与旧的同步返回值一致。
	Result *ApplyResult `json:"result,omitempty"`
	// StartedAt / UpdatedAt 供前端判断「多久没动静了」。
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 阶段标识。取值写成常量而不是散落的字面量：前端要按阶段分支，
// 两边各写一份字符串迟早对不上。
const (
	StageIdle        = "idle"
	StageFetching    = "fetching"
	StageDownloading = "downloading"
	StageVerifying   = "verifying"
	StageExtracting  = "extracting"
	StageReplacing   = "replacing"
	StageMigrating   = "migrating"
	StageFinished    = "finished"
	StageFailed      = "failed"
)

// 进度状态。Apply 在后台 goroutine 里跑，而查询进度是另一个请求，
// 所以状态必须是包级的。整套在线更新同时只允许一个任务（见 Start）。
var (
	progressMu sync.RWMutex
	progress   Progress
)

// CurrentProgress 返回最近一次更新的进度。
//
// 没有任务在跑时返回 Stage=idle 的空进度，而不是 404——
// 「还没开始更新」是正常状态，前端据此显示空面板。
func CurrentProgress() Progress {
	progressMu.RLock()
	defer progressMu.RUnlock()
	if progress.Stage == "" {
		return Progress{Stage: StageIdle}
	}
	return progress
}

// beginProgress 清空并写入一次新任务的初始状态。
func beginProgress() {
	progressMu.Lock()
	defer progressMu.Unlock()
	now := time.Now()
	progress = Progress{
		Stage:     StageFetching,
		Message:   "正在查询更新源…",
		StartedAt: now,
		UpdatedAt: now,
	}
}

// BeginProgress 由控制器在启动任务前调用，重置上一轮遗留的状态。
func BeginProgress() { beginProgress() }

// FinishProgress 标记任务成功结束。
func FinishProgress(res *ApplyResult) {
	recordProgress(func(p *Progress) {
		p.Stage = StageFinished
		p.Finished = true
		p.Failed = false
		p.Error = ""
		p.Message = "更新完成"
		p.Percent = 100
		if res != nil {
			p.Result = res
			// 阶段消息比通用的「更新完成」有用：替换成功意味着马上要重启，
			// 没开启替换则意味着还要手工操作，两者的下一步完全不同。
			if res.Replaced {
				p.Message = "更新完成，服务即将重启"
			} else {
				p.Message = "已下载并校验，等待手工替换"
			}
		}
	})
}

// FailProgress 标记任务失败，并把原因写进进度。
//
// 失败信息必须走进度而不是只返回给发起请求的那一次调用：请求早已返回
// 「已启动」，之后的失败只有轮询才看得到。
func FailProgress(msg string) {
	recordProgress(func(p *Progress) {
		p.Stage = StageFailed
		p.Failed = true
		p.Finished = true
		p.Error = msg
		p.Message = "更新失败"
	})
}

// recordProgress 更新进度。fn 收到的是当前快照的副本，改完再写回，
// 避免调用方在闭包里意外把别的字段一起覆盖。
func recordProgress(fn func(p *Progress)) {
	progressMu.Lock()
	defer progressMu.Unlock()
	p := progress
	fn(&p)
	p.UpdatedAt = time.Now()
	progress = p
}

// appendStep 往累积日志里追加一行。
func appendStep(msg string) {
	recordProgress(func(p *Progress) { p.Steps = append(p.Steps, msg) })
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
	// Release 正文本来就已经取回来了（fetchLatest 解的是同一个 release 结构），
	// 之前只是没往外送。更新详情要的就是它——运维在点「应用更新」之前需要知道
	// 这次会不会有新配置项、客户端要不要重新编译。
	//
	// 这里不做任何加工：正文是 Markdown，且来自 UPDATE_API_BASE 指向的更新源
	// （可以是镜像），属于不可信输入。要不要渲染、怎么渲染，由前端负责，
	// 后端只负责原样送达。
	res.ReleaseNotes = rel.Body

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

// Apply 下载并校验新版本，不报告进度。
//
// 等价于 ApplyWithProgress(..., nil)。保留这个薄封装是因为「只想拿结果」
// 的调用方（例如将来的命令行工具）不该被一个回调参数绑住。
func (u *Updater) Apply(current, allowTarget, binaryName string) (*ApplyResult, error) {
	return u.ApplyWithProgress(current, allowTarget, binaryName, nil)
}

// ApplyWithProgress 下载并校验新版本，并通过 progress 回调实时报告进度。
//
// progress 可以为 nil（此时不做任何额外工作）。非 nil 时它会被并发调用：
// 更新在后台 goroutine 里跑，查询进度是另一个请求，两者共享这份状态。
// 所以回调内部必须只做「记一份快照」这种轻活，绝不能阻塞。
func (u *Updater) ApplyWithProgress(current, allowTarget, binaryName string, progress func(Progress)) (*ApplyResult, error) {
	if !u.cfg.Enabled {
		return nil, errors.New("在线更新未启用（需设置 UPDATE_ENABLED=true）")
	}

	var steps []string
	step := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		steps = append(steps, msg)
		u.log("%s", msg)
		appendStep(msg)
	}
	// stage 切阶段时补一条进度。下载阶段由字节回调单独驱动，
	// 不在这里覆盖它的 Done/Total。
	stage := func(name, format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		recordProgress(func(p *Progress) {
			p.Stage = name
			p.Message = msg
			// 进入新阶段时把百分比清零，避免上一阶段的数字被误读成当前进度。
			p.Done, p.Total, p.Percent = 0, 0, 0
		})
	}

	stage(StageFetching, "正在查询更新源…")
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

	// 暂存目录与部署目录同一个锚点，否则启动清理会漏掉一半残渣。
	//
	// 这里刻意用 root（可执行文件所在目录）而不是 binaryName：调用方传的是
	// filepath.Base(exe)，拿它拼路径会落到工作目录下的 update/，于是下载的几十 MB
	// 发布包和解出来的 stage 全都留在那儿，启动时的 CleanupLeftovers 只清程序目录
	// 那一份，永远清不掉。
	root, err := deployRoot()
	if err != nil {
		return nil, err
	}
	dir := updateDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	archivePath := filepath.Join(dir, u.cfg.Asset)

	// 1. 下载发布包
	stage(StageDownloading, "正在下载 %s", u.cfg.Asset)
	downloadURL := u.cfg.assetURL(asset.BrowserDownloadURL)
	if u.cfg.DownloadMirror != "" {
		step("下载地址经镜像：%s", u.cfg.DownloadMirror)
	}
	if err := u.download(downloadURL, archivePath, asset.Size, func(done int64) {
		recordProgress(func(p *Progress) {
			p.Stage = StageDownloading
			p.Done = done
			p.Total = asset.Size
			p.Percent = percentOf(done, asset.Size)
			p.Message = "正在下载 " + humanBytes(done) + " / " + humanBytes(asset.Size)
		})
	}); err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	info, _ := os.Stat(archivePath)
	var size int64
	if info != nil {
		size = info.Size()
	}
	step("已下载 %.1f MB", float64(size)/(1<<20))

	// 2. 校验和
	stage(StageVerifying, "正在校验 sha256…")
	sum, err := fileSHA256FromFile(archivePath)
	if err != nil {
		return nil, err
	}
	checksums, err := u.fetchAsset(rel, checksumFile)
	if err != nil {
		return nil, fmt.Errorf("无法获取 checksums.txt，拒绝在无法校验的情况下继续: %w", err)
	}
	if err := verifyChecksum(checksums, u.cfg.Asset, sum); err != nil {
		return nil, err
	}
	step("sha256 校验通过 %s", sum[:16])

	// 3. 解到暂存目录：可执行文件 + public/ + resources/
	stage(StageExtracting, "正在解压新版本…")
	live, err := os.Executable()
	if err != nil {
		return nil, err
	}
	live, _ = filepath.EvalSymlinks(live)

	// 解压目标统一放在 update/stage 下，与最终部署目录同级——
	// 跨文件系统 rename 会失败，同盘才保证替换是原子的。
	stagingRoot := filepath.Join(dir, "stage")
	if err := os.RemoveAll(stagingRoot); err != nil {
		return nil, err
	}

	want := append([]string{binaryName}, deployDirs...)
	found, err := extractArchive(archivePath, stagingRoot, want, nil)
	if err != nil {
		return nil, fmt.Errorf("解压失败: %w", err)
	}
	if !found[binaryName] {
		return nil, fmt.Errorf("归档包里没有找到 %s", binaryName)
	}

	staged := filepath.Join(stagingRoot, binaryName)
	arch, err := elfFingerprint(staged)
	if err != nil {
		return nil, fmt.Errorf("产物校验失败: %w", err)
	}
	step("已解出 %s（ELF %s）", binaryName, arch)

	// public 必须完整。包里带着残缺的 public 时若继续下去，会把现存的站点
	// 整份换掉、后台直接 404——比不更新更糟。
	if err := verifyStagedSites(stagingRoot, found); err != nil {
		return nil, err
	}

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
		// 这里刻意不设 StageFinished：终态（含 Finished/Failed 标志）由调用方
		// 通过 FinishProgress / FailProgress 落。混着设会造出
		//「stage=finished 但 finished=false」这种自相矛盾的进度，前端无从判断该不该停轮询。
		step("新版本已就绪，未替换当前程序；暂存目录 %s", stagingRoot)
		return res, nil
	}

	// 4. 替换：先换站点，再换程序，最后跑迁移
	//
	// 顺序是有意的：站点是静态文件、替换失败只影响后台，而程序替换完还要跑
	// 迁移。把可回滚的东西先换掉，最后一步失败时还能整体退回去。
	stage(StageReplacing, "正在替换静态站点与当前程序…")

	var pending []replaced
	rollback := func(reason error) error {
		for i := len(pending) - 1; i >= 0; i-- {
			if err := pending[i].restore(root); err != nil {
				// 回滚本身失败是最坏的情况：磁盘上可能既没有新版本也没有旧版本。
				// 必须说出来，否则运维看到的只是一句「更新失败」。
				return fmt.Errorf("%w；且回滚 %s 失败: %v", reason, pending[i].rel, err)
			}
		}
		return reason
	}

	// 先扬掉再整份拷入。分两步 rename（现有的挪到 update/old，新的挪过来）而不是
	// 先删后拷：删完再拷的那一瞬目录不存在，而站点正被运行中的服务按请求路径读，
	// 那一下就是 404。两步之间的窗口只剩一次 rename。
	for _, rel := range deployDirs {
		if !found[rel] {
			step("跳过 %s（发布包里没有）", rel)
			continue
		}
		backup := oldPathFor(root, rel)
		if err := swapDir(root, filepath.Join(stagingRoot, filepath.FromSlash(rel)), rel, backup); err != nil {
			return nil, rollback(err)
		}
		pending = append(pending, replaced{rel: rel, backup: backup})
		step("已替换 %s（整份换掉，不留上一版残渣）", rel)
	}

	// 备份并替换可执行文件
	backup := live + ".bak"
	if err := copyFile(live, backup, 0o755); err != nil {
		return nil, rollback(fmt.Errorf("备份当前程序失败: %w", err))
	}
	res.BackupPath = backup
	step("已备份当前程序到 %s", backup)

	// 先写同目录临时文件再 rename：跨文件系统 rename 会失败，同目录则原子。
	tmp := live + ".incoming"
	if err := copyFile(staged, tmp, 0o755); err != nil {
		return nil, rollback(err)
	}
	if err := os.Rename(tmp, live); err != nil {
		os.Remove(tmp)
		return nil, rollback(fmt.Errorf("替换程序失败，已回滚: %w", err))
	}
	res.Replaced = true
	step("已替换为新版本 %s", latest)

	// 5. 先跑迁移再退出。迁移失败就把备份换回去——否则进程一退出，
	// systemd 拉起一个起不来的版本，故障就从「更新失败」变成「服务不可用」。
	stage(StageMigrating, "正在执行数据库迁移…")
	if err := u.runMigrate(live); err != nil {
		if rerr := copyFile(backup, live, 0o755); rerr != nil {
			return nil, fmt.Errorf("新版本数据库迁移失败: %w；且程序回滚失败: %v", err, rerr)
		}
		return nil, rollback(fmt.Errorf("新版本数据库迁移失败，已回滚到备份版本，请检查迁移日志: %w", err))
	}
	res.Migrated = true
	step("数据库迁移成功")

	// 6. 清理更新时留下的垃圾
	//
	// 下载的发布包（几十 MB）、.part、暂存目录、换下来的旧站点都在 update/ 下，
	// 更新成功后它们没有任何用途，留着只会让下一轮更新反复多占一份磁盘。
	// 备份的 live.bak 刻意留着：它是这一版出问题之后唯一的回退路径，
	// 而它每次更新都会被同名覆盖，数量上不会累积。
	if err := os.RemoveAll(dir); err != nil {
		// 清理失败不影响更新结果，服务已经是新版本且跑得起来。
		u.log("[Update] 清理暂存目录 %s 失败: %v", dir, err)
		step("已更新，但暂存目录 %s 清理失败，可手工删除", dir)
	} else {
		step("已清理暂存目录 %s", dir)
	}

	res.Restart = "本进程即将退出，请由 systemd 自动拉起新版本。"
	step("更新完成，服务即将重启")
	return res, nil
}

// verifyStagedSites 检查暂存目录里被认领的站点是否完整。
//
// 「被认领」指发布包里确实带了 public（extractArchive 的 found）。包里带了却
// 缺任何一个站点的 index.html 只能说明包本身坏了，此时必须拒绝整次更新：
// public 是整份换掉的，带着一份残缺的上去等于把能用的后台换成 404。
//
// 没带 public 时直接跳过、保留现场那份——发布包不带前端时不该连累程序更新。
func verifyStagedSites(stagingRoot string, found map[string]bool) error {
	if !found["public"] {
		return nil
	}
	for _, sentinel := range deploySentinels {
		path := filepath.Join(stagingRoot, filepath.FromSlash(sentinel))
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("发布包里缺少 %s，拒绝用残缺的站点覆盖现有版本", sentinel)
		}
	}
	return nil
}

// replaced 记录一次已完成的目录替换，用于迁移失败时回退。
type replaced struct {
	rel    string
	backup string
}

// oldPathFor 返回换下来的旧内容在 update/old 下的存放位置。
func oldPathFor(root, rel string) string {
	return filepath.Join(updateDir(root), "old", filepath.FromSlash(rel))
}

// swapDir 把 staged 整份换到 root/rel：先把现有的挪到 backup，再把新的挪过来。
//
// 分两步 rename 而不是先删后拷，正是「先把 public 扬了重新拷入」的意思，
// 但中间不留空窗：删完再拷的那一瞬目录不存在，而站点正被运行中的服务按请求
// 路径读，那一下就是 404。同盘 rename 是原子的，两步之间的窗口只剩一次 rename。
func swapDir(root, staged, rel, backup string) error {
	target := filepath.Join(root, filepath.FromSlash(rel))

	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return err
	}
	// 上一次失败可能留下了旧备份，先清掉，否则 rename 到已存在的路径行为不确定。
	if err := os.RemoveAll(backup); err != nil {
		return err
	}

	hadOld := false
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("换下旧 %s 失败: %w", rel, err)
		}
		hadOld = true
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		if hadOld {
			os.Rename(backup, target)
		}
		return err
	}

	if err := os.Rename(staged, target); err != nil {
		// 换上去失败就把旧的那份挪回来，不能留下一个空目录。
		if hadOld {
			os.Rename(backup, target)
		}
		return fmt.Errorf("换上新 %s 失败: %w", rel, err)
	}
	return nil
}

// restore 把 backup 挪回原位。backup 不存在说明原先那里就没有这份内容，
// 此时只要把换上去的删掉。
func (r replaced) restore(root string) error {
	target := filepath.Join(root, filepath.FromSlash(r.rel))
	if _, err := os.Lstat(r.backup); err == nil {
		os.RemoveAll(target)
		return os.Rename(r.backup, target)
	}
	return os.RemoveAll(target)
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

// download 下载一个文件，过程中按字节回调进度。
//
// total 来自 Release 里的 size 字段，只用于显示百分比；传 0 也能正常工作，
// 此时回调仍然报告已下载字节数，由调用方自己决定怎么呈现。
//
// onProgress 可能被调用很多次（每个 Write 一次），所以它必须很轻——
// 调用方的节流放在自己那一层。
func (u *Updater) download(url, dest string, total int64, onProgress func(int64)) error {
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
	w := &progressWriter{w: f, onProgress: onProgress}
	if _, err := io.Copy(w, io.LimitReader(resp.Body, maxArtifactBytes)); err != nil {
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

// progressWriter 在写入过程中累加字节数并回调。
type progressWriter struct {
	w          io.Writer
	done       int64
	onProgress func(int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.done += int64(n)
		if p.onProgress != nil {
			p.onProgress(p.done)
		}
	}
	return n, err
}

// percentOf 返回百分比，取值 0..100。total 未知（<=0）时返回 0——
// 显示一个凭空猜的百分比比不显示更糟。
func percentOf(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	p := float64(done) / float64(total) * 100
	if p > 100 {
		p = 100
	}
	if p < 0 {
		p = 0
	}
	return math.Round(p*10) / 10
}

// humanBytes 把字节数写成「12.3 MB」这种形式。
func humanBytes(n int64) string {
	const unit = 1 << 20
	if n < unit {
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
	return fmt.Sprintf("%.1f MB", float64(n)/unit)
}

func (u *Updater) fetchAsset(rel *release, name string) ([]byte, error) {
	asset := findAsset(rel, name)
	if asset == nil {
		return nil, fmt.Errorf("发布包 %s 里没有 %s", rel.TagName, name)
	}

	url := u.cfg.assetURL(asset.BrowserDownloadURL)
	if name == checksumFile && u.cfg.ChecksumURL != "" {
		// 校验值改从可信源取。这不是优化，是把 sha256 从「同源自证」变成
		// 「异源比对」——否则攻破镜像的人同时提供包和校验值就能绕过校验。
		url = u.cfg.ChecksumURL
	}
	if name == checksumFile {
		u.log("[Update] 校验值来源: %s", u.cfg.ChecksumURL)
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
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

// deployDirs 是在线更新要替换的**目录**，相对部署根目录。
//
// 整份换掉而不是覆盖：SvelteKit / Vite 的产物文件名带内容哈希，覆盖只会让上一版的
// 残渣逐版累积——仓库里曾攒到 140 个没有任何入口引用的旧文件，其中一个
// public/admin/build/ 子目录还被当成第二份后台、以 /admin/build/ 暴露出去。
//
// 按「整个 public 一份」而不是逐个站点替换，是因为目录枚举一旦漏掉一个站点，
// 漏掉的那个就会永远停在旧版本，而界面上看不出任何异常。
//
// 刻意不含 database/ 与 storage/：包里只有 .keep 占位，而这两个目录里是真实的
// 数据库与日志。也不含 start.sh / smart-mzcmc.service / .env.example——部署脚手架
// 由现场维护，自动覆盖可能把改过路径的 systemd unit 冲掉。
var deployDirs = []string{
	"public",
	"resources",
}

// public 必须带的文件。缺了就说明这个包没带前端，此时若继续更新，
// swapDir 会把现存的站点整个扬掉——后台直接 404。
//
// 三个站点都要查而不是只看 admin：docs 与 interviewer 也是后端托管的，
// 少一个就是那个页面 404，而现象同样是「更新完还是坏的」。
var deploySentinels = []string{
	"public/admin/index.html",
	"public/docs/index.html",
	"public/interviewer/index.html",
}

// deployRoot 解析部署根目录，也就是 public/ 与 resources/ 的父目录。
//
// 按**可执行文件所在目录**解析，与 main.go 里确定发布包根目录的规则一致：
// 发布包结构是「二进制 + public/ + resources/」，解压即可运行，所以 public 必须
// 相对可执行文件定位，而不是相对源码路径或工作目录。
//
// 此前这里用的是工作目录，而 routes/staticSite.go 里的 /admin、/docs 也是工作目录
// 相对的，于是「下载→校验→解包→替换程序」全程正常、只有站点纹丝不动——文件确实
// 换掉了，只是换在了另一套坐标指向的地方。全程没有任何报错，现场只能报一句
// 「不生效」。
//
// 工作目录与程序目录不一致时明确记一笔：这类问题不抛错、不失败，只表现为「没效
// 果」，不记下来就只能靠猜。
func deployRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("无法定位当前程序: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	if wd, err := os.Getwd(); err == nil {
		if wd != filepath.Dir(exe) {
			log.Printf("[Update] 注意：程序在 %s，工作目录是 %s。前端按程序所在目录部署到 %s",
				filepath.Dir(exe), wd, filepath.Join(filepath.Dir(exe), "public"))
		}
	}

	return filepath.Dir(exe), nil
}

// CleanupLeftovers 清掉上一次更新残留的暂存目录。
//
// 为什么要放在启动时而不是只在更新成功后：更新过程中进程被杀（systemd 超时、
// 断电、运维直接 kill）是常态，那一刻的 update/ 就成了孤儿——里面有几十 MB 的
// 压缩包、解出来的旧站点、换下来的上一版。它们既不会被用到，也不会自己消失，
// 下一轮更新又会在同一个目录上重新铺一遍。
//
// 刻意不动 <程序>.bak：它是这一版出问题之后唯一的回退路径，而且放在 update/
// 之外。也不动 <程序>.incoming：那是同一次更新正在写的临时文件，真被跑到就说明
// 有两个更新在并发，那属于另一个问题。
func CleanupLeftovers(root string) (string, error) {
	// 空串会让 updateDir 回退到工作目录，于是清掉的是另一处 update/——不如不清。
	// 这条守卫不常有，但 RemoveAll 没有第二次机会。
	if strings.TrimSpace(root) == "" {
		return "", errors.New("部署根目录为空，拒绝清理 update 目录")
	}
	dir := updateDir(root)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// 只删 update 目录本身，不碰它的父目录。updateDir 只会拼出 base 为 update 的
	// 路径，这里是最后一道闸：万一以后有人改了拼法，宁可不删。
	if filepath.Base(abs) != "update" {
		return "", fmt.Errorf("拒绝清理 %s：不是 update 目录", abs)
	}
	if _, err := os.Stat(abs); err != nil {
		if os.IsNotExist(err) {
			return abs, nil
		}
		return abs, err
	}
	return abs, os.RemoveAll(abs)
}

// updateDir 返回暂存目录：<部署根目录>/update。
//
// 入参是**部署根目录**（可执行文件所在目录），不是可执行文件路径也不是文件名。
// 传文件名会拼出工作目录下的 update/，与部署根分属两处——这类不一致不会报错，
// 只会让启动清理漏掉自己放过的那一半残渣。
func updateDir(root string) string {
	if root == "" {
		root = "."
	}
	return filepath.Join(root, "update")
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
