// Package automation根据明确配置的规则处理规范事件；它不持有后端，也不保存配置。
package automation

import (
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

// Rule的动作沿用唯一切换请求契约。文件中的Action.ID必须为空：一次规则可能多次
// 触发，每次真实入队都须生成新关联ID，不能把持久化规则ID当成请求幂等键。
type Rule struct {
	ID             string              `json:"id"`
	Enabled        bool                `json:"enabled"`
	Priority       int                 `json:"priority,omitempty"`
	DebounceMillis int64               `json:"debounce_ms,omitempty"`
	CooldownMillis int64               `json:"cooldown_ms,omitempty"`
	When           Condition           `json:"when"`
	Action         model.SwitchRequest `json:"action"`
}

// Condition只接受两种互斥模式：Kind选择真实事件；USBPresent选择当前存在状态。
// 启动基线没有事件来源或插入/登录语义，因此状态条件不接受Source、Name、Attributes。
type Condition struct {
	Kind       model.EventKind   `json:"kind,omitempty"`
	Source     string            `json:"source,omitempty"`
	Name       string            `json:"name,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	USB        *USBMatch         `json:"usb,omitempty"`
	USBPresent *bool             `json:"usb_present,omitempty"`
}

// USBMatch只做字段筛选，不推导全局设备身份。nil数字字段及空字符串表示不限制，
// 数字指针允许明确匹配VID/PID=0；空或重复序列号不得用来合并设备。
type USBMatch struct {
	VendorID  *uint16 `json:"vendor_id,omitempty"`
	ProductID *uint16 `json:"product_id,omitempty"`
	Serial    string  `json:"serial,omitempty"`
	Path      string  `json:"path,omitempty"`
}

func (m *USBMatch) matches(device model.USBDevice) bool {
	return m != nil && (m.VendorID == nil || *m.VendorID == device.VendorID) &&
		(m.ProductID == nil || *m.ProductID == device.ProductID) &&
		(m.Serial == "" || m.Serial == device.Serial) && (m.Path == "" || m.Path == device.Path)
}

// LoadRules仅解码调用者提供的流，不选择路径、不创建文件、不将读取失败变成空规则。
// 规则文档版本独立于显示器配置版本；只有显式rules:[]才表示已配置但关闭。
func LoadRules(reader io.Reader) ([]Rule, error) {
	if reader == nil {
		return nil, ruleError("", "规则读取器不能为空", nil)
	}
	data, err := io.ReadAll(io.LimitReader(reader, config.MaxConfigBytes+1))
	if err != nil {
		return nil, model.NewError(model.CodeIO, "读取自动化规则失败", err)
	}
	if len(data) > config.MaxConfigBytes {
		return nil, model.NewError(model.CodeMessageTooLarge, "自动化规则超过1MiB", nil)
	}
	var document struct {
		Version int     `json:"version"`
		Rules   *[]Rule `json:"rules"`
	}
	if err := model.DecodeJSON(data, &document); err != nil {
		return nil, ruleError("", "规则JSON字段或结构无效", err)
	}
	if document.Version != 1 {
		return nil, model.NewError(model.CodeUnsupportedVersion, "只接受自动化规则版本1", nil)
	}
	if document.Rules == nil {
		return nil, ruleError("rules", "规则列表必须显式提供且不能为null", nil)
	}
	if err := validateRuleList(*document.Rules); err != nil {
		return nil, err
	}
	return cloneRules(*document.Rules), nil
}

// ValidateRules复用显示器配置和请求校验，并在同一配置快照上解析动作范围。
// 语法合法的目标/分组仍可能包含未映射屏，必须在注册前拒绝，不能留到部分发送后。
func ValidateRules(cfg config.Config, rules []Rule) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := validateRuleList(rules); err != nil {
		return err
	}
	for i, rule := range rules {
		if _, err := resolveScope(cfg, rule.Action); err != nil {
			return ruleError(fmt.Sprintf("rules[%d].action", i), "规则动作引用或显示器选择无效", err)
		}
	}
	return nil
}

func validateRuleList(rules []Rule) error {
	seen := make(map[string]bool, len(rules))
	for i, rule := range rules {
		field := fmt.Sprintf("rules[%d]", i)
		if !model.ValidID(rule.ID) || seen[rule.ID] {
			return ruleError(field+".id", "规则ID无效或重复", nil)
		}
		seen[rule.ID] = true
		if rule.DebounceMillis < 0 || rule.CooldownMillis < 0 ||
			rule.DebounceMillis > math.MaxInt64/int64(time.Millisecond) || rule.CooldownMillis > math.MaxInt64/int64(time.Millisecond) {
			return ruleError(field, "防抖和冷却毫秒必须非负且可表示为本地时长", nil)
		}
		if rule.Action.ID != "" {
			return ruleError(field+".action.id", "规则文件不能预置请求关联ID", nil)
		}
		request := rule.Action
		request.ID = "automation:validation"
		if err := request.Validate(); err != nil {
			return ruleError(field+".action", "规则动作无效", err)
		}
		when := rule.When
		usbSpecified := when.USB != nil && (when.USB.VendorID != nil || when.USB.ProductID != nil || when.USB.Serial != "" || when.USB.Path != "")
		if when.USBPresent != nil {
			if when.Kind != "" || when.Source != "" || when.Name != "" || len(when.Attributes) != 0 || !usbSpecified {
				return ruleError(field+".when", "USB状态条件只能含usb_present及明确USB筛选", nil)
			}
			continue
		}
		if when.Source != "" && !model.ValidID(when.Source) {
			return ruleError(field+".when.source", "事件来源筛选无效", nil)
		}
		event := model.Event{ID: "validation", Source: "automation", Kind: when.Kind, Name: when.Name, Attributes: when.Attributes, ObservedAt: time.Unix(1, 0)}
		if when.Kind == model.EventUSBAdded || when.Kind == model.EventUSBRemoved {
			if !usbSpecified {
				return ruleError(field+".when.usb", "USB事件必须提供明确设备筛选", nil)
			}
			event.USB = &model.USBDevice{}
		} else if when.USB != nil {
			return ruleError(field+".when.usb", "非USB事件不能包含USB筛选", nil)
		}
		if when.Kind != model.EventCustom && when.Kind != model.EventTimer && when.Name != "" {
			return ruleError(field+".when.name", "只有自定义和定时事件可按名称筛选", nil)
		}
		if err := event.Validate(); err != nil {
			return ruleError(field+".when", "事件条件无效", err)
		}
	}
	return nil
}

func resolveScope(cfg config.Config, action model.SwitchRequest) ([]model.DisplayID, error) {
	var target *model.Target
	for i := range cfg.Targets {
		if cfg.Targets[i].ID == action.TargetID {
			target = &cfg.Targets[i]
			break
		}
	}
	if target == nil {
		return nil, ruleError("target_id", "目标不存在", nil)
	}
	scope := append([]model.DisplayID(nil), action.DisplayIDs...)
	if action.GroupID != "" {
		found := false
		for _, group := range cfg.Groups {
			if group.ID == action.GroupID {
				scope, found = append([]model.DisplayID(nil), group.DisplayIDs...), true
				break
			}
		}
		if !found {
			return nil, ruleError("group_id", "分组不存在", nil)
		}
	}
	if len(scope) == 0 {
		for _, input := range target.Inputs {
			scope = append(scope, input.DisplayID)
		}
	}
	mapped := make(map[model.DisplayID]bool, len(target.Inputs))
	for _, input := range target.Inputs {
		mapped[input.DisplayID] = true
	}
	for _, id := range scope {
		if !mapped[id] {
			return nil, ruleError("display_ids", "目标未映射所选显示器", nil)
		}
	}
	sort.Slice(scope, func(i, j int) bool { return scope[i] < scope[j] })
	return scope, nil
}

func ruleError(field, message string, cause error) *model.Error {
	err := model.NewError(model.CodeInvalidConfig, message, cause)
	err.Field = field
	return err
}
