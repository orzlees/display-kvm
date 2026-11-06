package model

import (
	"encoding/hex"
	"time"
)

type DisplayID string
type TargetID string
type GroupID string
type SchemeID string
type RequestID string

// IdentityHints只提供匹配候选，不是唯一键。真实设备可能重复或缺少序列号，
// 路径也可能随接口变化；存在多个候选时必须交由人工绑定，不能自动合并。
type IdentityHints struct {
	Manufacturer string `json:"manufacturer,omitempty"`
	ProductCode  string `json:"product_code,omitempty"`
	Serial       string `json:"serial,omitempty"`
	EDIDSHA256   string `json:"edid_sha256,omitempty"`
	Path         string `json:"path,omitempty"`
}

// Binding记录用户确认的路径约束；路径失效时必须重新绑定，不能回退到首个候选。
type Binding struct {
	Path string `json:"path"`
}

type Display struct {
	ID       DisplayID     `json:"id"`
	Name     string        `json:"name"`
	Identity IdentityHints `json:"identity"`
	Binding  *Binding      `json:"binding,omitempty"`
}

// Device中的BackendID只在相应后端及当前枚举周期内有效，不可当作DisplayID持久化。
// 后端负责验证设备仍指向原枚举对象，失效后应返回not_found/unreachable。
type Device struct {
	BackendID string        `json:"backend_id"`
	Name      string        `json:"name"`
	Identity  IdentityHints `json:"identity"`
}

type InputProtocol string

const (
	ProtocolVCP        InputProtocol = "vcp"
	ProtocolLGInputAlt InputProtocol = "lg_input_alt"
)

// InputScheme保存经过选择或校准的真实协议参数。厂商方案不隐式转成标准VCP，
// 也不给所有同品牌设备套用默认值；不支持该协议的后端必须明确拒绝。
type InputScheme struct {
	ID       SchemeID      `json:"id"`
	Protocol InputProtocol `json:"protocol"`
	VCPCode  uint8         `json:"vcp_code,omitempty"`
	Value    uint16        `json:"value"`
}

type TargetInput struct {
	DisplayID DisplayID `json:"display_id"`
	SchemeID  SchemeID  `json:"scheme_id"`
}

type Target struct {
	ID     TargetID      `json:"id"`
	Name   string        `json:"name"`
	Inputs []TargetInput `json:"inputs"`
}

type Group struct {
	ID         GroupID     `json:"id"`
	Name       string      `json:"name"`
	DisplayIDs []DisplayID `json:"display_ids"`
}

type DeviceCapabilities struct {
	Protocols         []InputProtocol `json:"protocols"`
	CanReadInput      bool            `json:"can_read_input"`
	UnavailableReason string          `json:"unavailable_reason,omitempty"`
}

// nil值表示没有有效输入观测；ObservedAt是观测时间，不是请求或缓存读取时间。
type InputObservation struct {
	Value      *uint16   `json:"value,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// WriteReceipt只证明传输已提交，不证明读回值或目标主机画面。后端不能在这里
// 声称画面成功；切走导致通道失联时，上一条有效观测仍应按原时间保留。
type WriteReceipt struct {
	SentAt time.Time `json:"sent_at"`
}

type ExecutionState string

const (
	StateAccepted  ExecutionState = "accepted"
	StateSent      ExecutionState = "sent"
	StateConfirmed ExecutionState = "confirmed"
	StateFailed    ExecutionState = "failed"
	StateUnknown   ExecutionState = "unknown"
)

type DisplayResult struct {
	DisplayID    DisplayID         `json:"display_id"`
	State        ExecutionState    `json:"state"`
	DesiredValue uint16            `json:"desired_value"`
	SentAt       *time.Time        `json:"sent_at,omitempty"`
	Observation  *InputObservation `json:"observation,omitempty"`
	Error        *Error            `json:"error,omitempty"`
}

type SwitchResult struct {
	RequestID RequestID       `json:"request_id"`
	Displays  []DisplayResult `json:"displays"`
	Error     *Error          `json:"error,omitempty"`
}

// ValidID为逻辑身份和关联标识提供统一、跨语言可实现的规则，禁止依赖数组下标。
func ValidID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}

func (d Display) Validate() error {
	if !ValidID(string(d.ID)) {
		return invalid("id", "显示器逻辑ID无效")
	}
	if d.Identity.EDIDSHA256 != "" {
		decoded, err := hex.DecodeString(d.Identity.EDIDSHA256)
		if err != nil || len(decoded) != 32 {
			return invalid("identity.edid_sha256", "EDID摘要必须为64位十六进制SHA256")
		}
	}
	if d.Binding != nil && d.Binding.Path == "" {
		return invalid("binding.path", "人工绑定路径不能为空")
	}
	return nil
}

func (s InputScheme) Validate() error {
	if !ValidID(string(s.ID)) {
		return invalid("id", "输入方案ID无效")
	}
	switch s.Protocol {
	case ProtocolVCP:
		if s.VCPCode != 0x60 {
			return invalid("vcp_code", "标准输入切换只允许VCP 0x60")
		}
	case ProtocolLGInputAlt:
		if s.VCPCode != 0 {
			return invalid("vcp_code", "LG alternate不接受标准VCP代码")
		}
	default:
		return NewError(CodeUnsupportedProtocol, "不支持的输入控制协议", nil)
	}
	if s.Value == 0 {
		return invalid("value", "输入值必须为1至65535，且须由设备能力或校准确认")
	}
	return nil
}

func (r SwitchResult) Validate() error {
	if !ValidID(string(r.RequestID)) {
		return invalid("request_id", "结果缺少有效请求关联ID")
	}
	if len(r.Displays) == 0 && r.Error == nil {
		return invalid("displays", "空结果必须说明请求失败原因")
	}
	if r.Error != nil {
		if err := r.Error.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[DisplayID]bool, len(r.Displays))
	for _, display := range r.Displays {
		if !ValidID(string(display.DisplayID)) || seen[display.DisplayID] {
			return invalid("displays.display_id", "逐屏结果ID无效或重复")
		}
		seen[display.DisplayID] = true
		if display.Error != nil {
			if err := display.Error.Validate(); err != nil {
				return err
			}
		}
		switch display.State {
		case StateAccepted:
			if display.SentAt != nil || display.Error != nil {
				return invalid("displays.state", "已接受状态不能附带发送时间或失败")
			}
		case StateSent, StateConfirmed:
			if display.SentAt == nil || display.SentAt.IsZero() || display.Error != nil || display.DesiredValue == 0 {
				return invalid("displays.state", "发送或确认状态需要发送证据、有效目标值且不能附带失败")
			}
			// 缓存中的旧读回值即使碰巧等于目标，也不能作为本次操作的确认。
			// 例如10:00发送，09:59读回17，不能据此把10:00的请求标为确认。
			if display.State == StateConfirmed && (display.Observation == nil || display.Observation.Value == nil || display.Observation.ObservedAt.IsZero() || display.Observation.ObservedAt.Before(*display.SentAt) || *display.Observation.Value != display.DesiredValue) {
				return invalid("displays.observation", "确认需要发送后取得的相同输入读回证据")
			}
		case StateFailed, StateUnknown:
			if display.Error == nil {
				return invalid("displays.error", "失败或未知状态必须保留原因")
			}
		default:
			return invalid("displays.state", "未知执行状态")
		}
	}
	return nil
}
