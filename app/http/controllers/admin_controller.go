package controllers

import (
	"strconv"
	"strings"
	"time"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/audit"
	"smart-mzcmc/app/models"
)

type AdminController struct{}

func NewAdminController() *AdminController {
	return &AdminController{}
}

// --- User Management ---

func (c *AdminController) ListUsers(ctx http.Context) http.Response {
	var users []models.User
	if err := facades.Orm().Query().Select("id", "username", "display_name", "role", "created_at").Find(&users); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询用户失败"})
	}

	// 一并给出中文角色名。前端不需要再各自维护一份映射——之前
	// AppShell、用户页、权限分配页各写了一个 admin ? '管理员' : '导播'
	// 的三元表达式，加了 super_admin 之后全部会把超管显示成「导播」。
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		role := models.Role(u.Role)
		out = append(out, map[string]any{
			"id":           u.ID,
			"username":     u.Username,
			"display_name": u.DisplayName,
			"role":         u.Role,
			"role_label":   role.Label(),
			"created_at":   u.CreatedAt,
		})
	}
	return ctx.Response().Json(200, out)
}

func (c *AdminController) DeleteUser(ctx http.Context) http.Response {
	id, _ := strconv.Atoi(ctx.Request().Route("id"))
	if id == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的用户ID"})
	}

	actor, aerr := actorFrom(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	target, err := loadUser(id)
	if err != nil {
		return ctx.Response().Json(404, map[string]any{"error": "用户不存在"})
	}

	if gerr := guardDeleteUser(actor, target); gerr != nil {
		return ctx.Response().Json(gerr.status, map[string]any{"error": gerr.message})
	}

	if _, err := facades.Orm().Query().Where("id = ?", id).Delete(&models.User{}); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "删除用户失败"})
	}

	facades.Orm().Query().Where("user_id = ?", id).Delete(&models.UserProject{})
	facades.Orm().Query().Where("user_id = ?", id).Delete(&models.ProjectLock{})

	auditRoleChange(ctx, actor, target, "删除用户")
	return ctx.Response().Json(200, map[string]any{"message": "删除成功"})
}

func (c *AdminController) UpdateUserRole(ctx http.Context) http.Response {
	id, _ := strconv.Atoi(ctx.Request().Route("id"))
	roleInput := ctx.Request().Input("role", "")

	if id == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的用户ID"})
	}

	newRole := models.Role(roleInput)
	if !newRole.Valid() {
		return ctx.Response().Json(400, map[string]any{
			"error": "角色非法，可选值：" + roleOptionsText(),
		})
	}

	actor, aerr := actorFrom(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	target, err := loadUser(id)
	if err != nil {
		return ctx.Response().Json(404, map[string]any{"error": "用户不存在"})
	}

	// 四条越权/锁死防护都在这里：不能改同级或更高、不能自降权、
	// 不能授予高于自己的角色、不能动最后一个超管。少一条就有可利用的口子。
	if gerr := guardRoleChange(actor, target, newRole); gerr != nil {
		return ctx.Response().Json(gerr.status, map[string]any{"error": gerr.message})
	}

	if _, err := facades.Orm().Query().Where("id = ?", id).
		Update(&models.User{Role: string(newRole)}); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "更新角色失败"})
	}

	auditRoleChange(ctx, actor, target, "把角色改为 "+newRole.Label())
	return ctx.Response().Json(200, map[string]any{"message": "更新成功"})
}

// loadUser 按 ID 取用户。
func loadUser(id int) (models.User, error) {
	var user models.User
	if err := facades.Orm().Query().Where("id = ?", id).First(&user); err != nil || user.ID == 0 {
		return models.User{}, err
	}
	return user, nil
}

// --- Project Management ---

func (c *AdminController) ListProjects(ctx http.Context) http.Response {
	var projects []models.Project
	if err := facades.Orm().Query().Find(&projects); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询项目失败"})
	}
	return ctx.Response().Json(200, sortProjectsForDisplay(projects))
}

// projectPayload 是创建/更新项目共用的字段解析结果。
//
// 单独抽出来是因为两个接口要接受的字段完全一样，各写一份必然会出现
// 「创建能设日程、更新不能」这类只在一半路径上生效的缺口。
type projectPayload struct {
	Name           string
	Code           string
	Description    *string
	Venue          *string
	ScheduledStart *time.Time
	ClearStart     bool
	ScheduledEnd   *time.Time
	ClearEnd       bool
	OwnerID        *uint
	Status         *string
	Mode           *string
}

// parseProjectPayload 解析项目字段。
//
// **用「键是否存在」而不是「值是否为空」判断要不要更新**，这是本函数存在
// 的全部理由：之前 UpdateProject 写的是 `if description != ""`，于是传空串
// 会被静默忽略——描述一旦设过就再也清不掉。前端还专门为这个 bug 加了一句
// 「后端会忽略空描述，清空后无法保存」的提示。同一个坑在邮箱字段上踩过一次
// （见 UpdateProfile 里对 All() 的用法）。
//
// onlyCreate 为 true 时（创建）不做「键是否存在」的判断，一律取默认值。
func parseProjectPayload(ctx http.Context, onlyCreate bool) (projectPayload, string) {
	input := ctx.Request().All()
	payload := projectPayload{}

	if onlyCreate || hasKey(input, "name") {
		payload.Name = strings.TrimSpace(ctx.Request().Input("name", ""))
	}
	if onlyCreate || hasKey(input, "code") {
		payload.Code = strings.TrimSpace(ctx.Request().Input("code", ""))
	}
	if onlyCreate || hasKey(input, "description") {
		desc := strings.TrimSpace(ctx.Request().Input("description", ""))
		payload.Description = &desc
	}
	if onlyCreate || hasKey(input, "venue") {
		venue := strings.TrimSpace(ctx.Request().Input("venue", ""))
		payload.Venue = &venue
	}

	for _, field := range []string{"scheduled_start", "scheduled_end"} {
		if !onlyCreate && !hasKey(input, field) {
			continue
		}
		raw := ctx.Request().Input(field, "")
		parsed, ok := parseOptionalTime(raw)
		if !ok {
			return projectPayload{}, field + " 时间格式不正确（示例：2026-05-20T09:00）"
		}
		if field == "scheduled_start" {
			payload.ScheduledStart = parsed
			payload.ClearStart = parsed == nil
		} else {
			payload.ScheduledEnd = parsed
			payload.ClearEnd = parsed == nil
		}
	}

	if onlyCreate || hasKey(input, "owner_id") {
		raw := ctx.Request().Input("owner_id", "0")
		owner, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return projectPayload{}, "owner_id 必须是数字"
		}
		ownerID := uint(owner)
		payload.OwnerID = &ownerID
	}

	if onlyCreate || hasKey(input, "status") {
		status := strings.TrimSpace(ctx.Request().Input("status", ""))
		if status == "" {
			status = models.ProjectStatusPlanned
		}
		if !models.ValidProjectStatus(status) {
			return projectPayload{}, "项目状态非法，可选值：" + strings.Join(models.ProjectStatuses, " / ")
		}
		payload.Status = &status
	}

	if onlyCreate || hasKey(input, "mode") {
		mode := strings.TrimSpace(ctx.Request().Input("mode", ""))
		if mode == "" {
			mode = models.ProjectModeLive
		}
		if !models.ValidProjectMode(mode) {
			return projectPayload{}, "项目模式非法，可选值：" + strings.Join(models.ProjectModes, " / ")
		}
		payload.Mode = &mode
	}

	return payload, ""
}

// hasKey 判断请求体里是否显式出现了某个字段。
//
// Goravel 的 ContextRequest 没有 Has()，只能用 All() 判键是否存在——
// 这是区分「传了空串（= 清空）」与「没传（= 不动）」的唯一办法。
func hasKey(input map[string]any, key string) bool {
	_, ok := input[key]
	return ok
}

// applyProjectPayload 把解析结果写进模型（仅创建时使用）。
func applyProjectPayload(project *models.Project, payload projectPayload) {
	if payload.Description != nil {
		project.Description = *payload.Description
	}
	if payload.Venue != nil {
		project.Venue = *payload.Venue
	}
	project.ScheduledStart = payload.ScheduledStart
	project.ScheduledEnd = payload.ScheduledEnd
	if payload.OwnerID != nil {
		project.OwnerID = *payload.OwnerID
	}
	project.Status = models.ProjectStatusPlanned
	if payload.Status != nil {
		project.Status = *payload.Status
	}
	project.Mode = models.ProjectModeLive
	if payload.Mode != nil {
		project.Mode = *payload.Mode
	}
}

// parseOptionalTime 解析可为空的时间字段。空串返回 (nil, true) 表示「清空」。
func parseOptionalTime(raw string) (*time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return &t, true
		}
	}
	return nil, false
}

func (c *AdminController) CreateProject(ctx http.Context) http.Response {
	payload, errText := parseProjectPayload(ctx, true)
	if errText != "" {
		return ctx.Response().Json(400, map[string]any{"error": errText})
	}
	if payload.Name == "" || payload.Code == "" {
		return ctx.Response().Json(400, map[string]any{"error": "项目名称和编码不能为空"})
	}

	actor, aerr := actorFrom(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	project := models.Project{
		Name: payload.Name,
		Code: payload.Code,
	}
	applyProjectPayload(&project, payload)

	if err := facades.Orm().Query().Create(&project); err != nil {
		return ctx.Response().Json(409, map[string]any{"error": "项目编码已存在"})
	}

	// 新项目默认给一套机位预设，否则导播端打开就是空按钮区。
	seedDefaultCameras(project.ID)

	recordAudit(ctx, actor, audit.Record{
		Action:     "project.create",
		Summary:    "创建项目 " + project.Name,
		TargetType: "project",
		TargetID:   strconv.FormatUint(uint64(project.ID), 10),
		Detail:     map[string]any{"code": project.Code},
	})

	return ctx.Response().Json(201, project)
}

func (c *AdminController) UpdateProject(ctx http.Context) http.Response {
	id, _ := strconv.Atoi(ctx.Request().Route("id"))
	if id == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的项目ID"})
	}

	actor, aerr := actorFrom(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	payload, errText := parseProjectPayload(ctx, false)
	if errText != "" {
		return ctx.Response().Json(400, map[string]any{"error": errText})
	}

	updates := map[string]any{}
	// 名称不能改成空——项目名是列表里唯一的可读标识，清空后只剩一个编号。
	if payload.Name != "" {
		updates["name"] = payload.Name
	}
	// 其余字段：只要请求里出现了这个键就写进去，**包括空串**。
	if payload.Description != nil {
		updates["description"] = *payload.Description
	}
	if payload.Venue != nil {
		updates["venue"] = *payload.Venue
	}
	if payload.ClearStart {
		updates["scheduled_start"] = nil
	} else if payload.ScheduledStart != nil {
		updates["scheduled_start"] = *payload.ScheduledStart
	}
	if payload.ClearEnd {
		updates["scheduled_end"] = nil
	} else if payload.ScheduledEnd != nil {
		updates["scheduled_end"] = *payload.ScheduledEnd
	}
	if payload.OwnerID != nil {
		updates["owner_id"] = *payload.OwnerID
	}
	if payload.Status != nil {
		updates["status"] = *payload.Status
	}
	if payload.Mode != nil {
		updates["mode"] = *payload.Mode
	}

	if len(updates) > 0 {
		// map 形式的 Update 必须先 Model()，否则报 Table not set（见陷阱 2）。
		if _, err := facades.Orm().Query().Model(&models.Project{}).
			Where("id = ?", id).Update(updates); err != nil {
			return ctx.Response().Json(500, map[string]any{"error": "更新项目失败"})
		}
	}

	recordAudit(ctx, actor, audit.Record{
		Action:     "project.update",
		Summary:    "更新项目字段",
		TargetType: "project",
		TargetID:   strconv.Itoa(id),
		Detail:     updates,
	})

	return ctx.Response().Json(200, map[string]any{"message": "更新成功"})
}

func (c *AdminController) DeleteProject(ctx http.Context) http.Response {
	id, _ := strconv.Atoi(ctx.Request().Route("id"))
	if id == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的项目ID"})
	}

	actor, aerr := actorFrom(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	var project models.Project
	projectName := ""
	if err := facades.Orm().Query().Select("id", "name").Where("id = ?", id).
		First(&project); err == nil && project.ID != 0 {
		projectName = project.Name
	}

	if _, err := facades.Orm().Query().Where("id = ?", id).Delete(&models.Project{}); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "删除项目失败"})
	}

	// 关联数据一并清掉。新增的 project_cameras / project_states / shot_cuts
	// 如果不在这里删，删项目会留下永远没人读的孤儿行。
	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.UserProject{})
	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.ProjectLock{})
	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.InterviewStatus{})
	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.Message{})
	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.ProjectCamera{})
	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.ProjectState{})
	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.ShotCut{})

	recordAudit(ctx, actor, audit.Record{
		Action:     "project.delete",
		Summary:    "删除项目 " + projectName,
		TargetType: "project",
		TargetID:   strconv.Itoa(id),
	})

	return ctx.Response().Json(200, map[string]any{"message": "删除成功"})
}

// --- User-Project Assignment ---

func (c *AdminController) AssignProject(ctx http.Context) http.Response {
	userID, _ := strconv.Atoi(ctx.Request().Input("user_id", "0"))
	projectID, _ := strconv.Atoi(ctx.Request().Input("project_id", "0"))

	if userID == 0 || projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "user_id 和 project_id 不能为空"})
	}

	actor, aerr := actorFrom(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	// 先确认两边都存在：授权记录本身没有外键约束，写进一个不存在的
	// project_id 会让后台显示一条指向「项目 #999」的幽灵授权。
	if _, err := loadUser(userID); err != nil {
		return ctx.Response().Json(404, map[string]any{"error": "用户不存在"})
	}
	var project models.Project
	if err := facades.Orm().Query().Select("id").Where("id = ?", projectID).
		First(&project); err != nil || project.ID == 0 {
		return ctx.Response().Json(404, map[string]any{"error": "项目不存在"})
	}

	up := models.UserProject{
		UserID:    uint(userID),
		ProjectID: uint(projectID),
	}

	if err := facades.Orm().Query().Create(&up); err != nil {
		return ctx.Response().Json(409, map[string]any{"error": "已存在该授权记录"})
	}

	// 授权与撤销是「谁能看哪个项目」的决定，而 A2 之后这张表真的参与鉴权，
	// 所以必须留痕。
	recordAudit(ctx, actor, audit.Record{
		Action:     "project.grant",
		Summary:    "授予项目访问权限",
		TargetType: "user_project",
		TargetID:   strconv.Itoa(userID) + ":" + strconv.Itoa(projectID),
		Detail:     map[string]any{"user_id": userID, "project_id": projectID},
	})

	return ctx.Response().Json(201, up)
}

func (c *AdminController) RevokeProject(ctx http.Context) http.Response {
	userID, _ := strconv.Atoi(ctx.Request().Input("user_id", "0"))
	projectID, _ := strconv.Atoi(ctx.Request().Input("project_id", "0"))

	if userID == 0 || projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "参数无效"})
	}

	actor, aerr := actorFrom(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	if _, err := facades.Orm().Query().Where("user_id = ? AND project_id = ?", userID, projectID).Delete(&models.UserProject{}); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "撤销授权失败"})
	}

	recordAudit(ctx, actor, audit.Record{
		Action:     "project.revoke",
		Summary:    "撤销项目访问权限",
		TargetType: "user_project",
		TargetID:   strconv.Itoa(userID) + ":" + strconv.Itoa(projectID),
		Detail:     map[string]any{"user_id": userID, "project_id": projectID},
	})

	return ctx.Response().Json(200, map[string]any{"message": "撤销成功"})
}

func (c *AdminController) ListUserProjects(ctx http.Context) http.Response {
	userID := ctx.Request().Route("id")

	var ups []models.UserProject
	if err := facades.Orm().Query().Where("user_id = ?", userID).Find(&ups); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询失败"})
	}

	return ctx.Response().Json(200, ups)
}

// --- 机位预设 ---

// defaultCameraNames 与导播端原先硬编码的那 10 个名字一致。
// 迁移里也给存量项目灌过同样一份，两处必须保持同步。
var defaultCameraNames = []string{
	"全景", "50米", "100米", "1000米", "20×50接力",
	"跳远", "跳高", "跳长绳", "韵律操", "领导讲话",
}

// seedDefaultCameras 给新项目播下一套默认机位。
func seedDefaultCameras(projectID uint) {
	for i, name := range defaultCameraNames {
		camera := models.ProjectCamera{
			ProjectID: projectID,
			Name:      name,
			SortOrder: i,
		}
		if err := facades.Orm().Query().Create(&camera); err != nil {
			// 机位只是预设，建不出来不该让项目创建失败。
			return
		}
	}
}

func (c *AdminController) CreateCamera(ctx http.Context) http.Response {
	projectID, _ := strconv.Atoi(ctx.Request().Route("id"))
	if projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的项目ID"})
	}
	name := strings.TrimSpace(ctx.Request().Input("name", ""))
	if name == "" {
		return ctx.Response().Json(400, map[string]any{"error": "机位名称不能为空"})
	}

	sortOrder, _ := strconv.Atoi(ctx.Request().Input("sort_order", "0"))
	// 不传顺序时排到最后，而不是插到最前面把已有顺序全部挤乱。
	if ctx.Request().Input("sort_order", "") == "" {
		count, _ := facades.Orm().Query().Model(&models.ProjectCamera{}).
			Where("project_id = ?", projectID).Count()
		sortOrder = int(count)
	}

	camera := models.ProjectCamera{
		ProjectID: uint(projectID),
		Name:      name,
		SortOrder: sortOrder,
	}
	if err := facades.Orm().Query().Create(&camera); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "创建机位失败"})
	}

	if actor, aerr := actorFrom(ctx); aerr == nil {
		recordAudit(ctx, actor, audit.Record{
			Action:     "camera.create",
			Summary:    "新增机位 " + camera.Name,
			TargetType: "project_camera",
			TargetID:   strconv.FormatUint(uint64(camera.ID), 10),
			Detail:     map[string]any{"project_id": projectID},
		})
	}

	return ctx.Response().Json(201, camera)
}

func (c *AdminController) UpdateCamera(ctx http.Context) http.Response {
	id, _ := strconv.Atoi(ctx.Request().Route("cameraId"))
	if id == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的机位ID"})
	}

	input := ctx.Request().All()
	updates := map[string]any{}
	if hasKey(input, "name") {
		name := strings.TrimSpace(ctx.Request().Input("name", ""))
		if name == "" {
			return ctx.Response().Json(400, map[string]any{"error": "机位名称不能为空"})
		}
		updates["name"] = name
	}
	if hasKey(input, "sort_order") {
		order, err := strconv.Atoi(ctx.Request().Input("sort_order", "0"))
		if err != nil {
			return ctx.Response().Json(400, map[string]any{"error": "sort_order 必须是数字"})
		}
		updates["sort_order"] = order
	}
	if len(updates) == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "没有需要更新的内容"})
	}

	if _, err := facades.Orm().Query().Model(&models.ProjectCamera{}).
		Where("id = ?", id).Update(updates); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "更新机位失败"})
	}

	if actor, aerr := actorFrom(ctx); aerr == nil {
		recordAudit(ctx, actor, audit.Record{
			Action:     "camera.update",
			Summary:    "更新机位",
			TargetType: "project_camera",
			TargetID:   strconv.Itoa(id),
			Detail:     updates,
		})
	}

	return ctx.Response().Json(200, map[string]any{"message": "更新成功"})
}

func (c *AdminController) DeleteCamera(ctx http.Context) http.Response {
	id, _ := strconv.Atoi(ctx.Request().Route("cameraId"))
	if id == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的机位ID"})
	}

	if _, err := facades.Orm().Query().Where("id = ?", id).
		Delete(&models.ProjectCamera{}); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "删除机位失败"})
	}

	if actor, aerr := actorFrom(ctx); aerr == nil {
		recordAudit(ctx, actor, audit.Record{
			Action:     "camera.delete",
			Summary:    "删除机位",
			TargetType: "project_camera",
			TargetID:   strconv.Itoa(id),
		})
	}

	return ctx.Response().Json(200, map[string]any{"message": "删除成功"})
}
