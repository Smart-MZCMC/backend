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

	// 判断该采访点是否已有记录。
	//
	// **不能靠 First 的 error**：SQLite 驱动下查不到记录时不返回错误，只把结构体
	// 留成零值，于是原本该走 Create 的分支被跳过，变成对着 id=0 更新 0 行——
	// 接口返回 200 却什么都没写入，采访端上报的状态导演播端根本收不到。
	// 用 Count 明确判断存在性。
	existing, err := facades.Orm().Query().Model(&models.InterviewStatus{}).
		Where("project_id = ? AND point_code = ?", projectID, pointCode).Count()
	if err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询采访状态失败"})
	}

	// 两条分支都要落到下面去广播消息，所以这里用一个外层变量承接，
	// 不能在 create 分支里提前 return —— 那会漏掉首次上报的推送。
	var interview models.InterviewStatus

	if existing == 0 {
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
		if err := facades.Orm().Query().
			Where("project_id = ? AND point_code = ?", projectID, pointCode).
			First(&interview); err != nil || interview.ID == 0 {
			return ctx.Response().Json(500, map[string]any{"error": "读取采访状态失败"})
		}

		updates := map[string]any{"status": status}
		if pointName != "" {
			updates["point_name"] = pointName
		}
		if _, err := facades.Orm().Query().Model(&models.InterviewStatus{}).
			Where("id = ?", interview.ID).Update(updates); err != nil {
			// 之前这里是裸调用、错误被丢弃，于是更新失败也无从得知。
			return ctx.Response().Json(500, map[string]any{"error": "更新采访状态失败"})
		}
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
