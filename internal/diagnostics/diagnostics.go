// Package diagnostics通过已有后端队列提供只读诊断、人工校准和有限试切。
// 设备标识仅供本进程展示，日志或导出须使用EncodeReport的脱敏白名单。
package diagnostics

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/identity"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/switcher"
)

// Service只借用08已有队列，不构造后端或第二个队列。cfg必须与该队列的构造
// 快照一致；保存配置会关闭旧队列，调用方须用保存后的配置重建唯一队列及Service。
type Service struct {
	queue   *switcher.Queue
	cfg     config.Config
	mu      sync.Mutex
	retired bool
}

type EvidenceBasis string

const (
	BasisUnknown  EvidenceBasis = "unknown"
	BasisReadback EvidenceBasis = "readback"
	BasisManual   EvidenceBasis = "manual"
)

// Diagnostic中的候选只供展示；不能把其BackendID用于后续I/O。
// CanCalibrate仅表示身份足够明确，可以进入人工流程，不证明输入码受到设备支持。
type Diagnostic struct {
	Display          model.Display
	Candidates       []model.Device
	BackendIssue     *model.Error
	IdentityIssue    *model.Error
	Capabilities     model.DeviceCapabilities
	CapabilityIssue  *model.Error
	Observation      *model.InputObservation
	ObservationIssue *model.Error
	InputConfidence  EvidenceBasis
	CanCalibrate     bool
	Note             string
}

// BackendDiagnostic允许配置向导在尚无逻辑显示器时先检查控制后端。
// Available仅表示本次枚举成功，不证明每台显示器可读写DDC或所有输入都受支持。
type BackendDiagnostic struct {
	Available bool
	Devices   []model.Device
	Issue     *model.Error
	Note      string
}

func New(queue *switcher.Queue, cfg config.Config) (*Service, error) {
	if queue == nil {
		return nil, model.NewError(model.CodeBackendUnavailable, "诊断需要已有的唯一后端访问队列", nil)
	}
	snapshot, err := copyConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Service{queue: queue, cfg: snapshot}, nil
}

func (s *Service) InspectBackend(ctx context.Context) (BackendDiagnostic, error) {
	if err := s.active(); err != nil {
		return BackendDiagnostic{}, err
	}
	if err := ctx.Err(); err != nil {
		return BackendDiagnostic{}, explainError(err)
	}
	devices, err := s.queue.Enumerate(ctx)
	return BackendDiagnostic{
		Available: err == nil, Devices: devices, Issue: explainError(err),
		Note: "枚举成功只表示后端可响应，DDC/CI还受到权限、OSD设置、GPU、线缆和扩展坞影响，不能据此判断硬件损坏或画面成功。",
	}, nil
}

// Inspect在schemeID为空时只诊断身份和能力；非空时只读取明确配置的输入方案，
// 不构造默认码、不遍历可能值。能力列表只说明协议，不能当成完整物理端口清单。
func (s *Service) Inspect(ctx context.Context, id model.DisplayID, schemeID model.SchemeID) (Diagnostic, error) {
	var selected *model.InputScheme
	if schemeID != "" {
		for _, scheme := range s.cfg.InputSchemes {
			if scheme.ID == schemeID {
				copy := scheme
				selected = &copy
				break
			}
		}
		if selected == nil {
			return Diagnostic{}, model.NewError(model.CodeNotFound, "诊断输入方案不在当前配置中", nil)
		}
	}
	return s.inspect(ctx, id, selected)
}

func (s *Service) inspect(ctx context.Context, id model.DisplayID, scheme *model.InputScheme) (Diagnostic, error) {
	if err := s.active(); err != nil {
		return Diagnostic{}, err
	}
	if err := ctx.Err(); err != nil {
		return Diagnostic{}, explainError(err)
	}
	diagnostic := Diagnostic{
		InputConfidence: BasisUnknown,
		Note:            "DDC不响应可能与OSD中的DDC/CI开关、权限、GPU、线缆或扩展坞通路有关，不能据此判定显示器硬件损坏。能力列表可能不完整，不能据此猜输入码或禁止全部人工校准。",
	}
	found := false
	for _, display := range s.cfg.Displays {
		if display.ID == id {
			diagnostic.Display = display
			found = true
			break
		}
	}
	if !found {
		return diagnostic, model.NewError(model.CodeNotFound, "逻辑显示器不在当前配置中", nil)
	}
	if diagnostic.Display.Binding != nil {
		binding := *diagnostic.Display.Binding
		diagnostic.Display.Binding = &binding
	}
	backend, err := s.InspectBackend(ctx)
	if err != nil {
		return diagnostic, err
	}
	if backend.Issue != nil {
		diagnostic.BackendIssue = backend.Issue
		return diagnostic, nil
	}
	resolutions, err := identity.Resolve(s.cfg.Displays, backend.Devices)
	if err != nil {
		diagnostic.IdentityIssue = explainError(err)
		return diagnostic, nil
	}
	for _, resolution := range resolutions {
		if resolution.DisplayID != id {
			continue
		}
		diagnostic.Candidates = append([]model.Device(nil), resolution.Candidates...)
		if resolution.Error != nil {
			diagnostic.IdentityIssue = explainError(resolution.Error)
			return diagnostic, nil
		}
		if resolution.Device == nil {
			diagnostic.IdentityIssue = model.NewError(model.CodeNotFound, "当前没有可唯一解析的设备", nil)
			return diagnostic, nil
		}
		diagnostic.CanCalibrate = true
	}
	// 队列每次读取都重新解析逻辑身份；上面的枚举只是诊断快照，不能把候选对象
	// 直接交给后端，否则其他操作重枚举后可能使用已经失效的Windows设备引用。
	diagnostic.Capabilities, err = s.queue.Capabilities(ctx, id)
	if err != nil {
		diagnostic.CapabilityIssue = explainError(err)
		if code := model.CodeOf(err); code == model.CodeIdentityAmbiguous || code == model.CodeNotFound {
			diagnostic.CanCalibrate = false
			diagnostic.IdentityIssue = explainError(err)
			return diagnostic, nil
		}
	} else if diagnostic.Capabilities.UnavailableReason != "" {
		diagnostic.CapabilityIssue = model.NewError(model.CodeBackendUnavailable, "后端声明当前控制通路不可用，具体通路仍须检查", nil)
	} else if len(diagnostic.Capabilities.Protocols) == 0 {
		diagnostic.CapabilityIssue = model.NewError(model.CodeUnsupportedOperation, "未取得完整协议能力，输入端口支持情况仍未知，可由人工核对", nil)
	}
	if scheme == nil {
		return diagnostic, nil
	}
	started := time.Now()
	observation, readErr := s.queue.ReadInput(ctx, id, *scheme)
	if observation.Value != nil && !observation.ObservedAt.IsZero() {
		diagnostic.Observation = copyObservation(&observation)
	}
	if readErr != nil {
		diagnostic.ObservationIssue = explainError(readErr)
		if code := model.CodeOf(readErr); code == model.CodeIdentityAmbiguous || code == model.CodeNotFound {
			diagnostic.CanCalibrate = false
			diagnostic.IdentityIssue = explainError(readErr)
		}
	} else if diagnostic.Observation == nil || observation.ObservedAt.Before(started) {
		diagnostic.ObservationIssue = model.NewError(model.CodeTransport, "未取得本次读取的有效观测，历史值不能证明当前输入", nil)
	} else {
		diagnostic.InputConfidence = BasisReadback
	}
	return diagnostic, nil
}

func (s *Service) active() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retired {
		return model.NewError(model.CodeBackendUnavailable, "校准保存已停用旧队列，须核对保存结果并重建唯一队列", nil)
	}
	return nil
}

// copyConfig复用配置的唯一编码/校验入口取得独立快照，避免重复维护嵌套字段复制。
func copyConfig(cfg config.Config) (config.Config, error) {
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, cfg); err != nil {
		return config.Config{}, err
	}
	return config.Load(&encoded)
}

func copyObservation(observation *model.InputObservation) *model.InputObservation {
	if observation == nil {
		return nil
	}
	copy := *observation
	if observation.Value != nil {
		value := *observation.Value
		copy.Value = &value
	}
	return &copy
}

// explainError保留共享错误类别和进程内根因，但不把后端路径、子进程输出或认证
// 材料拼进展示文本。无法细分的错误明确保持未知，不把传输失败解释成硬件损坏。
func explainError(err error) *model.Error {
	if err == nil {
		return nil
	}
	code := model.CodeOf(err)
	message := "原因尚不明确，需要进一步检查，不能判断硬件是否损坏"
	switch code {
	case model.CodeUnsupportedOperation, model.CodeUnsupportedProtocol:
		message = "后端未提供所请求的能力或协议，不能由此推断所有端口均不支持"
	case model.CodePermissionDenied:
		message = "当前会话权限不足，需要检查设备和服务访问权限"
	case model.CodeUnreachable:
		message = "当前控制通路不可达，可能切走后失联，不能由此判断画面或硬件状态"
	case model.CodeBackendUnavailable:
		message = "当前后端或依赖服务不可用，需要检查运行环境和控制通路"
	case model.CodeIdentityAmbiguous:
		message = "显示器身份不唯一或路径变化，需要人工确认绑定"
	case model.CodeNotFound:
		message = "所需设备或配置当前缺席，保留已有映射并等待人工核对"
	case model.CodeCanceled:
		message = "操作已取消，已经发生或无法确定的发送仍须保留"
	case model.CodeTimeout:
		message = "操作已超时，不能据此判定未发送或画面成功"
	case model.CodeTransport:
		message = "控制传输或读回失败，输入和画面状态仍需核对"
	case model.CodeIO:
		message = "数据读写失败，需要核对实际保存或导出结果"
	}
	return model.NewError(code, message, err)
}

// ExplainResult只解释08保留的逐屏事实，读回一致也不能证明目标主机已有画面。
func ExplainResult(result model.DisplayResult) string {
	switch result.State {
	case model.StateAccepted:
		return "请求已接受，尚无发送证据，画面状态未知"
	case model.StateSent:
		return "控制请求已发送，尚无有效读回确认，画面状态未知"
	case model.StateConfirmed:
		return "发送后输入读回与目标一致，仍不能证明目标主机画面成功"
	case model.StateFailed:
		return "本屏请求失败，保留错误和已有观测，不能由此判断当前画面"
	default:
		if result.SentAt != nil {
			return "已保留发送证据，但读回或通路无法确认，画面状态未知"
		}
		return "提交结果未知，不能认定未发送，不能自动重发或回切，画面状态未知"
	}
}
