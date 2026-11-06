// Package scheduler将显式配置的每日墙钟日程转换为规范定时事件，
// 动作始终交给同一规则引擎及后台唯一队列，不直接控制显示器。
package scheduler

import (
	"fmt"
	"io"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

type Schedule struct {
	ID       string `json:"id"`
	Enabled  bool   `json:"enabled"`
	Time     string `json:"time"`
	Timezone string `json:"timezone"`
}

type ScheduleStatus struct {
	Schedule     Schedule   `json:"schedule"`
	MissedPolicy string     `json:"missed_policy"`
	NextTrigger  *time.Time `json:"next_trigger,omitempty"`
}

// LoadSchedules只读取显式传入的独立版本1文档，不搜索默认路径或恢复旧日程。
// 与规则文件沿用同一严格解码及大小上限；缺少列表不能被误当成主动关闭。
func LoadSchedules(reader io.Reader) ([]Schedule, error) {
	if reader == nil {
		return nil, scheduleError("", "日程读取器不能为空", nil)
	}
	data, err := io.ReadAll(io.LimitReader(reader, config.MaxConfigBytes+1))
	if err != nil {
		return nil, model.NewError(model.CodeIO, "读取定时日程失败", err)
	}
	if len(data) > config.MaxConfigBytes {
		return nil, model.NewError(model.CodeMessageTooLarge, "定时日程超过1MiB", nil)
	}
	var document struct {
		Version   int         `json:"version"`
		Schedules *[]Schedule `json:"schedules"`
	}
	if err := model.DecodeJSON(data, &document); err != nil {
		return nil, scheduleError("", "日程JSON字段或结构无效", err)
	}
	if document.Version != 1 {
		return nil, model.NewError(model.CodeUnsupportedVersion, "只接受定时日程版本1", nil)
	}
	if document.Schedules == nil {
		return nil, scheduleError("schedules", "日程列表必须显式提供且不能为null", nil)
	}
	if err := ValidateSchedules(*document.Schedules); err != nil {
		return nil, err
	}
	return append([]Schedule{}, (*document.Schedules)...), nil
}

func ValidateSchedules(schedules []Schedule) error {
	seen := make(map[string]bool, len(schedules))
	for i, schedule := range schedules {
		field := fmt.Sprintf("schedules[%d]", i)
		if !model.ValidID(schedule.ID) || seen[schedule.ID] {
			return scheduleError(field+".id", "日程ID无效或重复", nil)
		}
		seen[schedule.ID] = true
		if len(schedule.Time) != 5 || schedule.Time[2] != ':' ||
			schedule.Time[0] < '0' || schedule.Time[0] > '9' || schedule.Time[1] < '0' || schedule.Time[1] > '9' ||
			schedule.Time[3] < '0' || schedule.Time[3] > '9' || schedule.Time[4] < '0' || schedule.Time[4] > '9' ||
			schedule.Time[:2] > "23" || schedule.Time[3:] > "59" {
			return scheduleError(field+".time", "日程时间必须为24小时制HH:MM", nil)
		}
		// 空名称会被标准库解释为UTC，Local会跟随宿主变化；二者都不符合显式时区契约。
		if schedule.Timezone == "" || schedule.Timezone == "Local" {
			return scheduleError(field+".timezone", "日程必须显式指定IANA时区或UTC", nil)
		}
		if _, err := time.LoadLocation(schedule.Timezone); err != nil {
			return scheduleError(field+".timezone", "日程时区不可加载", err)
		}
	}
	return nil
}

func scheduleError(field, message string, cause error) *model.Error {
	err := model.NewError(model.CodeInvalidConfig, message, cause)
	err.Field = field
	return err
}
