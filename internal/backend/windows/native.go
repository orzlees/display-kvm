package windows

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"github.com/orzlee/displayKvm/internal/model"
)

const (
	maxNativeDisplayItems = 4096
	maxNativeCapabilities = 1 << 16
	maxCapabilityDepth    = 32
	nativeTopologyTries   = 3
)

type nativeLUID struct {
	LowPart  uint32
	HighPart int32
}

type nativeDisplayID struct {
	Adapter nativeLUID
	ID      uint32
}

type nativeDisplayPath struct {
	Source nativeDisplayID
	Target nativeDisplayID
}

type nativeTargetName struct {
	Path         string
	FriendlyName string
}

type nativePhysicalMonitor struct {
	Handle      uintptr
	Description string
}

// nativeAPI保持原生调用边界，宿主测试只替换系统入口，仍执行真实身份映射和能力解析。
// 返回物理句柄的调用即使失败也必须交回已填充的句柄，交由共享Backend统一释放。
type nativeAPI interface {
	probe() error
	displayConfigBufferSizes() (uint32, uint32, error)
	queryDisplayConfig(pathCount, modeCount uint32) ([]nativeDisplayPath, error)
	sourceName(nativeDisplayID) (string, error)
	targetName(nativeDisplayID) (nativeTargetName, error)
	enumDisplayMonitors() ([]uintptr, error)
	monitorName(uintptr) (string, error)
	physicalMonitorCount(uintptr) (uint32, error)
	physicalMonitors(uintptr, uint32) ([]nativePhysicalMonitor, error)
	capabilitiesLength(uintptr) (uint32, error)
	capabilities(uintptr, uint32) ([]byte, error)
	getVCP(uintptr, uint8) (value, maximum uint32, err error)
	setVCP(uintptr, uint8, uint32) error
	destroyPhysicalMonitor(uintptr) error
}

type nativeTransport struct{ api nativeAPI }

var _ Transport = (*nativeTransport)(nil)

func newNativeTransport(api nativeAPI) *nativeTransport { return &nativeTransport{api: api} }

func (t *nativeTransport) EnumDisplayMonitors(ctx context.Context) ([]LogicalMonitor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := t.api.probe(); err != nil {
		return nil, nativeError("加载Windows显示器API", err)
	}
	mapping, err := t.displayMapping(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	handles, err := t.api.enumDisplayMonitors()
	if err != nil {
		return nil, nativeError("EnumDisplayMonitors", err)
	}
	if len(handles) > maxNativeDisplayItems {
		return nil, nativeInvalid("EnumDisplayMonitors返回的数量超出安全上限")
	}
	result := make([]LogicalMonitor, 0, len(handles))
	seenHandles, seenPaths := make(map[uintptr]bool), make(map[string]bool)
	for _, handle := range handles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if handle == 0 || seenHandles[handle] {
			return nil, nativeAmbiguous("逻辑显示器句柄为空或重复")
		}
		seenHandles[handle] = true
		gdi, err := t.api.monitorName(handle)
		if err != nil {
			return nil, nativeError("GetMonitorInfoW", err)
		}
		path, ok := mapping[normalizeNativeName(gdi)]
		if !ok || path.Path == "" || seenPaths[path.Path] {
			return nil, nativeAmbiguous("逻辑显示器无法唯一关联活动目标路径")
		}
		seenPaths[path.Path] = true
		result = append(result, LogicalMonitor{ID: path.Path, Handle: handle})
	}
	return result, ctx.Err()
}

// displayMapping仅接受一个活动source对应一个target且目标路径唯一的拓扑。
// 克隆输出和缺路径不能依靠数组顺序、友好名称或可复用的HMONITOR猜测身份。
func (t *nativeTransport) displayMapping(ctx context.Context) (map[string]nativeTargetName, error) {
	var paths []nativeDisplayPath
	for attempt := 0; attempt < nativeTopologyTries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pathCount, modeCount, err := t.api.displayConfigBufferSizes()
		if err != nil {
			if errors.Is(err, syscall.Errno(122)) && attempt+1 < nativeTopologyTries {
				continue
			}
			return nil, nativeError("GetDisplayConfigBufferSizes", err)
		}
		if pathCount > maxNativeDisplayItems || modeCount > maxNativeDisplayItems {
			return nil, nativeInvalid("活动显示拓扑数量超出安全上限")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		paths, err = t.api.queryDisplayConfig(pathCount, modeCount)
		if err != nil {
			// 只有API明确报告缓冲区因拓扑竞争不足时重查；未知失败不能变成回退或重试。
			if errors.Is(err, syscall.Errno(122)) && attempt+1 < nativeTopologyTries {
				continue
			}
			return nil, nativeError("QueryDisplayConfig", err)
		}
		if len(paths) > int(pathCount) {
			return nil, nativeInvalid("QueryDisplayConfig返回数量超过提供的缓冲区")
		}
		break
	}
	mapping := make(map[string]nativeTargetName, len(paths))
	seenSources, seenTargets, seenPaths := make(map[nativeDisplayID]bool), make(map[nativeDisplayID]bool), make(map[string]bool)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if seenSources[path.Source] || seenTargets[path.Target] {
			return nil, nativeAmbiguous("活动source或target并非一对一关系，不能按顺序关联显示器")
		}
		seenSources[path.Source], seenTargets[path.Target] = true, true
		gdi, err := t.api.sourceName(path.Source)
		if err != nil {
			return nil, nativeError("DisplayConfigGetDeviceInfo(source)", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target, err := t.api.targetName(path.Target)
		if err != nil {
			return nil, nativeError("DisplayConfigGetDeviceInfo(target)", err)
		}
		gdi, target.Path = normalizeNativeName(gdi), normalizeNativeName(target.Path)
		if gdi == "" || target.Path == "" || strings.ContainsRune(gdi, 0) || strings.ContainsRune(target.Path, 0) || seenPaths[target.Path] {
			return nil, nativeAmbiguous("活动显示目标缺少唯一的GDI名称或monitorDevicePath")
		}
		if _, exists := mapping[gdi]; exists {
			return nil, nativeAmbiguous("同一GDI名称对应多个活动显示目标")
		}
		seenPaths[target.Path] = true
		mapping[gdi] = target
	}
	return mapping, ctx.Err()
}

// targetForMonitor在打开前后复核拓扑；HMONITOR可能在同步打开期间因热插拔改变关联。
// 仅在打开前检查会把已经取得的替换设备句柄错误标为旧路径，导致后续身份核验失效。
func (t *nativeTransport) targetForMonitor(ctx context.Context, monitor LogicalMonitor) (nativeTargetName, error) {
	mapping, err := t.displayMapping(ctx)
	if err != nil {
		return nativeTargetName{}, err
	}
	gdi, err := t.api.monitorName(monitor.Handle)
	if err != nil {
		return nativeTargetName{}, nativeError("GetMonitorInfoW", err)
	}
	target, ok := mapping[normalizeNativeName(gdi)]
	if !ok || target.Path == "" || target.Path != monitor.ID {
		return nativeTargetName{}, model.NewError(model.CodeUnreachable, "Windows显示目标路径在打开期间已经变化", nil)
	}
	return target, ctx.Err()
}

func (t *nativeTransport) GetPhysicalMonitors(ctx context.Context, monitor LogicalMonitor) ([]PhysicalMonitor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := t.targetForMonitor(ctx, monitor)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	count, err := t.api.physicalMonitorCount(monitor.Handle)
	if err != nil {
		// 尚未取得真实HANDLE时无法发布不可用设备；保留原错误让共享扫描失败并清理
		// 此前屏幕的句柄，不能无声跳过失败或构造假句柄来伪装成功枚举。
		return nil, nativeError("GetNumberOfPhysicalMonitorsFromHMONITOR", err)
	}
	if count == 0 {
		return nil, nil
	}
	if count > maxNativeDisplayItems {
		return nil, nativeInvalid("物理显示器数量超出安全上限")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	physical, openErr := t.api.physicalMonitors(monitor.Handle, count)
	result := make([]PhysicalMonitor, 0, len(physical))
	for _, item := range physical {
		name := item.Description
		if name == "" {
			name = target.FriendlyName
		}
		if name == "" {
			name = target.Path
		}
		result = append(result, PhysicalMonitor{
			ID: target.Path, Handle: item.Handle, Name: name,
			Identity: model.IdentityHints{Path: target.Path},
		})
	}
	// 从这里起，即使适配、取消或能力查询失败，也必须把所有已取得句柄交回Backend。
	if openErr != nil {
		return result, nativeError("GetPhysicalMonitorsFromHMONITOR", openErr)
	}
	if count != 1 || len(result) != 1 {
		return result, nativeAmbiguous("HMONITOR不能唯一对应一个物理显示器，禁止按描述或顺序猜测")
	}
	if result[0].Handle == 0 {
		return result, nativeInvalid("GetPhysicalMonitorsFromHMONITOR返回空句柄")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := t.targetForMonitor(ctx, monitor); err != nil {
		return result, err
	}
	result[0].Capabilities, err = t.monitorCapabilities(ctx, result[0].Handle)
	return result, err
}

func (t *nativeTransport) monitorCapabilities(ctx context.Context, handle uintptr) (model.DeviceCapabilities, error) {
	length, err := t.api.capabilitiesLength(handle)
	if err != nil {
		return nativeCapabilityFailure("GetCapabilitiesStringLength", err)
	}
	if length < 2 || length > maxNativeCapabilities {
		return model.DeviceCapabilities{}, nativeInvalid("GetCapabilitiesStringLength返回不可信的能力串长度")
	}
	if err := ctx.Err(); err != nil {
		return model.DeviceCapabilities{}, err
	}
	data, err := t.api.capabilities(handle, length)
	if err != nil {
		return nativeCapabilityFailure("CapabilitiesRequestAndCapabilitiesReply", err)
	}
	if len(data) != int(length) {
		return model.DeviceCapabilities{}, nativeInvalid("能力串缓冲区长度与API声明不一致")
	}
	supported, err := parseNativeCapabilities(data)
	if err != nil {
		return model.DeviceCapabilities{}, err
	}
	if !supported {
		return model.DeviceCapabilities{UnavailableReason: "显示器真实MCCS能力未报告VCP 0x60输入控制"}, nil
	}
	return model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolVCP}, CanReadInput: true}, ctx.Err()
}

func nativeCapabilityFailure(stage string, cause error) (model.DeviceCapabilities, error) {
	err := nativeError(stage, cause)
	if nativeUnsupported(cause) {
		// 明确的不支持只影响当前屏；权限、设备失联和未知传输失败必须继续作为错误返回。
		return model.DeviceCapabilities{UnavailableReason: "显示器不支持原生DDC输入能力：" + err.Error()}, nil
	}
	return model.DeviceCapabilities{}, err
}

func (t *nativeTransport) DestroyPhysicalMonitor(monitor PhysicalMonitor) error {
	return nativeError("DestroyPhysicalMonitor", t.api.destroyPhysicalMonitor(monitor.Handle))
}

func (t *nativeTransport) GetVCPFeature(ctx context.Context, monitor PhysicalMonitor, code uint8) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	value, _, err := t.api.getVCP(monitor.Handle, code)
	if err != nil {
		return 0, nativeError("GetVCPFeatureAndVCPFeatureReply", err)
	}
	// 0x60是非连续输入源代码，maximum不能作为端口取值范围。保留完整current，
	// 由共享Backend已有的uint16边界拒绝越界，避免截断或误拒驱动的合法非连续读数。
	// 成功读回是已发生的观测。不能在这里用随后取消替换成功值；共享Backend先记录
	// 值和时间，再合并取消与清理错误，保持历史观测可追溯。
	return value, nil
}

func (t *nativeTransport) SetVCPFeature(ctx context.Context, monitor PhysicalMonitor, code uint8, value uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Win32 DDC调用同步且不可抢占。不启动后台协程或提前释放使用中的句柄；写后取消
	// 由共享Backend合并为失败及零回执，上层据此表达unknown而不是自动重发。
	return nativeError("SetVCPFeature", t.api.setVCP(monitor.Handle, code, value))
}

func normalizeNativeName(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func nativeAmbiguous(message string) error {
	return model.NewError(model.CodeIdentityAmbiguous, message, nil)
}

func nativeInvalid(message string) error { return model.NewError(model.CodeTransport, message, nil) }

func nativeUnsupported(err error) bool {
	// 只认可明确的不支持结果。ERROR_GEN_FAILURE等未知失败不能被伪装成内屏不支持。
	return errors.Is(err, syscall.Errno(50)) || // ERROR_NOT_SUPPORTED
		errors.Is(err, syscall.Errno(0xc0262580)) || // ERROR_GRAPHICS_I2C_NOT_SUPPORTED
		errors.Is(err, syscall.Errno(0xc0262584)) // ERROR_GRAPHICS_DDCCI_VCP_NOT_SUPPORTED
}

func nativeError(stage string, cause error) error {
	if cause == nil {
		return nil
	}
	var errno syscall.Errno
	code := model.CodeTransport
	if errors.As(cause, &errno) {
		cause = fmt.Errorf("Win32 %d (0x%08X): %w", uint32(errno), uint32(errno), cause)
		switch errno {
		case 5:
			code = model.CodePermissionDenied
		case 126, 127, 1157:
			code = model.CodeBackendUnavailable
		case 6, 1167, 0xc0262581, 0xc026258c, 0xc026258d:
			code = model.CodeUnreachable
		default:
			if nativeUnsupported(cause) {
				code = model.CodeUnsupportedOperation
			}
		}
	}
	// model.Error的文本不自动包含cause；原码既要保留在可errors.Is追溯的错误链中，
	// 也要写入消息，否则能力隔离后仅剩UnavailableReason时会丢失排查依据。
	return model.NewError(code, stage+"失败："+cause.Error(), cause)
}

func nativeBOOLResult(stage string, result uintptr, cause error) error {
	if result != 0 {
		return nil
	}
	if cause == nil || errors.Is(cause, syscall.Errno(0)) {
		cause = errors.New("系统调用返回FALSE但未提供扩展错误码")
	}
	return nativeError(stage, cause)
}

// parseNativeCapabilities只解释根能力串的vcp分区；model/name中的“60”不能证明支持。
// 长度、NUL、嵌套深度和十六进制token都来自不可信设备，必须在读取和转换前有界校验。
// 原包没有MCCS解析器，因此在原生边界局部实现，不扩展公共能力模型或生成端口默认值。
func parseNativeCapabilities(data []byte) (bool, error) {
	if len(data) < 2 || len(data) > maxNativeCapabilities || data[len(data)-1] != 0 || bytes.IndexByte(data[:len(data)-1], 0) >= 0 {
		return false, nativeInvalid("MCCS能力串缺少正确的NUL边界")
	}
	text := strings.TrimSpace(string(data[:len(data)-1]))
	if len(text) < 2 || text[0] != '(' {
		return false, nativeInvalid("MCCS能力串缺少根括号")
	}
	end, err := nativeCapabilityEnd(text, 0)
	if err != nil || end != len(text)-1 {
		return false, nativeInvalid("MCCS能力串括号损坏或存在额外数据")
	}
	found, supported := false, false
	for offset := 1; offset < end; {
		for offset < end && nativeSpace(text[offset]) {
			offset++
		}
		if offset == end {
			break
		}
		start := offset
		for offset < end && !nativeSpace(text[offset]) && text[offset] != '(' && text[offset] != ')' {
			offset++
		}
		name := text[start:offset]
		for offset < end && nativeSpace(text[offset]) {
			offset++
		}
		if name == "" || offset >= end || text[offset] != '(' {
			return false, nativeInvalid("MCCS能力分区缺少名称或括号")
		}
		closeAt, err := nativeCapabilityEnd(text, offset)
		if err != nil {
			return false, err
		}
		if strings.EqualFold(name, "vcp") {
			if found {
				return false, nativeInvalid("MCCS能力串含重复vcp分区")
			}
			found = true
			supported, err = nativeVCPSection(text[offset+1 : closeAt])
			if err != nil {
				return false, err
			}
		}
		offset = closeAt + 1
	}
	return supported, nil
}

func nativeCapabilityEnd(text string, start int) (int, error) {
	depth := 0
	for offset := start; offset < len(text); offset++ {
		switch text[offset] {
		case '(':
			depth++
			if depth > maxCapabilityDepth {
				return 0, nativeInvalid("MCCS能力串嵌套超过安全上限")
			}
		case ')':
			depth--
			if depth == 0 {
				return offset, nil
			}
		}
		if text[offset] < 0x20 && !nativeSpace(text[offset]) {
			return 0, nativeInvalid("MCCS能力串含非法控制字符")
		}
	}
	return 0, nativeInvalid("MCCS能力串括号未闭合")
}

func nativeVCPSection(text string) (bool, error) {
	supported := false
	for offset := 0; offset < len(text); {
		for offset < len(text) && nativeSpace(text[offset]) {
			offset++
		}
		if offset == len(text) {
			break
		}
		start := offset
		for offset < len(text) && !nativeSpace(text[offset]) && text[offset] != '(' && text[offset] != ')' {
			offset++
		}
		token := text[start:offset]
		if len(token) == 0 || len(token) > 2 {
			return false, nativeInvalid("MCCS VCP特性代码无效")
		}
		feature, err := strconv.ParseUint(token, 16, 8)
		if err != nil {
			return false, nativeInvalid("MCCS VCP特性代码不是完整十六进制token")
		}
		if feature == 0x60 {
			supported = true
		}
		for offset < len(text) && nativeSpace(text[offset]) {
			offset++
		}
		if offset < len(text) && text[offset] == '(' {
			end, err := nativeCapabilityEnd(text, offset)
			if err != nil {
				return false, err
			}
			// 输入值仅校验真实语法，不推导HDMI/DP含义；目标值继续使用现有配置uint16。
			values := strings.FieldsFunc(text[offset+1:end], func(c rune) bool { return c == '(' || c == ')' || c == ' ' || c == '\t' || c == '\r' || c == '\n' })
			for _, value := range values {
				if len(value) > 4 {
					return false, nativeInvalid("MCCS VCP值超出uint16范围")
				}
				if _, err := strconv.ParseUint(value, 16, 16); err != nil {
					return false, nativeInvalid("MCCS VCP值不是完整十六进制token")
				}
			}
			offset = end + 1
		}
	}
	return supported, nil
}

func nativeSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
