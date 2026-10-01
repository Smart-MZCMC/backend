package models

import (
	"crypto/md5"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/goravel/framework/facades"
)

type User struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	Username    string `json:"username" gorm:"uniqueIndex;size:50;not null"`
	Password    string `json:"-" gorm:"not null"`
	DisplayName string `json:"display_name" gorm:"size:100"`
	Role        string `json:"role" gorm:"size:20;default:director"`
	// Email 同时是 WeAvatar 头像的取值依据，因此唯一（可为空）。
	// 写入前一律经 NormalizeEmail 归一化，唯一性检查与头像哈希用同一个值。
	Email string `json:"email" gorm:"size:255;uniqueIndex:users_email_unique"`
	// TokenVersion 改密码时递增。令牌里带上签发时的版本号，验签时与这里的值
	// 比对，不一致即视为已失效——改密码才能立刻让所有旧令牌作废，
	// 而不是等 JWT_TTL（默认 60 分钟）自然过期。
	TokenVersion int       `json:"-" gorm:"default:0"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (User) TableName() string {
	return "users"
}

// NormalizeEmail 把邮箱归一化为「去首尾空格 + 全小写」。
//
// 必须存归一化后的值，两个理由：
//  1. 唯一约束与头像哈希必须基于同一个值。否则 A@x.com 与 a@x.com 在库里是
//     两条记录，却算出同一个 MD5——两个人共用一张头像。
//  2. 绝大多数邮箱服务本身不区分大小写，不归一化就会出现「明明只有一个邮箱
//     却提示已被占用」。
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidEmail 校验邮箱格式。
//
// 刻意从简：只要求「@ 前非空、恰好一个 @、@ 后有一个点、点后非空、且不含
// 空白」。完整 RFC 5322 正则既难看又覆盖不了一切，而这里的邮箱只用于头像，
// 不用于投递，所以严格校验没有实际收益，却会误拒合法地址。
func ValidEmail(email string) bool {
	if email == "" || len(email) > 255 {
		return false
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return false
	}
	at := strings.Index(email, "@")
	if at <= 0 || at != strings.LastIndex(email, "@") {
		return false
	}
	domain := email[at+1:]
	dot := strings.LastIndex(domain, ".")
	// 点必须在 @ 之后，且点后与点前都要有内容（排除 a@b. 这种结尾形式）。
	if dot <= 0 || dot == len(domain)-1 {
		return false
	}
	return true
}

// AvatarURL 返回该用户的 WeAvatar 头像地址；未设置邮箱时返回空串。
//
// 为什么在后端算而不在前端：浏览器的 Web Crypto 只支持 SHA 系列，没有 MD5，
// 前端算就得引第三方依赖。放后端还顺带让 WeAvatar 域名变成可配置
// （config/avatar.go 的 AVATAR_BASE_URL），将来换镜像不用改前端。
//
// 注意这是 Gravatar 系服务的约定：MD5(去空格后的小写邮箱)。算错会静默返回
// 默认头像，所以规范化必须和写入时一致。
func (u User) AvatarURL() string {
	if strings.TrimSpace(u.Email) == "" {
		return ""
	}
	return avatarURLFor(u.Email)
}

// avatarURLFor 是 AvatarURL 的包级实现，方便测试直接调用。
func avatarURLFor(email string) string {
	base := strings.TrimRight(facades.Config().GetString("avatar.base_url",
		"https://weavatar.com/avatar"), "/")
	size := facades.Config().GetInt("avatar.size", 96)

	sum := md5.Sum([]byte(NormalizeEmail(email)))
	hash := hex.EncodeToString(sum[:])

	// 不传 d 参数：WeAvatar 对未注册的邮箱会返回一个字母头像（HTTP 200），
	// 而不是 404。那正好和前端自己画的首字母圆圈一致，不会出现破图。
	return base + "/" + hash + "?s=" + strconv.Itoa(size)
}
