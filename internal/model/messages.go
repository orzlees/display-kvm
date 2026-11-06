package model

import (
	"encoding/base64"
	"net"
	"path/filepath"
	"strconv"
	"time"
)

const (
	ProtocolVersion       = 1
	EndpointVersion       = 1
	MaxMessageBytes       = 1 << 20
	ConnectTimeout        = 2 * time.Second
	DefaultRequestTimeout = 10 * time.Second
	MaxRequestTimeout     = 30 * time.Second
	ResponseTimeout       = 35 * time.Second
)

type SwitchRequest struct {
	ID            RequestID   `json:"id"`
	TargetID      TargetID    `json:"target_id"`
	DisplayIDs    []DisplayID `json:"display_ids,omitempty"`
	GroupID       GroupID     `json:"group_id,omitempty"`
	TimeoutMillis int64       `json:"timeout_ms,omitempty"`
}

func (r SwitchRequest) Validate() error {
	if !ValidID(string(r.ID)) || !ValidID(string(r.TargetID)) {
		return invalid("id", "请求ID和目标ID必须有效")
	}
	if r.GroupID != "" && (!ValidID(string(r.GroupID)) || len(r.DisplayIDs) > 0) {
		return invalid("group_id", "分组与逐屏选择不能同时指定，分组ID必须有效")
	}
	seen := make(map[DisplayID]bool, len(r.DisplayIDs))
	for _, id := range r.DisplayIDs {
		if !ValidID(string(id)) || seen[id] {
			return invalid("display_ids", "显示器选择含非法或重复ID")
		}
		seen[id] = true
	}
	if r.TimeoutMillis < 0 || r.TimeoutMillis > MaxRequestTimeout.Milliseconds() {
		return invalid("timeout_ms", "操作超时必须为0或1至30000毫秒")
	}
	return nil
}

// 调用者须先Validate。零值表示统一默认值，不能解释为无限等待。
func (r SwitchRequest) EffectiveTimeout() time.Duration {
	if r.TimeoutMillis == 0 {
		return DefaultRequestTimeout
	}
	return time.Duration(r.TimeoutMillis) * time.Millisecond
}

// Endpoint仅描述私有发现文件；Validate不验证文件所有者或ACL，也不创建监听器。
// 运行实现必须先保证文件和父目录私有，再使用其中的地址和认证材料。
type Endpoint struct {
	Version         int       `json:"version"`
	ProtocolVersion int       `json:"protocol_version"`
	Network         string    `json:"network"`
	Address         string    `json:"address"`
	Token           string    `json:"token"`
	PID             int       `json:"pid"`
	StartedAt       time.Time `json:"started_at"`
}

// configDirectory由os.UserConfigDir取得；空值/失败须由调用方报错，不可回退公共目录。
func EndpointPath(configDirectory string) string {
	return filepath.Join(configDirectory, "DisplayKVM", "runtime", "endpoint.json")
}

func (e Endpoint) Validate() error {
	if e.Version != EndpointVersion || e.ProtocolVersion != ProtocolVersion {
		return NewError(CodeUnsupportedVersion, "不支持的端点或协议版本", nil)
	}
	host, portText, err := net.SplitHostPort(e.Address)
	if err != nil || e.Network != "tcp4" || host != "127.0.0.1" {
		return invalid("address", "端点只允许tcp4和127.0.0.1")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return invalid("address", "端点需要已分配的有效十进制端口")
	}
	if !validToken(e.Token) {
		return NewError(CodeAuthenticationFailed, "端点认证令牌格式无效", nil)
	}
	if e.PID <= 0 || e.StartedAt.IsZero() {
		return invalid("endpoint", "端点缺少进程和启动时间")
	}
	return nil
}

type Operation string

const (
	OperationSwitch                 Operation = "switch"
	OperationEnumerate              Operation = "enumerate"
	OperationEventCapabilities      Operation = "event_capabilities"
	OperationEmitEvent              Operation = "emit_event"
	OperationManagementCapabilities Operation = "management_capabilities"
	OperationManagement             Operation = "management"
)

type Request struct {
	Version    int                `json:"version"`
	ID         RequestID          `json:"id"`
	Token      string             `json:"token"`
	Operation  Operation          `json:"operation"`
	Switch     *SwitchRequest     `json:"switch,omitempty"`
	Event      *Event             `json:"event,omitempty"`
	Management *ManagementRequest `json:"management,omitempty"`
}

type Response struct {
	Version           int                 `json:"version"`
	ID                RequestID           `json:"id"`
	Operation         Operation           `json:"operation"`
	SwitchResult      *SwitchResult       `json:"switch_result,omitempty"`
	Devices           []Device            `json:"devices,omitempty"`
	EventCapabilities []EventCapability   `json:"event_capabilities,omitempty"`
	Accepted          bool                `json:"accepted,omitempty"`
	Error             *Error              `json:"error,omitempty"`
	Management        *ManagementResponse `json:"management,omitempty"`
}

func (r Request) Validate() error {
	if r.Version != ProtocolVersion {
		return NewError(CodeUnsupportedVersion, "不支持的请求协议版本", nil)
	}
	if !ValidID(string(r.ID)) {
		return invalid("id", "消息需要有效关联ID")
	}
	if !validToken(r.Token) {
		return NewError(CodeAuthenticationFailed, "请求认证令牌格式无效", nil)
	}
	if r.Operation != OperationManagement && r.Management != nil {
		return invalid("management", "其他操作不能包含管理负载")
	}
	switch r.Operation {
	case OperationSwitch:
		if r.Switch == nil || r.Event != nil || r.Switch.ID != r.ID {
			return invalid("switch", "切换负载必须存在、关联ID一致且无其他操作负载")
		}
		return r.Switch.Validate()
	case OperationEnumerate, OperationEventCapabilities, OperationManagementCapabilities:
		if r.Switch != nil || r.Event != nil {
			return invalid("operation", "只读操作不接受其他负载")
		}
	case OperationEmitEvent:
		// 外部自定义入口不能伪造平台USB或系统事件，避免绕过来源筛选。
		if r.Event == nil || r.Switch != nil || r.Event.Kind != EventCustom {
			return invalid("event", "外部入口只接受自定义事件")
		}
		return r.Event.Validate()
	case OperationManagement:
		if r.Management == nil || r.Switch != nil || r.Event != nil {
			return invalid("management", "管理操作需要独占管理负载")
		}
		return r.Management.Validate()
	default:
		return NewError(CodeUnsupportedOperation, "不支持的IPC操作", nil)
	}
	return nil
}

func (r Response) Validate() error {
	if r.Version != ProtocolVersion {
		return NewError(CodeUnsupportedVersion, "不支持的响应协议版本", nil)
	}
	if !ValidID(string(r.ID)) {
		return invalid("id", "响应需要有效关联ID")
	}
	if r.Error != nil {
		// 未知操作也需要用原操作名关联明确的拒绝；成功响应才要求已知操作。
		if !ValidID(string(r.Operation)) {
			return invalid("operation", "错误响应需要有效操作名")
		}
		if r.SwitchResult != nil || r.Devices != nil || r.EventCapabilities != nil || r.Accepted || r.Management != nil {
			return invalid("error", "请求级失败不能同时报告成功负载")
		}
		return r.Error.Validate()
	}
	if r.Operation != OperationManagement && r.Management != nil {
		return invalid("management", "其他响应不能包含管理负载")
	}
	switch r.Operation {
	case OperationSwitch:
		if r.SwitchResult == nil || r.SwitchResult.RequestID != r.ID || r.Devices != nil || r.EventCapabilities != nil || r.Accepted {
			return invalid("switch_result", "切换响应负载或关联ID不一致")
		}
		return r.SwitchResult.Validate()
	case OperationEnumerate:
		if r.SwitchResult != nil || r.EventCapabilities != nil || r.Accepted {
			return invalid("devices", "枚举响应包含其他操作负载")
		}
	case OperationEventCapabilities:
		if r.SwitchResult != nil || r.Devices != nil || r.Accepted {
			return invalid("event_capabilities", "事件能力响应包含其他操作负载")
		}
		for _, capability := range r.EventCapabilities {
			if err := capability.Validate(); err != nil {
				return err
			}
		}
	case OperationEmitEvent, OperationManagementCapabilities:
		if !r.Accepted || r.SwitchResult != nil || r.Devices != nil || r.EventCapabilities != nil {
			return invalid("accepted", "自定义事件响应只确认已接受，不代表发生切换")
		}
	case OperationManagement:
		if r.Management == nil || r.SwitchResult != nil || r.Devices != nil || r.EventCapabilities != nil || r.Accepted {
			return invalid("management", "管理响应必须独占管理负载")
		}
		return r.Management.Validate()
	default:
		return NewError(CodeUnsupportedOperation, "不支持的IPC操作", nil)
	}
	return nil
}

func DecodeRequest(data []byte) (Request, error) {
	var request Request
	if len(data) > MaxMessageBytes {
		return request, NewError(CodeMessageTooLarge, "IPC消息超过1MiB", nil)
	}
	if err := DecodeJSON(data, &request); err != nil {
		return Request{}, NewError(CodeInvalidRequest, "IPC请求JSON无效", err)
	}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func DecodeResponse(data []byte) (Response, error) {
	var response Response
	if len(data) > MaxMessageBytes {
		return response, NewError(CodeMessageTooLarge, "IPC消息超过1MiB", nil)
	}
	if err := DecodeJSON(data, &response); err != nil {
		return Response{}, NewError(CodeInvalidRequest, "IPC响应JSON无效", err)
	}
	if err := response.Validate(); err != nil {
		return Response{}, err
	}
	return response, nil
}

func validToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil && len(decoded) == 32
}
