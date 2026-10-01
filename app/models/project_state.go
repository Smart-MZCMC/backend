package models

import "time"

// ProjectState 是每个项目「当前生效」的切台状态，一个项目一行。
//
// 存在的理由：切台状态此前只以一段不透明 JSON 存在 messages.content 里，
// 而且只在新 shot_state 到来时才广播一次。于是中途加入的解说端/包装端会
// 一直停在「等待导播指令」，重连同理——直到下一次切台才恢复。
// 落成一行可查的表之后，WebSocket 的欢迎消息就能把当前状态一并带上，
// 新连接（含重连）不必等下一次切台。
type ProjectState struct {
	// ProjectID 同时是主键：一个项目只保留最新状态，不留历史。
	// 历史在 shot_cuts 表里。
	ProjectID   uint      `json:"project_id" gorm:"primaryKey"`
	CurrentShot string    `json:"current_shot" gorm:"size:100"`
	NextShot    string    `json:"next_shot" gorm:"size:100"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (ProjectState) TableName() string {
	return "project_states"
}
