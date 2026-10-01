package models

import "time"

// 项目状态取值。用字符串而不是枚举类型，是为了让 DB 里的值与 JSON 一致，
// 排查问题时一眼能看懂；合法性校验集中在 ProjectStatuses 里。
const (
	ProjectStatusPlanned   = "planned"
	ProjectStatusLive      = "live"
	ProjectStatusFinished  = "finished"
	ProjectStatusCancelled = "cancelled"
)

// 项目模式：正式直播或彩排。shot_cuts 会把它一起记下来，用于分场统计。
const (
	ProjectModeLive      = "live"
	ProjectModeRehearsal = "rehearsal"
)

// ProjectStatuses 按业务推进顺序排列，供前端下拉与校验使用。
var ProjectStatuses = []string{
	ProjectStatusPlanned,
	ProjectStatusLive,
	ProjectStatusFinished,
	ProjectStatusCancelled,
}

var ProjectModes = []string{ProjectModeLive, ProjectModeRehearsal}

// ValidProjectStatus 报告状态是否合法。空串视为合法（表示「未设置」），
// 由调用方决定是否回落到 planned。
func ValidProjectStatus(s string) bool {
	if s == "" {
		return true
	}
	for _, v := range ProjectStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// ValidProjectMode 报告模式是否合法。空串同理。
func ValidProjectMode(m string) bool {
	if m == "" {
		return true
	}
	for _, v := range ProjectModes {
		if v == m {
			return true
		}
	}
	return false
}

type Project struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	Name        string `json:"name" gorm:"size:100;not null"`
	Code        string `json:"code" gorm:"uniqueIndex;size:50;not null"`
	Description string `json:"description" gorm:"size:500"`

	// ScheduledStart/End 是计划时间窗。用指针是因为「没安排日程」与
	// 「安排在零值时刻」必须区分开：导播端要按它排序挑当前/下一场，
	// 把未排期的项目当成 0001 年会把列表顺序彻底搞乱。
	ScheduledStart *time.Time `json:"scheduled_start" gorm:"index"`
	ScheduledEnd   *time.Time `json:"scheduled_end"`
	Venue          string     `json:"venue" gorm:"size:200"`
	// OwnerID 是项目负责人（users.id）。0 表示未指定。
	OwnerID uint `json:"owner_id" gorm:"index"`
	// Status 见 ProjectStatus* 常量；Mode 见 ProjectMode* 常量。
	// 两者都给了 DB 默认值，历史行在迁移里也会被补齐。
	Status string `json:"status" gorm:"size:20;default:planned"`
	Mode   string `json:"mode" gorm:"size:20;default:live"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (Project) TableName() string {
	return "projects"
}
