package automation

import (
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

// 配置、规则及查询结果都可能由调用者保留。逐层复制切片、映射和指针，避免外部
// 编辑改变已注册动作，或查询者通过返回值改写冷却、事件证据和逐屏结果。
func cloneConfig(cfg config.Config) config.Config {
	cfg.Displays = append([]model.Display(nil), cfg.Displays...)
	for i := range cfg.Displays {
		if cfg.Displays[i].Binding != nil {
			binding := *cfg.Displays[i].Binding
			cfg.Displays[i].Binding = &binding
		}
	}
	cfg.InputSchemes = append([]model.InputScheme(nil), cfg.InputSchemes...)
	cfg.Targets = append([]model.Target(nil), cfg.Targets...)
	for i := range cfg.Targets {
		cfg.Targets[i].Inputs = append([]model.TargetInput(nil), cfg.Targets[i].Inputs...)
	}
	cfg.Groups = append([]model.Group(nil), cfg.Groups...)
	for i := range cfg.Groups {
		cfg.Groups[i].DisplayIDs = append([]model.DisplayID(nil), cfg.Groups[i].DisplayIDs...)
	}
	return cfg
}

func cloneRules(rules []Rule) []Rule {
	rules = append([]Rule(nil), rules...)
	for i := range rules {
		when := &rules[i].When
		when.Attributes = cloneAttributes(when.Attributes)
		if when.USBPresent != nil {
			present := *when.USBPresent
			when.USBPresent = &present
		}
		if when.USB != nil {
			usb := *when.USB
			if usb.VendorID != nil {
				value := *usb.VendorID
				usb.VendorID = &value
			}
			if usb.ProductID != nil {
				value := *usb.ProductID
				usb.ProductID = &value
			}
			when.USB = &usb
		}
		rules[i].Action = cloneRequest(rules[i].Action)
	}
	return rules
}

func cloneAttributes(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneEvent(event *model.Event) *model.Event {
	if event == nil {
		return nil
	}
	copy := *event
	copy.Attributes = cloneAttributes(event.Attributes)
	if event.USB != nil {
		usb := *event.USB
		copy.USB = &usb
	}
	return &copy
}

func cloneRequest(request model.SwitchRequest) model.SwitchRequest {
	request.DisplayIDs = append([]model.DisplayID(nil), request.DisplayIDs...)
	return request
}

func cloneError(err *model.Error) *model.Error {
	if err == nil {
		return nil
	}
	copy := *err
	return &copy
}

func cloneResult(result *model.SwitchResult) *model.SwitchResult {
	if result == nil {
		return nil
	}
	copy := *result
	copy.Error = cloneError(result.Error)
	copy.Displays = append([]model.DisplayResult(nil), result.Displays...)
	for i := range copy.Displays {
		display := &copy.Displays[i]
		display.Error = cloneError(display.Error)
		if display.SentAt != nil {
			at := *display.SentAt
			display.SentAt = &at
		}
		if display.Observation != nil {
			observation := *display.Observation
			if observation.Value != nil {
				value := *observation.Value
				observation.Value = &value
			}
			display.Observation = &observation
		}
	}
	return &copy
}

func cloneDecision(decision Decision) Decision {
	decision.Request = cloneRequest(decision.Request)
	decision.DisplayIDs = append([]model.DisplayID(nil), decision.DisplayIDs...)
	return decision
}

func cloneRecord(record Record) Record {
	record.Event = cloneEvent(record.Event)
	record.Decision = cloneDecision(record.Decision)
	record.Result = cloneResult(record.Result)
	record.Error = cloneError(record.Error)
	return record
}
