package model

import "time"

type EventKind string

const (
	EventUSBAdded       EventKind = "usb_added"
	EventUSBRemoved     EventKind = "usb_removed"
	EventSessionStarted EventKind = "session_started"
	EventResume         EventKind = "resume"
	EventCustom         EventKind = "custom"
	EventTimer          EventKind = "timer"
)

type USBDevice struct {
	VendorID  uint16 `json:"vendor_id"`
	ProductID uint16 `json:"product_id"`
	Serial    string `json:"serial,omitempty"`
	Path      string `json:"path,omitempty"`
}

// Event只报告来源观察到的事实，不代表已触发切换。空USB序列号合法；
// 规则层必须使用明确的筛选条件，不得将这些字段当成设备全局唯一身份。
type Event struct {
	ID         string            `json:"id"`
	Kind       EventKind         `json:"kind"`
	Source     string            `json:"source"`
	ObservedAt time.Time         `json:"observed_at"`
	Name       string            `json:"name,omitempty"`
	USB        *USBDevice        `json:"usb,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// EventCapability以Available判断能力是否可用；不可用时Reason必须非空。
// 可用时Reason可省略，也可补充能力边界说明，不能因有说明文字反推为不可用。
type EventCapability struct {
	Kind         EventKind `json:"kind"`
	Available    bool      `json:"available"`
	Reason       string    `json:"reason,omitempty"`
	FilterFields []string  `json:"filter_fields"`
}

type EventMessage struct {
	Version int   `json:"version"`
	Event   Event `json:"event"`
}

func (e Event) Validate() error {
	if !ValidID(e.ID) || !ValidID(e.Source) || e.ObservedAt.IsZero() {
		return invalid("event", "事件需要有效ID、来源和原始观测时间")
	}
	switch e.Kind {
	case EventUSBAdded, EventUSBRemoved:
		if e.USB == nil {
			return invalid("event.usb", "USB事件必须包含设备字段")
		}
	case EventSessionStarted, EventResume:
		if e.USB != nil {
			return invalid("event.usb", "会话和唤醒事件不能包含USB负载")
		}
	case EventCustom, EventTimer:
		if !ValidID(e.Name) || e.USB != nil {
			return invalid("event.name", "自定义和定时事件需要有效名称且不能包含USB负载")
		}
	default:
		return NewError(CodeUnsupportedProtocol, "不支持的事件种类", nil)
	}
	if len(e.Attributes) > 64 {
		return invalid("event.attributes", "事件扩展属性不能超过64项")
	}
	for key, value := range e.Attributes {
		if !ValidID(key) || len(value) > 1024 {
			return invalid("event.attributes", "属性名称无效或值超过1024字节")
		}
	}
	return nil
}

func (c EventCapability) Validate() error {
	switch c.Kind {
	case EventUSBAdded, EventUSBRemoved, EventSessionStarted, EventResume, EventCustom, EventTimer:
	default:
		return NewError(CodeUnsupportedProtocol, "不支持的事件能力种类", nil)
	}
	if !c.Available && c.Reason == "" {
		return invalid("reason", "不可用事件能力必须说明原因")
	}
	seen := make(map[string]bool, len(c.FilterFields))
	for _, field := range c.FilterFields {
		if !ValidID(field) || seen[field] {
			return invalid("filter_fields", "筛选字段无效或重复")
		}
		seen[field] = true
	}
	return nil
}

func (m EventMessage) Validate() error {
	if m.Version != ProtocolVersion {
		return NewError(CodeUnsupportedVersion, "不支持的事件消息版本", nil)
	}
	return m.Event.Validate()
}
