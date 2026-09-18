package models

import "time"

type Project struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	Name        string    `json:"name" gorm:"size:100;not null"`
	Code        string    `json:"code" gorm:"uniqueIndex;size:50;not null"`
	Description string    `json:"description" gorm:"size:500"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (Project) TableName() string {
	return "projects"
}
