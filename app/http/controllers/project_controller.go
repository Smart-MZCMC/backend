package controllers

import (
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

// ProjectController 提供「非管理端」的项目视图。
//
// 存在的理由：导播端的项目下拉此前调的是 /api/admin/projects，而那条路由
// 当时挂在「管理员及以上」之后——导播的令牌根本拿不到数据，下拉框恒定
// 是空的。管理接口不该被非管理端复用。
//
// ⚠️ /api/admin/projects 现在要的是 project.view（全员持有，见 app/rbac），
// 导播端调它也能拿到数据了。但**非管理端仍然该走这一组**：它按
// user_projects 过滤出「我有权看的项目」，而 /api/admin/projects 是全量列表。
// 两条接口的语义不一样，不是同一条路由的两个名字。
type ProjectController struct{}

func NewProjectController() *ProjectController {
	return &ProjectController{}
}

// actorForProjectList 取出当前登录用户。
//
// **不能用 actorFrom**：那个函数读的是 ctx 里的 "user"，而 "user" 是
// RequirePermission / RequireRole 写进去的。这条路由只挂了 Jwt（任何人都能
// 列自己有权看的项目），Jwt 只写 "user_id"，于是 actorFrom 会一律返回
// 「未提供认证令牌」。
func actorForProjectList(ctx http.Context) (models.User, *authzError) {
	userID, ok := ctx.Value("user_id").(uint)
	if !ok || userID == 0 {
		return models.User{}, &authzError{401, "未提供认证令牌"}
	}
	// ID == 0 的判断不能省：First 查不到时不报错，只把结构体留成零值。
	var user models.User
	if err := facades.Orm().Query().Select("id", "username", "role").
		Where("id = ?", userID).First(&user); err != nil || user.ID == 0 {
		return models.User{}, &authzError{401, "用户不存在或已被删除"}
	}
	if !models.Role(user.Role).Valid() {
		return models.User{}, &authzError{403, "账号角色异常（" + user.Role + "），请联系超级管理员修复"}
	}
	return user, nil
}

// projectRank 给项目排序分组，数字越小越靠前。
//
// 导播端此前是 `projects.first`，也就是 ID 最小的那个，没有任何时间含义——
// 谁先建谁常驻，赛程换了还要手动改代码。改成按时间排之后，
// 「正在进行的」>「即将开始的」>「没有日程的」>「已结束/已取消」。
func projectRank(p models.Project, now time.Time) int {
	switch p.Status {
	case models.ProjectStatusFinished, models.ProjectStatusCancelled:
		return 3
	case models.ProjectStatusLive:
		if p.ScheduledStart != nil && p.ScheduledEnd != nil &&
			!now.Before(*p.ScheduledStart) && !now.After(*p.ScheduledEnd) {
			return 0
		}
		return 1
	default:
		if p.ScheduledStart != nil && p.ScheduledStart.After(now) {
			return 1
		}
		return 2
	}
}

// sortProjectsForDisplay 按「当前/下一场优先」排序。
//
// 在 Go 里排而不是写 SQL：SQLite 对 NULL 的排序位置与其他库不一致，
// 而这里的排序规则还牵扯到状态与时间窗两个字段，用 SQL 表达既难读也难改。
// 项目数量是个位数，内存里排没有性能问题。
func sortProjectsForDisplay(projects []models.Project) []models.Project {
	now := time.Now()
	out := make([]models.Project, len(projects))
	copy(out, projects)

	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := projectRank(out[i], now), projectRank(out[j], now)
		if ri != rj {
			return ri < rj
		}
		si, sj := out[i].ScheduledStart, out[j].ScheduledStart
		switch {
		case si != nil && sj != nil:
			if !si.Equal(*sj) {
				return si.Before(*sj)
			}
		case si != nil:
			return true
		case sj != nil:
			return false
		}
		// 都没排期时按创建时间倒序：最近建的更可能是当下在用的那个。
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// List 返回当前登录用户有权访问的项目。
//
// 管理员及以上拿到全部；其余角色只拿被授权的。**但当一个人一个项目都没被
// 授权时退回全部**——否则把这张表接上鉴权的第一刻，所有还没配过权限的存量
// 部署都会看到一个空下拉框，现场直接没法播。
func (c *ProjectController) List(ctx http.Context) http.Response {
	actor, aerr := actorForProjectList(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	var projects []models.Project
	if err := facades.Orm().Query().Find(&projects); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询项目失败"})
	}

	if !models.Role(actor.Role).AtLeast(models.RoleAdmin) {
		ids, err := models.ProjectIDsOf(actor.ID)
		if err != nil {
			return ctx.Response().Json(500, map[string]any{"error": "查询项目授权失败"})
		}
		if len(ids) > 0 {
			allowed := make(map[uint]bool, len(ids))
			for _, id := range ids {
				allowed[id] = true
			}
			filtered := make([]models.Project, 0, len(ids))
			for _, p := range projects {
				if allowed[p.ID] {
					filtered = append(filtered, p)
				}
			}
			projects = filtered
		}
	}

	return ctx.Response().Json(200, sortProjectsForDisplay(projects))
}

// Cameras 返回项目的机位预设，按配置顺序。
//
// 导播端此前把这 10 个名字硬编码在 Dart 里，换个场地就得改代码重新构建。
func (c *ProjectController) Cameras(ctx http.Context) http.Response {
	projectID, _ := strconv.Atoi(ctx.Request().Route("projectId"))
	if projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的项目ID"})
	}

	var cameras []models.ProjectCamera
	if err := facades.Orm().Query().
		Where("project_id = ?", projectID).
		OrderBy("sort_order").
		OrderBy("id").
		Find(&cameras); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询机位失败"})
	}

	return ctx.Response().Json(200, cameras)
}

// ShotCutSummary 是切台报表的汇总部分。
type ShotCutSummary struct {
	CutCount int `json:"cut_count"`
	// AvgDwellSeconds 是「平均停留时长」。最后一段没有后续切台，无从计算
	// 停留了多久，所以不参与平均——把它按「到现在为止」算会让这个数字
	// 随着你盯着屏幕看的时间不断变大。
	AvgDwellSeconds float64 `json:"avg_dwell_seconds"`
	// ByShot 按机位统计出现次数与平均停留时长，供「哪个机位占用最多」看。
	ByShot []ShotStat `json:"by_shot"`
	// ByMode 按 live / rehearsal 分场统计，彩排与正式可以分开看。
	ByMode []ModeStat `json:"by_mode"`
}

type ShotStat struct {
	Shot            string  `json:"shot"`
	Count           int     `json:"count"`
	AvgDwellSeconds float64 `json:"avg_dwell_seconds"`
}

type ModeStat struct {
	Mode     string `json:"mode"`
	CutCount int    `json:"cut_count"`
}

// ShotCuts 返回切台时间线与报表。
//
// 这张表（shot_cuts）是 B 组里性价比最高的一项：它同时解锁了切台时间线、
// 按机位/时段筛选、切台次数与平均停留时长、彩排与正式分场统计四件事，
// 而在此之前这些信息只存在于 messages.content 的一段不透明 JSON 里。
func (c *ProjectController) ShotCuts(ctx http.Context) http.Response {
	projectID, _ := strconv.Atoi(ctx.Request().Route("projectId"))
	if projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的项目ID"})
	}

	limit, _ := strconv.Atoi(ctx.Request().Input("limit", "500"))
	if limit <= 0 || limit > 2000 {
		limit = 500
	}

	query := facades.Orm().Query().Model(&models.ShotCut{}).
		Where("project_id = ?", projectID)

	if from, ok := parseTimeFilter(ctx.Request().Input("from", "")); ok {
		query = query.Where("cut_at >= ?", from)
	}
	if to, ok := parseTimeFilter(ctx.Request().Input("to", "")); ok {
		query = query.Where("cut_at <= ?", to)
	}
	if shot := ctx.Request().Input("shot", ""); shot != "" {
		query = query.Where("to_shot = ?", shot)
	}

	total, err := query.Count()
	if err != nil {
		log.Printf("[ShotCuts] 统计切台次数失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "查询切台记录失败"})
	}

	var cuts []models.ShotCut
	if err := query.OrderBy("cut_at").Limit(limit).Find(&cuts); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询切台记录失败"})
	}

	return ctx.Response().Json(200, map[string]any{
		"project_id": projectID,
		"total":      total,
		"cuts":       cuts,
		"summary":    buildShotCutSummary(cuts),
	})
}

// buildShotCutSummary 从时间线算出报表数字。
func buildShotCutSummary(cuts []models.ShotCut) ShotCutSummary {
	summary := ShotCutSummary{CutCount: len(cuts)}
	if len(cuts) == 0 {
		summary.ByShot = []ShotStat{}
		summary.ByMode = []ModeStat{}
		return summary
	}

	// 每一段的时长 = 下一条切台的时间 - 这一条切台的时间。
	// 用累计和除以段数，而不是逐个累加浮点数，减少误差。
	type acc struct {
		count int
		total float64
	}
	byShot := map[string]*acc{}
	var dwellTotal float64
	dwellCount := 0

	for i := range cuts {
		cut := cuts[i]
		a := byShot[cut.ToShot]
		if a == nil {
			a = &acc{}
			byShot[cut.ToShot] = a
		}
		a.count++
		if i+1 < len(cuts) {
			dwell := cuts[i+1].CutAt.Sub(cut.CutAt).Seconds()
			if dwell >= 0 {
				a.total += dwell
				dwellTotal += dwell
				dwellCount++
			}
		}
	}
	if dwellCount > 0 {
		summary.AvgDwellSeconds = dwellTotal / float64(dwellCount)
	}

	shots := make([]string, 0, len(byShot))
	for shot := range byShot {
		shots = append(shots, shot)
	}
	// 按次数倒序、同名按字典序，保证同样的输入永远得到同样的输出——
	// 报表每次刷新顺序都不一样的话，人就没法比对两次结果。
	sort.Slice(shots, func(i, j int) bool {
		if byShot[shots[i]].count != byShot[shots[j]].count {
			return byShot[shots[i]].count > byShot[shots[j]].count
		}
		return shots[i] < shots[j]
	})
	summary.ByShot = make([]ShotStat, 0, len(shots))
	for _, shot := range shots {
		a := byShot[shot]
		stat := ShotStat{Shot: shot, Count: a.count}
		// 只统计「后面还有切台」的那几段。被筛掉最后一段的机位分母为 0，
		// 平均值保持 0 而不是 NaN。
		segments := a.count
		if shot == cuts[len(cuts)-1].ToShot {
			segments--
		}
		if segments > 0 {
			stat.AvgDwellSeconds = a.total / float64(segments)
		}
		summary.ByShot = append(summary.ByShot, stat)
	}

	modeCount := map[string]int{}
	for _, cut := range cuts {
		mode := cut.Mode
		if mode == "" {
			mode = models.ProjectModeLive
		}
		modeCount[mode]++
	}
	modes := make([]string, 0, len(modeCount))
	for mode := range modeCount {
		modes = append(modes, mode)
	}
	sort.Strings(modes)
	summary.ByMode = make([]ModeStat, 0, len(modes))
	for _, mode := range modes {
		summary.ByMode = append(summary.ByMode, ModeStat{Mode: mode, CutCount: modeCount[mode]})
	}

	return summary
}
