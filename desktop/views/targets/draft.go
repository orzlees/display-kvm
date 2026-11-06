// Package targets呈现屏组与目标主机编辑页；配置提交只交给共享管理服务，
// 页面不访问配置文件；已保存目标的输入测试由后台管理会话执行，页面不直接写硬件。
package targets

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

type selection struct {
	Kind string
	ID   string
}

type draft struct {
	Config   config.Config
	Selected selection
	Dirty    bool
}

// cloneConfig复制完整配置而非只拼接本页可编辑字段。配置还承载设备身份、绑定、
// 输入方案以及其他页面的数据；只复制分组和目标会在保存时丢失未编辑内容。
// 使用核心结构本身的JSON字段，后续版本增加字段时也无需维护另一份字段清单。
func cloneConfig(cfg config.Config) (config.Config, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return config.Config{}, model.NewError(model.CodeInvalidConfig, "targets.error.clone_encode", err)
	}
	var copy config.Config
	if err := json.Unmarshal(data, &copy); err != nil {
		return config.Config{}, model.NewError(model.CodeInvalidConfig, "targets.error.clone_decode", err)
	}
	return copy, nil
}

func newDraft(cfg config.Config) (*draft, error) {
	copy, err := cloneConfig(cfg)
	if err != nil {
		return nil, err
	}
	d := &draft{Config: copy}
	if len(copy.Groups) > 0 {
		d.Selected = selection{Kind: "group", ID: string(copy.Groups[0].ID)}
	} else if len(copy.Targets) > 0 {
		d.Selected = selection{Kind: "target", ID: string(copy.Targets[0].ID)}
	}
	return d, nil
}

func (d *draft) selectItem(selected selection) bool {
	if selected.Kind == "group" {
		for _, group := range d.Config.Groups {
			if string(group.ID) == selected.ID {
				d.Selected = selected
				return true
			}
		}
	}
	if selected.Kind == "target" {
		for _, target := range d.Config.Targets {
			if string(target.ID) == selected.ID {
				d.Selected = selected
				return true
			}
		}
	}
	return false
}

func (d *draft) add(kind string, catalog i18n.Catalog) error {
	if kind != "group" && kind != "target" {
		return model.NewError(model.CodeInvalidConfig, "targets.error.choose", nil)
	}
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return model.NewError(model.CodeIO, "targets.error.new_id", err)
	}
	id := kind + "-" + hex.EncodeToString(token[:])
	if kind == "group" {
		d.Config.Groups = append(d.Config.Groups, model.Group{ID: model.GroupID(id), Name: catalog.Text("targets.new_group", nil)})
	} else {
		// 空映射只存在于未提交草稿中；必须由用户逐屏选择已有方案，
		// 保存时由核心拒绝仍为空的目标，不能根据品牌或次序自动填端口。
		d.Config.Targets = append(d.Config.Targets, model.Target{ID: model.TargetID(id), Name: catalog.Text("targets.new_target", nil)})
	}
	d.Selected = selection{Kind: kind, ID: id}
	d.Dirty = true
	return nil
}

func (d *draft) update(form url.Values) error {
	selected := selection{Kind: form.Get("kind"), ID: form.Get("id")}
	if selected != d.Selected {
		return model.NewError(model.CodeInvalidConfig, "targets.error.editor_changed", nil)
	}
	if selected.Kind == "group" {
		for i := range d.Config.Groups {
			group := &d.Config.Groups[i]
			if string(group.ID) != selected.ID {
				continue
			}
			group.Name = form.Get("name")
			var members []model.DisplayID
			counts := make(map[model.DisplayID]int, len(group.DisplayIDs))
			for _, id := range group.DisplayIDs {
				counts[id]++
			}
			for _, id := range form["display_id"] {
				member := model.DisplayID(id)
				members = append(members, member)
				counts[member]--
			}
			// 复选框按显示器列表提交，名称修改不能顺带重排原成员。实际成员变化时
			// 保留提交内容供核心检查，重复或未知ID不能被本页静默去重掩盖。
			sameMembers := len(members) == len(group.DisplayIDs)
			for _, count := range counts {
				sameMembers = sameMembers && count == 0
			}
			if !sameMembers {
				group.DisplayIDs = members
			}
			d.Dirty = true
			return nil
		}
	}
	if selected.Kind == "target" {
		for i := range d.Config.Targets {
			target := &d.Config.Targets[i]
			if string(target.ID) != selected.ID {
				continue
			}
			target.Name = form.Get("name")
			var inputs []model.TargetInput
			seen := make(map[model.DisplayID]bool, len(target.Inputs))
			// 页面不提供排序功能，更新输入值时保留已有映射次序；只把新加入的屏幕
			// 追加到末尾，避免保存名称等操作顺带重排未编辑的映射。
			for _, input := range target.Inputs {
				seen[input.DisplayID] = true
				if id := form.Get("mapping." + string(input.DisplayID)); id != "" {
					inputs = append(inputs, model.TargetInput{DisplayID: input.DisplayID, SchemeID: model.SchemeID(id)})
				}
			}
			for _, display := range d.Config.Displays {
				if id := form.Get("mapping." + string(display.ID)); id != "" && !seen[display.ID] {
					inputs = append(inputs, model.TargetInput{DisplayID: display.ID, SchemeID: model.SchemeID(id)})
				}
			}
			target.Inputs = inputs
			d.Dirty = true
			return nil
		}
	}
	return model.NewError(model.CodeNotFound, "targets.error.editor_missing", nil)
}

func (d *draft) removeSelected() error {
	selected := d.Selected
	if selected.Kind == "group" {
		for i, group := range d.Config.Groups {
			if string(group.ID) == selected.ID {
				d.Config.Groups = append(d.Config.Groups[:i], d.Config.Groups[i+1:]...)
				d.Selected = selection{}
				d.Dirty = true
				return nil
			}
		}
	}
	if selected.Kind == "target" {
		for i, target := range d.Config.Targets {
			if string(target.ID) == selected.ID {
				d.Config.Targets = append(d.Config.Targets[:i], d.Config.Targets[i+1:]...)
				d.Selected = selection{}
				d.Dirty = true
				return nil
			}
		}
	}
	return model.NewError(model.CodeNotFound, "targets.error.choose_remove", nil)
}
