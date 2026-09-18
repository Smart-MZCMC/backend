package controllers

import (
	"strconv"
	"time"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

type LockController struct{}

func NewLockController() *LockController {
	return &LockController{}
}

const lockTTL = 90 * time.Second

func (c *LockController) Acquire(ctx http.Context) http.Response {
	projectID, _ := strconv.Atoi(ctx.Request().Route("projectId"))
	userID := ctx.Value("user_id").(uint)

	if projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的项目ID"})
	}

	var existing models.ProjectLock
	err := facades.Orm().Query().Where("project_id = ?", projectID).First(&existing)
	if err == nil {
		if time.Now().Before(existing.ExpireAt) {
			if existing.UserID == userID {
				return ctx.Response().Json(200, map[string]any{
					"message": "已持有控制权",
					"lock":    existing,
					"renewed": true,
				})
			}
			return ctx.Response().Json(409, map[string]any{
				"error":     "控制权已被其他导播持有",
				"holder_id": existing.UserID,
				"expire_at": existing.ExpireAt,
			})
		}
		facades.Orm().Query().Where("id = ?", existing.ID).Delete(&models.ProjectLock{})
	}

	lock := models.ProjectLock{
		ProjectID: uint(projectID),
		UserID:    userID,
		LockedAt:  time.Now(),
		ExpireAt:  time.Now().Add(lockTTL),
	}

	if err := facades.Orm().Query().Create(&lock); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "获取控制权失败"})
	}

	msg := models.Message{
		ProjectID: uint(projectID),
		SenderID:  userID,
		Type:      "lock_update",
		Content:   `{"action":"acquire","user_id":` + strconv.Itoa(int(userID)) + `}`,
	}
	facades.Orm().Query().Create(&msg)

	return ctx.Response().Json(200, map[string]any{
		"message": "获取控制权成功",
		"lock":    lock,
	})
}

func (c *LockController) Release(ctx http.Context) http.Response {
	projectID, _ := strconv.Atoi(ctx.Request().Route("projectId"))
	userID := ctx.Value("user_id").(uint)

	if projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "无效的项目ID"})
	}

	var lock models.ProjectLock
	if err := facades.Orm().Query().Where("project_id = ? AND user_id = ?", projectID, userID).First(&lock); err != nil {
		return ctx.Response().Json(404, map[string]any{"error": "未持有该控制权"})
	}

	facades.Orm().Query().Where("id = ?", lock.ID).Delete(&models.ProjectLock{})

	msg := models.Message{
		ProjectID: uint(projectID),
		SenderID:  userID,
		Type:      "lock_update",
		Content:   `{"action":"release","user_id":` + strconv.Itoa(int(userID)) + `}`,
	}
	facades.Orm().Query().Create(&msg)

	return ctx.Response().Json(200, map[string]any{"message": "已释放控制权"})
}

func (c *LockController) Heartbeat(ctx http.Context) http.Response {
	projectID, _ := strconv.Atoi(ctx.Request().Route("projectId"))
	userID := ctx.Value("user_id").(uint)

	var lock models.ProjectLock
	if err := facades.Orm().Query().Where("project_id = ? AND user_id = ?", projectID, userID).First(&lock); err != nil {
		return ctx.Response().Json(404, map[string]any{"error": "未持有控制权，需重新获取"})
	}

	lock.ExpireAt = time.Now().Add(lockTTL)
	facades.Orm().Query().Where("id = ?", lock.ID).Update(&lock)

	return ctx.Response().Json(200, map[string]any{
		"message":   "心跳已更新",
		"expire_at": lock.ExpireAt,
	})
}

func (c *LockController) Status(ctx http.Context) http.Response {
	projectID, _ := strconv.Atoi(ctx.Request().Route("projectId"))

	var lock models.ProjectLock
	if err := facades.Orm().Query().Where("project_id = ?", projectID).First(&lock); err != nil {
		return ctx.Response().Json(200, map[string]any{"locked": false})
	}

	if time.Now().After(lock.ExpireAt) {
		facades.Orm().Query().Where("id = ?", lock.ID).Delete(&models.ProjectLock{})
		return ctx.Response().Json(200, map[string]any{"locked": false})
	}

	return ctx.Response().Json(200, map[string]any{
		"locked":    true,
		"user_id":   lock.UserID,
		"locked_at": lock.LockedAt,
		"expire_at": lock.ExpireAt,
	})
}
