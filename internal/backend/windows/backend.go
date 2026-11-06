// Package windows提供可注入的Windows显示器协议后端，原生入口仅在Windows加载系统API。
package windows

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orzlee/displayKvm/internal/backend"
	"github.com/orzlee/displayKvm/internal/model"
)

// LogicalMonitor描述逻辑显示器，Handle仅供本次传输使用，不能释放为物理句柄。
// ID由传输提供，用于重新核验对象；不得仅从枚举数组下标或可复用的裸句柄生成。
type LogicalMonitor struct {
	ID     string
	Handle uintptr
}

// PhysicalMonitor中的Handle属于一次打开操作，必须由DestroyPhysicalMonitor释放。
// ID必须让传输在重新打开时核验同一物理对象，身份不足时应返回明确错误，不能猜测。
// Identity只是共享契约中的匹配线索；Capabilities必须来自传输实际提供的设备能力，
// 不能因系统为Windows、显示名含LG或存在VCP函数就宣称设备支持相应协议。
type PhysicalMonitor struct {
	ID           string
	Handle       uintptr
	Name         string
	Identity     model.IdentityHints
	Capabilities model.DeviceCapabilities
}

// Transport隔离枚举、物理句柄与VCP调用，使非Windows宿主不必加载原生API。
// GetPhysicalMonitors即使返回错误，也必须返回已经取得的全部物理句柄供调用方清理；
// 单次打开返回的句柄不得与其他尚未关闭的打开操作共用所有权。
// 销毁操作故意不接收ctx：取消不能跳过清理。同步原生调用若无法抢占，必须等其
// 返回再清理，禁止另起协程后提前返回并释放仍在使用中的句柄。
// 传输应保留实际错误，按共享model.Error区分权限、通路、不可用及不支持；
// SetVCPFeature返回错误不保证未发送，后端不自动重试或转用厂商协议。
type Transport interface {
	EnumDisplayMonitors(ctx context.Context) ([]LogicalMonitor, error)
	GetPhysicalMonitors(ctx context.Context, monitor LogicalMonitor) ([]PhysicalMonitor, error)
	DestroyPhysicalMonitor(monitor PhysicalMonitor) error
	GetVCPFeature(ctx context.Context, monitor PhysicalMonitor, code uint8) (uint32, error)
	SetVCPFeature(ctx context.Context, monitor PhysicalMonitor, code uint8, value uint32) error
}

// Backend只保存当前枚举周期的描述信息，不保存物理裸句柄。实际设备操作仍由上层
// 单一写入者串行调度；这里的锁仅保护枚举周期，不创建第二套队列或重试机制。
type Backend struct {
	transport  Transport
	instance   uint64
	mu         sync.RWMutex
	generation uint64
	bindings   map[string]deviceBinding
}

type monitorKey struct {
	logicalID  string
	physicalID string
}

type deviceBinding struct {
	key      monitorKey
	name     string
	identity model.IdentityHints
}

type openedMonitor struct {
	key      monitorKey
	physical PhysicalMonitor
}

type monitorSet struct {
	transport Transport
	monitors  []openedMonitor
	closed    bool
}

var instanceSequence atomic.Uint64

var _ backend.Backend = (*Backend)(nil)

func New(transport Transport) *Backend {
	return &Backend{transport: transport, instance: instanceSequence.Add(1)}
}

// NewNative保留明确的原生构造边界，构造本身不加载DLL、不探测或操作显示器。
// Windows在实际调用时核验系统入口、目标身份与设备能力；注入测试通过或DLL存在
// 都不能替代实际能力证据。其他平台保持明确不可用，不注册跨平台原生实现。
func NewNative() *Backend {
	return New(newPlatformTransport())
}

func (b *Backend) Name() string { return "windows" }

func (b *Backend) Enumerate(ctx context.Context) ([]model.Device, error) {
	// 新的枚举尝试立即终结旧周期，包括枚举失败的情况。否则失败后仍可能用旧引用
	// 操作已经热插拔或替换的设备，不能退回旧列表或第一个候选。
	b.mu.Lock()
	b.generation++
	generation := b.generation
	b.bindings = nil
	b.mu.Unlock()

	monitors, err := b.scan(ctx)
	if err != nil {
		return nil, err
	}
	devices := make([]model.Device, 0, len(monitors.monitors))
	bindings := make(map[string]deviceBinding, len(monitors.monitors))
	for index, monitor := range monitors.monitors {
		id := fmt.Sprintf("windows:%d:%d:%d", b.instance, generation, index)
		devices = append(devices, model.Device{
			BackendID: id, Name: monitor.physical.Name, Identity: monitor.physical.Identity,
		})
		bindings[id] = deviceBinding{key: monitor.key, name: monitor.physical.Name, identity: monitor.physical.Identity}
	}
	// 只有完整枚举且全部清理成功后才发布新周期；释放失败不能变成半份成功设备列表。
	if err = errors.Join(monitors.close(), operationError(ctx, "结束Windows显示器枚举", nil)); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.generation != generation {
		return nil, model.NewError(model.CodeNotFound, "Windows显示器枚举周期已失效", nil)
	}
	b.bindings = bindings
	return devices, nil
}

func (b *Backend) Capabilities(ctx context.Context, device model.Device) (capabilities model.DeviceCapabilities, err error) {
	err = b.withDevice(ctx, device, func(monitor PhysicalMonitor) error {
		capabilities = supportedCapabilities(monitor.Capabilities)
		return nil
	})
	if err != nil {
		return model.DeviceCapabilities{}, err
	}
	return capabilities, nil
}

func (b *Backend) ReadInput(ctx context.Context, device model.Device, scheme model.InputScheme) (observation model.InputObservation, err error) {
	if err = validateScheme(scheme); err != nil {
		return observation, err
	}
	err = b.withDevice(ctx, device, func(monitor PhysicalMonitor) error {
		if err := requireVCP(monitor, true); err != nil {
			return err
		}
		value, callErr := b.transport.GetVCPFeature(ctx, monitor, scheme.VCPCode)
		if callErr != nil {
			return operationError(ctx, "读取VCP 0x60", callErr)
		}
		if value > 65535 {
			return model.NewError(model.CodeTransport, "VCP读回超出输入值范围，不能截断为有效输入", nil)
		}
		current := uint16(value)
		observation = model.InputObservation{Value: &current, ObservedAt: time.Now()}
		// 同步读回已成功时，即使随后发现取消或释放失败，观测仍是有效历史事实。
		// 保留该值和原时间并同时返回错误，由上层决定是否可用于本次确认。
		return operationError(ctx, "完成VCP 0x60读回", nil)
	})
	return observation, err
}

func (b *Backend) SetInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
	if err := validateScheme(scheme); err != nil {
		return model.WriteReceipt{}, err
	}
	var sentAt time.Time
	err := b.withDevice(ctx, device, func(monitor PhysicalMonitor) error {
		if err := requireVCP(monitor, false); err != nil {
			return err
		}
		callErr := b.transport.SetVCPFeature(ctx, monitor, scheme.VCPCode, uint32(scheme.Value))
		if err := operationError(ctx, "提交VCP 0x60输入命令，失败时不能排除已经发送", callErr); err != nil {
			return err
		}
		sentAt = time.Now()
		return nil
	})
	if err != nil {
		// 提交后取消、传输失败或释放失败都可能已经改变输入。失败回执不是发送证据，
		// 也不能证明未发送；上层保留原错误并表达unknown，不能据此盲重试。
		return model.WriteReceipt{}, err
	}
	// 成功仅证明本次传输提交；独立ReadInput才提供观测，不能在这里声称画面成功。
	return model.WriteReceipt{SentAt: sentAt}, nil
}

func validateScheme(scheme model.InputScheme) error {
	if err := scheme.Validate(); err != nil {
		return err
	}
	if scheme.Protocol == model.ProtocolLGInputAlt {
		return model.NewError(model.CodeUnsupportedProtocol, "Windows后端未提供LG alternate厂商协议，不能转为标准VCP", nil)
	}
	return nil
}

func supportedCapabilities(reported model.DeviceCapabilities) model.DeviceCapabilities {
	result := model.DeviceCapabilities{UnavailableReason: reported.UnavailableReason}
	for _, protocol := range reported.Protocols {
		if protocol == model.ProtocolVCP {
			result.Protocols = []model.InputProtocol{model.ProtocolVCP}
			result.CanReadInput = reported.CanReadInput
			return result
		}
	}
	if result.UnavailableReason == "" {
		result.UnavailableReason = "设备未提供已实现的Windows输入控制协议"
	}
	return result
}

func requireVCP(monitor PhysicalMonitor, read bool) error {
	capabilities := supportedCapabilities(monitor.Capabilities)
	if len(capabilities.Protocols) == 0 {
		return model.NewError(model.CodeUnsupportedProtocol, "设备未报告标准VCP输入控制能力", nil)
	}
	if capabilities.UnavailableReason != "" {
		return model.NewError(model.CodeBackendUnavailable, "设备的Windows输入控制传输不可用", nil)
	}
	if read && !capabilities.CanReadInput {
		return model.NewError(model.CodeUnsupportedOperation, "设备不支持标准VCP输入读回", nil)
	}
	return nil
}

func (b *Backend) withDevice(ctx context.Context, device model.Device, operation func(PhysicalMonitor) error) (err error) {
	if err = operationError(ctx, "开始Windows显示器操作", nil); err != nil {
		return err
	}
	if b.transport == nil {
		return model.NewError(model.CodeBackendUnavailable, "Windows显示器传输未配置，原生适配不可用", nil)
	}
	b.mu.RLock()
	binding, found := b.bindings[device.BackendID]
	generation := b.generation
	b.mu.RUnlock()
	if !found {
		return model.NewError(model.CodeNotFound, "设备不属于当前Windows后端枚举周期", nil)
	}
	monitors, err := b.scan(ctx)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, monitors.close(), operationError(ctx, "结束Windows显示器操作", nil))
	}()
	for _, monitor := range monitors.monitors {
		if monitor.key != binding.key {
			continue
		}
		if monitor.physical.Name != binding.name || monitor.physical.Identity != binding.identity {
			return model.NewError(model.CodeUnreachable, "Windows显示器身份已变化，需要重新枚举和确认", nil)
		}
		b.mu.RLock()
		current := b.generation == generation
		b.mu.RUnlock()
		if !current {
			return model.NewError(model.CodeNotFound, "设备所属Windows显示器枚举周期已失效", nil)
		}
		if err = operationError(ctx, "访问Windows物理显示器前检查取消", nil); err != nil {
			return err
		}
		return operation(monitor.physical)
	}
	return model.NewError(model.CodeUnreachable, "原Windows显示器对象已不在当前通路，不能选择其他候选", nil)
}

func (b *Backend) scan(ctx context.Context) (set *monitorSet, err error) {
	if err = operationError(ctx, "开始枚举Windows显示器", nil); err != nil {
		return nil, err
	}
	if b.transport == nil {
		return nil, model.NewError(model.CodeBackendUnavailable, "Windows显示器传输未配置，原生适配不可用", nil)
	}
	set = &monitorSet{transport: b.transport}
	defer func() {
		if err != nil {
			err = errors.Join(err, set.close())
			set = nil
		}
	}()
	logicalMonitors, callErr := b.transport.EnumDisplayMonitors(ctx)
	if err = operationError(ctx, "枚举Windows逻辑显示器", callErr); err != nil {
		return set, err
	}
	logicalIDs := make(map[string]bool)
	physicalIDs := make(map[monitorKey]bool)
	handles := make(map[uintptr]bool)
	for _, logical := range logicalMonitors {
		if logical.ID == "" || logicalIDs[logical.ID] {
			return set, model.NewError(model.CodeIdentityAmbiguous, "逻辑显示器缺少可核验的唯一对象标记", nil)
		}
		logicalIDs[logical.ID] = true
		if err = operationError(ctx, "打开Windows物理显示器前检查取消", nil); err != nil {
			return set, err
		}
		physicalMonitors, openErr := b.transport.GetPhysicalMonitors(ctx, logical)
		var ownershipErr error
		// 部分失败和取消可能同时带回已取得的句柄，必须先完整登记所有权，再处理错误。
		// 重复的非零句柄只登记一次，避免异常传输导致重复释放；零句柄不代表有效资源。
		for _, physical := range physicalMonitors {
			if physical.Handle == 0 || handles[physical.Handle] {
				ownershipErr = errors.Join(ownershipErr, model.NewError(model.CodeTransport, "物理显示器传输返回无效或重复句柄", nil))
				continue
			}
			handles[physical.Handle] = true
			key := monitorKey{logicalID: logical.ID, physicalID: physical.ID}
			set.monitors = append(set.monitors, openedMonitor{key: key, physical: physical})
			if physical.ID == "" || physicalIDs[key] {
				ownershipErr = errors.Join(ownershipErr, model.NewError(model.CodeIdentityAmbiguous, "物理显示器缺少可核验的唯一对象标记", nil))
			}
			physicalIDs[key] = true
		}
		if err = operationError(ctx, "取得Windows物理显示器", errors.Join(openErr, ownershipErr)); err != nil {
			return set, err
		}
	}
	if err = operationError(ctx, "完成Windows物理显示器枚举", nil); err != nil {
		return set, err
	}
	return set, nil
}

func (set *monitorSet) close() error {
	if set.closed {
		return nil
	}
	set.closed = true
	var result error
	for _, monitor := range set.monitors {
		if err := set.transport.DestroyPhysicalMonitor(monitor.physical); err != nil {
			// 一次释放失败也继续清理其余物理句柄。保留每个失败的原始原因和阶段，
			// 不重试销毁未知状态的句柄，也绝不对逻辑HMONITOR调用物理句柄释放接口。
			result = errors.Join(result, transportError("释放Windows物理显示器", err))
		}
	}
	return result
}

func operationError(ctx context.Context, stage string, cause error) error {
	return transportError(stage, errors.Join(cause, ctx.Err()))
}

func transportError(stage string, cause error) error {
	if cause == nil {
		return nil
	}
	code := model.CodeOf(cause)
	var classified *model.Error
	if !errors.As(cause, &classified) && code != model.CodeCanceled && code != model.CodeTimeout {
		switch {
		case errors.Is(cause, fs.ErrPermission):
			code = model.CodePermissionDenied
		case errors.Is(cause, fs.ErrNotExist):
			code = model.CodeUnreachable
		case errors.Is(cause, errors.ErrUnsupported):
			code = model.CodeUnsupportedOperation
		default:
			code = model.CodeTransport
		}
	}
	// model.Error的可见文本不展开cause，需在消息中保留底层API阶段和原码，
	// 避免再次包装后丢失诊断依据；同一cause仍用于错误链和既有分类。
	return model.NewError(code, stage+"失败："+cause.Error(), cause)
}
