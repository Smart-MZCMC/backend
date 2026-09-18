package models

import "time"

type Message struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	ProjectID uint      `json:"project_id" gorm:"index;not null"`
	SenderID  uint      `json:"sender_id" gorm:"index"`
	Type      string    `json:"type" gorm:"size:30;not null"`
	Content   string    `json:"content" gorm:"type:text"`
	CreatedAt time.Time `json:"created_at"`

	Sender *User `json:"sender,omitempty" gorm:"foreignKey:SenderID"`
}

func (Message) TableName() string {
	return "messages"
}
