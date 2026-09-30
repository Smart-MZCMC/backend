package controllers

import (
	"github.com/goravel/framework/contracts/http"

	"smart-mzcmc/app/models"
)

type RoleController struct{}

func NewRoleController() *RoleController {
	return &RoleController{}
}

// roleInfo 是一个角色的对外描述。
type roleInfo struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Level int    `json:"level"`
}

// List 返回全部合法角色，按权限从高到低。
//
// 挂在校验者权限门槛之上、任何登录用户都能读：管理前端要靠它渲染角色标签与
// 「新建用户」的下拉选项。前端自己维护一份映射的问题已经吃过亏——AppShell、
// 用户页、权限分配页各有一处 role === 'admin' ? '管理员' : '导播'，加入
// 超级管理员之后三处都会把超管显示成「导播」。
func (c *RoleController) List(ctx http.Context) http.Response {
	all := models.AllRoles()
	out := make([]roleInfo, 0, len(all))
	for _, r := range all {
		out = append(out, roleInfo{
			Value: string(r),
			Label: r.Label(),
			Level: r.Level(),
		})
	}
	return ctx.Response().Json(200, out)
}
