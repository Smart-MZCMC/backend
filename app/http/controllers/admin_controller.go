package controllers

import (
	"strconv"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

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

	auditRoleChange(actor, target, "删除用户")
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

	auditRoleChange(actor, target, "把角色改为 "+newRole.Label())
	return ctx.Response().Json(200, map[string]any{"message": "更新成功"})
}

// loadUser 按 ID 取用户。
func loadUser(id int) (models.User, error) {
	var user models.User
	if err := facades.Orm().Query().Where("id = ?", id).First(&user); err != nil {
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
	return ctx.Response().Json(200, projects)
}

func (c *AdminController) CreateProject(ctx http.Context) http.Response {
	name := ctx.Request().Input("name", "")
	code := ctx.Request().Input("code", "")
	description := ctx.Request().Input("description", "")

	if name == "" || code == "" {
		return ctx.Response().Json(400, map[string]any{"error": "项目名称和编码不能为空"})
	}

	project := models.Project{
		Name:        name,
		Code:        code,
		Description: description,
	}

	if err := facades.Orm().Query().Create(&project); err != nil {
		return ctx.Response().Json(409, map[string]any{"error": "项目编码已存在"})
	}

	return ctx.Response().Json(201, project)
}

func (c *AdminController) UpdateProject(ctx http.Context) http.Response {
	id, _ := strconv.Atoi(ctx.Request().Route("id"))
	name := ctx.Request().Input("name", "")
	description := ctx.Request().Input("description", "")

	updates := map[string]any{}
	if name != "" {
		updates["name"] = name
	}
	if description != "" {
		updates["description"] = description
	}

	if len(updates) > 0 {
		if _, err := facades.Orm().Query().Where("id = ?", id).Update(updates); err != nil {
			return ctx.Response().Json(500, map[string]any{"error": "更新项目失败"})
		}
	}

	return ctx.Response().Json(200, map[string]any{"message": "更新成功"})
}

func (c *AdminController) DeleteProject(ctx http.Context) http.Response {
	id, _ := strconv.Atoi(ctx.Request().Route("id"))

	if _, err := facades.Orm().Query().Where("id = ?", id).Delete(&models.Project{}); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "删除项目失败"})
	}

	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.UserProject{})
	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.ProjectLock{})
	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.InterviewStatus{})
	facades.Orm().Query().Where("project_id = ?", id).Delete(&models.Message{})

	return ctx.Response().Json(200, map[string]any{"message": "删除成功"})
}

// --- User-Project Assignment ---

func (c *AdminController) AssignProject(ctx http.Context) http.Response {
	userID, _ := strconv.Atoi(ctx.Request().Input("user_id", "0"))
	projectID, _ := strconv.Atoi(ctx.Request().Input("project_id", "0"))

	if userID == 0 || projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "user_id 和 project_id 不能为空"})
	}

	up := models.UserProject{
		UserID:    uint(userID),
		ProjectID: uint(projectID),
	}

	if err := facades.Orm().Query().Create(&up); err != nil {
		return ctx.Response().Json(409, map[string]any{"error": "已存在该授权记录"})
	}

	return ctx.Response().Json(201, up)
}

func (c *AdminController) RevokeProject(ctx http.Context) http.Response {
	userID, _ := strconv.Atoi(ctx.Request().Input("user_id", "0"))
	projectID, _ := strconv.Atoi(ctx.Request().Input("project_id", "0"))

	if userID == 0 || projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "参数无效"})
	}

	if _, err := facades.Orm().Query().Where("user_id = ? AND project_id = ?", userID, projectID).Delete(&models.UserProject{}); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "撤销授权失败"})
	}

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
