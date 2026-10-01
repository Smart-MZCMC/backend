package models

import (
	"time"

	"github.com/goravel/framework/facades"
)

type UserProject struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	UserID    uint      `json:"user_id" gorm:"index;not null"`
	ProjectID uint      `json:"project_id" gorm:"index;not null"`
	CreatedAt time.Time `json:"created_at"`

	User    User    `json:"user,omitempty" gorm:"foreignKey:UserID"`
	Project Project `json:"project,omitempty" gorm:"foreignKey:ProjectID"`
}

func (UserProject) TableName() string {
	return "user_projects"
}

// IsProjectMember 报告用户是否被授权访问某个项目。
//
// 这张表从第一天就在，但此前只被管理接口增删查，从未决定过任何人能看什么：
// 后勤账号能看到全部项目列表，控制权接口只从 URL 取 projectId，
// WebSocket 更是知道 project_id 就能监听整个项目。这个函数就是它第一次
// 真正参与鉴权的地方。
//
// 查询失败时返回 false（宁可多拦一次，也不要在库出问题时放行）。
func IsProjectMember(userID, projectID uint) bool {
	if userID == 0 || projectID == 0 {
		return false
	}
	count, err := facades.Orm().Query().Model(&UserProject{}).
		Where("user_id = ? AND project_id = ?", userID, projectID).Count()
	if err != nil {
		return false
	}
	return count > 0
}

// ProjectIDsOf 返回用户被授权的全部项目 ID，用于「只列出我能看的项目」。
func ProjectIDsOf(userID uint) ([]uint, error) {
	if userID == 0 {
		return nil, nil
	}
	var links []UserProject
	if err := facades.Orm().Query().Select("project_id").
		Where("user_id = ?", userID).Find(&links); err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(links))
	for _, link := range links {
		ids = append(ids, link.ProjectID)
	}
	return ids, nil
}
