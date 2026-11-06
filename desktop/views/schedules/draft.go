package schedules

import (
	"bytes"
	"encoding/json"
	"net/url"
	"reflect"
	"strconv"

	"github.com/orzlee/displayKvm/internal/scheduler"
)

type draft struct {
	base     []scheduler.Schedule
	items    []scheduler.Schedule
	selected int
}

func newDraft(items []scheduler.Schedule) draft {
	d := draft{base: append([]scheduler.Schedule{}, items...), items: append([]scheduler.Schedule{}, items...), selected: -1}
	if len(items) > 0 {
		d.selected = 0
	}
	return d
}

// 原生表单把当前编辑项与选择/新增动作一起提交，避免切换列表时丢掉尚未保存的
// 输入。此处保留非法原值；保存时统一交核心校验，不能悄悄替换成默认时间或时区。
func (d *draft) capture(values url.Values) error {
	if values.Get("editing") != "1" {
		return nil
	}
	index, err := strconv.Atoi(values.Get("index"))
	if err != nil || index != d.selected || index < 0 || index >= len(d.items) {
		return messageError("schedules.error.editor_changed")
	}
	if value := values.Get("enabled"); value != "" && value != "1" {
		return messageError("schedules.error.enabled")
	}
	d.items[index] = scheduler.Schedule{ID: values.Get("id"), Time: values.Get("time"), Timezone: values.Get("timezone"), Enabled: values.Get("enabled") == "1"}
	return nil
}

func (d *draft) document() ([]byte, error) {
	data, err := json.Marshal(struct {
		Version   int                  `json:"version"`
		Schedules []scheduler.Schedule `json:"schedules"`
	}{1, d.items})
	if err != nil {
		return nil, err
	}
	// 与后台使用同一文档入口，连同禁用项、重复ID、大小及显式时区一起严格校验；
	// 不能因浏览器time控件或未勾选启用就绕过核心边界。
	if _, err := scheduler.LoadSchedules(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return data, nil
}

func (d *draft) dirty() bool { return !reflect.DeepEqual(d.base, d.items) }
