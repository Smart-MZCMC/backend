package controllers

import (
	"strconv"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

type InterviewController struct{}

func NewInterviewController() *InterviewController {
	return &InterviewController{}
}

func (c *InterviewController) ListByProject(ctx http.Context) http.Response {
	projectID := ctx.Request().Route("projectId")

	var statuses []models.InterviewStatus
	if err := facades.Orm().Query().Where("project_id = ?", projectID).Find(&statuses); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询采访状态失败"})
	}

	return ctx.Response().Json(200, statuses)
}

func (c *InterviewController) UpdateStatus(ctx http.Context) http.Response {
	projectID, _ := strconv.Atoi(ctx.Request().Input("project_id", "0"))
	pointCode := ctx.Request().Input("point_code", "")
	pointName := ctx.Request().Input("point_name", "")
	status := ctx.Request().Input("status", "")

	if projectID == 0 || pointCode == "" || status == "" {
		return ctx.Response().Json(400, map[string]any{"error": "请求参数无效"})
	}

	validStatuses := map[string]bool{
		"ready": true, "preparing": true, "not_ready": true, "offline": true,
	}
	if !validStatuses[status] {
		return ctx.Response().Json(400, map[string]any{"error": "无效的状态值"})
	}

	var interview models.InterviewStatus
	err := facades.Orm().Query().
		Where("project_id = ? AND point_code = ?", projectID, pointCode).
		First(&interview)

	if err != nil {
		interview = models.InterviewStatus{
			ProjectID: uint(projectID),
			PointCode: pointCode,
			PointName: pointName,
			Status:    status,
		}
		if err := facades.Orm().Query().Create(&interview); err != nil {
			return ctx.Response().Json(500, map[string]any{"error": "创建采访状态失败"})
		}
	} else {
		updates := map[string]any{"status": status}
		if pointName != "" {
			updates["point_name"] = pointName
		}
		facades.Orm().Query().Where("id = ?", interview.ID).Update(updates)
		interview.Status = status
		if pointName != "" {
			interview.PointName = pointName
		}
	}

	msg := models.Message{
		ProjectID: uint(projectID),
		Type:      "interview_status",
		Content:   `{"point_code":"` + pointCode + `","status":"` + status + `"}`,
	}
	facades.Orm().Query().Create(&msg)

	return ctx.Response().Json(200, interview)
}
