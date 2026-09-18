package models

import "time"

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
