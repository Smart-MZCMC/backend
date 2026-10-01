package models

import "time"

// ShotCut 是「导播确认已切」这一动作的流水记录。
//
// 之前切台只写进 messages.content 的不透明 JSON，既不能按时间查，也不能按
// 机位查。切台时间线、按机位/时段筛选、切台次数与平均停留时长报表、彩排与
// 正式分场对比，全都要靠这张结构化的表。
//
// 只在 next_shot 真正落地（导播点「确认已切」或直接改 current）时写一行，
// 纯粹的「预告」不写——否则平均停留时长会被预告次数污染。
type ShotCut struct {
	ID        uint   `json:"id" gorm:"primaryKey"`
	ProjectID uint   `json:"project_id" gorm:"index;not null"`
	FromShot  string `json:"from_shot" gorm:"size:100"`
	ToShot    string `json:"to_shot" gorm:"size:100"`
	// DirectorID 是执行这次切台的导播账号，供「谁切的」追溯。
	DirectorID uint `json:"director_id" gorm:"index"`
	// Mode 记录切台时项目处于 live 还是 rehearsal，用于分场统计。
	Mode  string    `json:"mode" gorm:"size:20"`
	CutAt time.Time `json:"cut_at" gorm:"index"`
}

func (ShotCut) TableName() string {
	return "shot_cuts"
}
