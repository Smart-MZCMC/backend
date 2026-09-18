package models

import "time"

type ProjectLock struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	ProjectID uint      `json:"project_id" gorm:"uniqueIndex;not null"`
	UserID    uint      `json:"user_id" gorm:"not null"`
	LockedAt  time.Time `json:"locked_at"`
	ExpireAt  time.Time `json:"expire_at"`

	User User `json:"user,omitempty" gorm:"foreignKey:UserID"`
}

func (ProjectLock) TableName() string {
	return "project_locks"
}
