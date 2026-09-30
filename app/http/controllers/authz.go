package controllers

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

// authzError 是一次授权失败的响应。
type authzError struct {
	status  int
	message string
}

// actorFrom 从上下文取出当前操作者。
//
// 前提是路由挂在 middleware.Jwt() 之后——它把 user 写进了 ctx。
func actorFrom(ctx http.Context) (models.User, *authzError) {
	user, ok := ctx.Value("user").(models.User)
	if !ok || user.ID == 0 {
		return models.User{}, &authzError{401, "未提供认证令牌"}
	}
	role := models.Role(user.Role)
	if !role.Valid() {
		return models.User{}, &authzError{403, "账号角色异常（" + user.Role + "），请联系超级管理员修复"}
	}
	return user, nil
}

// resolveActor 供公开路由使用：Register 挂在公开路由上，拿不到 Jwt 中间件
// 写入的上下文，所以自己再解析一次 Authorization 头。
func resolveActor(ctx http.Context, needLabel string) (models.User, *authzError) {
	raw := strings.TrimPrefix(ctx.Request().Header("Authorization", ""), "Bearer ")
	if raw == "" {
		return models.User{}, &authzError{401, needLabel + "需要管理员登录"}
	}

	secret := facades.Config().GetString("jwt.secret")
	if secret == "" {
		return models.User{}, &authzError{500, "JWT密钥未配置"}
	}

	token, err := jwt.Parse(raw, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return models.User{}, &authzError{401, "令牌无效或已过期，请重新登录"}
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return models.User{}, &authzError{401, "令牌解析失败"}
	}
	key, _ := claims["key"].(string)
	userID, err := strconv.ParseUint(key, 10, 64)
	if err != nil || userID == 0 {
		return models.User{}, &authzError{401, "无效的用户ID"}
	}

	var user models.User
	if err := facades.Orm().Query().Where("id = ?", userID).First(&user); err != nil {
		return models.User{}, &authzError{401, "用户不存在或已被删除"}
	}

	role := models.Role(user.Role)
	if !role.Valid() {
		return models.User{}, &authzError{403, "账号角色异常（" + user.Role + "），请联系超级管理员修复"}
	}
	return user, nil
}

// guardGrant 校验 actor 是否有权把 newRole 直接授予（新建账号时用）。
//
// 规则只有一条：不能授予高于自己的角色。管理员不能给自己或同事开超管，
// 否则「管理员」和「超级管理员」的分层就形同虚设——第一次管理员登录就能
// 把自己提上去，之后再没有区别。
func guardGrant(actor models.User, newRole models.Role) *authzError {
	actorRole := models.Role(actor.Role)
	if !actorRole.AtLeast(newRole) {
		return &authzError{403, "权限不足：不能授予高于自己的角色（" +
			newRole.Label() + "），你的角色为 " + actorRole.Label()}
	}
	return nil
}

// guardRoleChange 校验 actor 能否把 target 的角色改成 newRole。
//
// 判断本身拆成两步：先做不需要查库的纯逻辑（decideRoleChange），
// 再按需检查「最后一个超级管理员」。拆开是为了让前者能直接单测——
// 越权判断是最该有测试覆盖的地方，却不该为了测它去连数据库。
func guardRoleChange(actor, target models.User, newRole models.Role) *authzError {
	if aerr := decideRoleChange(actor, target, newRole); aerr != nil {
		return aerr
	}
	if models.Role(target.Role) == models.RoleSuperAdmin && newRole != models.RoleSuperAdmin {
		return guardLastSuperAdmin(target)
	}
	return nil
}

// decideRoleChange 是角色变更的纯逻辑判断，不做任何 IO。
//
// 三条规则，缺一条就会留下可利用的口子：
//  1. 只有管理员及以上能碰角色。等级高低决定的是「能用哪些功能」，
//     但改角色是对人的管理动作，不能因为「我等级比你高」就允许——
//     否则负责人能自己升成管理员，后勤能删掉导播账号。
//  2. 不能操作权限不低于自己的人。否则管理员可以把同事降级、把同级踢走。
//  3. 不能把自己降到低于自己。否则手滑点掉自己的权限就再也进不来，
//     超管想卸任也该由另一位超管来操作。
func decideRoleChange(actor, target models.User, newRole models.Role) *authzError {
	actorRole := models.Role(actor.Role)
	targetRole := models.Role(target.Role)

	if !actorRole.AtLeast(models.RoleAdmin) {
		return &authzError{403, "权限不足：只有管理员及以上可以调整角色"}
	}
	if !actorRole.AtLeast(targetRole) {
		return &authzError{403, "权限不足：不能修改权限不低于自己的账号（对方为 " +
			targetRole.Label() + "）"}
	}
	if actor.ID == target.ID && !newRole.AtLeast(actorRole) {
		return &authzError{400, "不能降低自己的权限，否则将无法再管理该系统"}
	}
	return guardGrant(actor, newRole)
}

// guardDeleteUser 校验 actor 能否删除 target。
func guardDeleteUser(actor, target models.User) *authzError {
	if aerr := decideDeleteUser(actor, target); aerr != nil {
		return aerr
	}
	if models.Role(target.Role) == models.RoleSuperAdmin {
		return guardLastSuperAdmin(target)
	}
	return nil
}

// decideDeleteUser 是删除账号的纯逻辑判断，不做任何 IO。
//
// 与 decideRoleChange 同理：删除账号属于管理动作，先要求管理员及以上，
// 再比相对等级。
func decideDeleteUser(actor, target models.User) *authzError {
	actorRole := models.Role(actor.Role)
	targetRole := models.Role(target.Role)

	if !actorRole.AtLeast(models.RoleAdmin) {
		return &authzError{403, "权限不足：只有管理员及以上可以删除账号"}
	}
	if actor.ID == target.ID {
		return &authzError{400, "不能删除自己的账号"}
	}
	if !actorRole.AtLeast(targetRole) {
		return &authzError{403, "权限不足：不能删除权限不低于自己的账号（对方为 " +
			targetRole.Label() + "）"}
	}
	return nil
}

// guardLastSuperAdmin 拦住「把最后一个超级管理员降级或删除」。
//
// 没有这道检查，系统会被锁死：没有任何账号能管用户、能改角色、能做系统更新，
// 只能回到服务器上手改数据库。
func guardLastSuperAdmin(target models.User) *authzError {
	count, err := facades.Orm().Query().Model(&models.User{}).
		Where("role = ?", string(models.RoleSuperAdmin)).Count()
	if err != nil {
		return &authzError{500, "查询超级管理员数量失败"}
	}
	if count <= 1 {
		return &authzError{400, "系统至少需要保留一个超级管理员，请先指定他人后再操作"}
	}
	return nil
}

// auditAction 记录一次敏感操作（系统更新、配置变更等）。
func auditAction(actor models.User, action string) {
	log.Printf("[AUTHZ] 操作者 %s(#%d, %s) 执行 %s",
		actor.Username, actor.ID, models.Role(actor.Role).Label(), action)
}

// auditRoleChange 记录角色与账号的变更。
//
// 这些是「谁能操作这套系统」的决定，出事时唯一的线索就是日志。
// 用标准库 log，与本项目其他控制器保持一致。
func auditRoleChange(actor, target models.User, action string) {
	log.Printf("[AUTHZ] 操作者 %s(#%d, %s) 对 %s(#%d, %s) 执行 %s",
		actor.Username, actor.ID, models.Role(actor.Role).Label(),
		target.Username, target.ID, models.Role(target.Role).Label(),
		action)
}
