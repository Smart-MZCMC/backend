package models

import "time"

// ProjectCamera 是某个项目的机位预设。
//
// 导播端的预设按钮此前是硬编码在 Dart 里的 10 个名字，换个场地就得改代码
// 重新构建。放进表之后由项目自己配置，界面按 SortOrder 渲染。
type ProjectCamera struct {
	ID        uint   `json:"id" gorm:"primaryKey"`
	ProjectID uint   `json:"project_id" gorm:"index;not null"`
	Name      string `json:"name" gorm:"size:100;not null"`
	// SortOrder 决定界面上的按钮顺序，允许重复（重复时按 ID 兜底排序）。
	SortOrder int       `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ProjectCamera) TableName() string {
	return "project_cameras"
}
