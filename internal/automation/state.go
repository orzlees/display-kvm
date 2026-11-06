package automation

import (
	"time"

	"github.com/orzlee/displayKvm/internal/daemon"
	"github.com/orzlee/displayKvm/internal/model"
)

type usbMark struct {
	at     time.Time
	kind   model.EventKind
	device model.USBDevice
}

type usbState struct {
	known      bool
	baselineAt time.Time
	devices    []model.USBDevice
	marks      map[string]usbMark
}

func (s usbState) copy() usbState {
	s.devices = append([]model.USBDevice(nil), s.devices...)
	marks := make(map[string]usbMark, len(s.marks))
	for key, value := range s.marks {
		marks[key] = value
	}
	s.marks = marks
	return s
}

func (s *usbState) baseline(value daemon.Baseline) error {
	if value.ObservedAt.IsZero() || value.ObservedAt.Before(s.baselineAt) || (!value.USBKnown && len(value.USBDevices) != 0) {
		return model.NewError(model.CodeBackendUnavailable, "补扫缺少时间、倒退或混淆未知USB状态", nil)
	}
	for _, mark := range s.marks {
		if value.ObservedAt.Before(mark.at) {
			return model.NewError(model.CodeBackendUnavailable, "补扫时间早于已知USB观测，保持未知", nil)
		}
		if value.USBKnown && value.ObservedAt.Equal(mark.at) {
			// 部分来源时间精度不足以区分同刻的事件和快照；同刻矛盾不能用
			// 返回先后决定真伪，否则旧移除可能覆盖已经观察到的新插入。
			present := false
			for _, device := range value.USBDevices {
				if device.Path == mark.device.Path {
					if device != mark.device {
						return model.NewError(model.CodeIdentityAmbiguous, "同刻补扫的USB身份与事件矛盾", nil)
					}
					present = true
				}
			}
			if present != (mark.kind == model.EventUSBAdded) {
				return model.NewError(model.CodeBackendUnavailable, "同刻补扫的USB存在状态与事件矛盾", nil)
			}
		}
	}
	// 补扫进行期间仍消费长期来源的事件；它们在新基线发布后按观测时间处理。
	// 同一不透明路径出现两次时无法区分重复来源与不同设备，不能静默合并身份。
	paths := make(map[string]bool, len(value.USBDevices))
	for _, device := range value.USBDevices {
		if device.Path != "" {
			if paths[device.Path] {
				return model.NewError(model.CodeIdentityAmbiguous, "补扫包含重复USB路径，保持未知", nil)
			}
			paths[device.Path] = true
		}
	}
	s.known, s.baselineAt = value.USBKnown, value.ObservedAt
	s.devices = append([]model.USBDevice(nil), value.USBDevices...)
	s.marks = make(map[string]usbMark)
	return nil
}

// apply只用外部观测时间防止状态倒退，绝不使用它计算本地防抖/冷却。
// 返回rescan_required时不猜测缺席；无路径、同刻矛盾和身份变化都需要新轮次补扫。
func (s *usbState) apply(event model.Event) (reason string, changed bool) {
	if event.ObservedAt.Before(s.baselineAt) {
		return "stale", false
	}
	if !s.known || event.USB.Path == "" {
		return "rescan_required", false
	}
	device := *event.USB
	previous, marked := s.marks[device.Path]
	if marked {
		if event.ObservedAt.Before(previous.at) {
			return "stale", false
		}
		if event.ObservedAt.Equal(previous.at) {
			if event.Kind == previous.kind && device == previous.device {
				return "duplicate_state", false
			}
			return "rescan_required", false
		}
	}
	index := -1
	for i, known := range s.devices {
		if known.Path == device.Path {
			index = i
			break
		}
	}
	if event.ObservedAt.Equal(s.baselineAt) {
		if event.Kind == model.EventUSBAdded && index >= 0 && s.devices[index] == device || event.Kind == model.EventUSBRemoved && index < 0 {
			return "stale", false
		}
		return "rescan_required", false
	}
	// 保留移除后的时间标记，防止迟到的旧插入复活设备；设备路径风暴超过有界窗口
	// 时重新取得基线，而不是无限保留墓碑或丢弃时间标记后接受旧事件。
	if !marked && len(s.marks) >= replayLimit {
		return "rescan_required", false
	}
	if s.marks == nil {
		s.marks = make(map[string]usbMark)
	}
	if index >= 0 && s.devices[index] != device {
		return "rescan_required", false
	}
	s.marks[device.Path] = usbMark{at: event.ObservedAt, kind: event.Kind, device: device}
	if event.Kind == model.EventUSBAdded {
		if index >= 0 {
			return "duplicate_state", false
		}
		s.devices = append(s.devices, device)
	} else {
		if index < 0 {
			return "duplicate_state", false
		}
		s.devices = append(s.devices[:index], s.devices[index+1:]...)
	}
	return "observed", true
}

func (s usbState) condition(when Condition) bool {
	if !s.known {
		return false
	}
	present := false
	for _, device := range s.devices {
		if when.USB.matches(device) {
			present = true
			break
		}
	}
	return present == *when.USBPresent
}

func eventMatches(when Condition, event *model.Event) bool {
	if event == nil || when.Kind != event.Kind || when.Source != "" && when.Source != event.Source || when.Name != "" && when.Name != event.Name {
		return false
	}
	if when.USB != nil && (event.USB == nil || !when.USB.matches(*event.USB)) {
		return false
	}
	for key, value := range when.Attributes {
		actual, exists := event.Attributes[key]
		if !exists || actual != value {
			return false
		}
	}
	return true
}

func isUSB(event *model.Event) bool {
	return event != nil && (event.Kind == model.EventUSBAdded || event.Kind == model.EventUSBRemoved)
}

// USB事件的候选绑定当时的具体事实。例如插入后在防抖期内移除，原插入候选就
// 已失效；即使移除事件没有匹配该规则，也不能继续把旧插入动作送入队列。
func candidateValid(state usbState, rule Rule, event *model.Event) bool {
	if rule.When.USBPresent != nil {
		return state.condition(rule.When)
	}
	if !isUSB(event) {
		return true
	}
	if !state.known || event.USB.Path == "" {
		return false
	}
	present := false
	for _, device := range state.devices {
		if device.Path == event.USB.Path {
			present = device == *event.USB
			if !present {
				return false
			}
			break
		}
	}
	return present == (event.Kind == model.EventUSBAdded)
}
