package models

import "time"

type InterviewStatus struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	ProjectID uint      `json:"project_id" gorm:"index;not null"`
	PointCode string    `json:"point_code" gorm:"size:50;not null"`
	PointName string    `json:"point_name" gorm:"size:100;not null"`
	Status    string    `json:"status" gorm:"size:20;default:offline"` // ready / preparing / not_ready / offline
	UpdatedAt time.Time `json:"updated_at"`
}

func (InterviewStatus) TableName() string {
	return "interview_status"
}
