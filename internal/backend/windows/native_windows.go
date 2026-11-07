//go:build windows

package windows

import (
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

const qdcOnlyActivePaths = 2

type winPathSourceInfo struct {
	Adapter     nativeLUID
	ID          uint32
	ModeInfoIdx uint32
	StatusFlags uint32
}

type winPathTargetInfo struct {
	Adapter          nativeLUID
	ID               uint32
	ModeInfoIdx      uint32
	OutputTechnology uint32
	Rotation         uint32
	Scaling          uint32
	RefreshRate      struct{ Numerator, Denominator uint32 }
	ScanlineOrdering uint32
	TargetAvailable  int32
	StatusFlags      uint32
}

type winPathInfo struct {
	Source winPathSourceInfo
	Target winPathTargetInfo
	Flags  uint32
}

// 模式union只作为QueryDisplayConfig的有界输出缓冲区，不解释模式或EDID身份。
// uint64数组保留64位Windows union的自然对齐，头16字节加48字节内容共64字节。
type winModeInfo struct {
	InfoType uint32
	ID       uint32
	Adapter  nativeLUID
	Data     [6]uint64
}

type winDeviceInfoHeader struct {
	Type    uint32
	Size    uint32
	Adapter nativeLUID
	ID      uint32
}

type winSourceDeviceName struct {
	Header  winDeviceInfoHeader
	GDIName [32]uint16
}

type winTargetDeviceName struct {
	Header           winDeviceInfoHeader
	Flags            uint32
	OutputTechnology uint32
	EDIDManufacturer uint16
	EDIDProduct      uint16
	Connector        uint32
	FriendlyName     [64]uint16
	DevicePath       [128]uint16
}

type winRect struct{ Left, Top, Right, Bottom int32 }

type winMonitorInfo struct {
	Size    uint32
	Monitor winRect
	Work    winRect
	Flags   uint32
	Device  [32]uint16
}

type winPhysicalMonitor struct {
	Handle      uintptr
	Description [128]uint16
}

type winProcedures struct {
	enumMonitors *syscall.LazyProc
	monitorInfo  *syscall.LazyProc
	bufferSizes  *syscall.LazyProc
	queryConfig  *syscall.LazyProc
	deviceInfo   *syscall.LazyProc
	physicalNum  *syscall.LazyProc
	physical     *syscall.LazyProc
	capLength    *syscall.LazyProc
	capReply     *syscall.LazyProc
	getVCP       *syscall.LazyProc
	setVCP       *syscall.LazyProc
	destroy      *syscall.LazyProc
}

// 仅系统绑定和无捕获回调按进程缓存；每次枚举的句柄和上下文不进入全局业务状态。
var (
	winProceduresOnce sync.Once
	winBindings       *winProcedures
	winBindingsErr    error
	winCallbackOnce   sync.Once
	winCallback       uintptr
)

type windowsNativeAPI struct{}

func newPlatformTransport() Transport { return newNativeTransport(windowsNativeAPI{}) }

func windowsProcedures() (*winProcedures, error) {
	winProceduresOnce.Do(func() {
		winBindings, winBindingsErr = loadWindowsProcedures()
	})
	return winBindings, winBindingsErr
}

func loadWindowsProcedures() (*winProcedures, error) {
	// kernel32是Windows已加载的KnownDLL且也是Go自身使用的系统DLL。先从它取得真实
	// 系统目录，再用绝对路径加载user32/dxva2，避免按工作目录或PATH搜索同名DLL。
	directory, err := windowsSystemDirectory()
	if err != nil {
		return nil, err
	}
	user32 := syscall.NewLazyDLL(filepath.Join(directory, "user32.dll"))
	dxva2 := syscall.NewLazyDLL(filepath.Join(directory, "dxva2.dll"))
	p := &winProcedures{
		enumMonitors: user32.NewProc("EnumDisplayMonitors"),
		monitorInfo:  user32.NewProc("GetMonitorInfoW"),
		bufferSizes:  user32.NewProc("GetDisplayConfigBufferSizes"),
		queryConfig:  user32.NewProc("QueryDisplayConfig"),
		deviceInfo:   user32.NewProc("DisplayConfigGetDeviceInfo"),
		physicalNum:  dxva2.NewProc("GetNumberOfPhysicalMonitorsFromHMONITOR"),
		physical:     dxva2.NewProc("GetPhysicalMonitorsFromHMONITOR"),
		capLength:    dxva2.NewProc("GetCapabilitiesStringLength"),
		capReply:     dxva2.NewProc("CapabilitiesRequestAndCapabilitiesReply"),
		getVCP:       dxva2.NewProc("GetVCPFeatureAndVCPFeatureReply"),
		setVCP:       dxva2.NewProc("SetVCPFeature"),
		destroy:      dxva2.NewProc("DestroyPhysicalMonitor"),
	}
	for _, proc := range []*syscall.LazyProc{p.enumMonitors, p.monitorInfo, p.bufferSizes, p.queryConfig, p.deviceInfo,
		p.physicalNum, p.physical, p.capLength, p.capReply, p.getVCP, p.setVCP, p.destroy} {
		// Find保留缺失DLL或入口的原始错误；不使用MustLoadDLL/MustFindProc或带隐式加载的裸Call。
		if err := proc.Find(); err != nil {
			return nil, nativeError("查找Windows显示器入口 "+proc.Name, err)
		}
	}
	return p, nil
}

func (windowsNativeAPI) probe() error {
	_, err := windowsProcedures()
	return err
}

func (windowsNativeAPI) displayConfigBufferSizes() (uint32, uint32, error) {
	p, err := windowsProcedures()
	if err != nil {
		return 0, 0, err
	}
	var paths, modes uint32
	status, _, _ := p.bufferSizes.Call(qdcOnlyActivePaths, uintptr(unsafe.Pointer(&paths)), uintptr(unsafe.Pointer(&modes)))
	// DisplayConfig系列返回LONG错误码本身，不能用BOOL语义解读遗留GetLastError。
	if uint32(status) != 0 {
		return 0, 0, syscall.Errno(uint32(status))
	}
	return paths, modes, nil
}

func (windowsNativeAPI) queryDisplayConfig(pathCount, modeCount uint32) ([]nativeDisplayPath, error) {
	p, err := windowsProcedures()
	if err != nil {
		return nil, err
	}
	if pathCount > maxNativeDisplayItems || modeCount > maxNativeDisplayItems {
		return nil, nativeInvalid("QueryDisplayConfig分配数量超过安全上限")
	}
	paths, modes := make([]winPathInfo, int(pathCount)), make([]winModeInfo, int(modeCount))
	var pathPointer, modePointer uintptr
	if len(paths) != 0 {
		pathPointer = uintptr(unsafe.Pointer(&paths[0]))
	}
	if len(modes) != 0 {
		modePointer = uintptr(unsafe.Pointer(&modes[0]))
	}
	actualPaths, actualModes := pathCount, modeCount
	status, _, _ := p.queryConfig.Call(qdcOnlyActivePaths, uintptr(unsafe.Pointer(&actualPaths)), pathPointer,
		uintptr(unsafe.Pointer(&actualModes)), modePointer, 0)
	runtime.KeepAlive(paths)
	runtime.KeepAlive(modes)
	if uint32(status) != 0 {
		return nil, syscall.Errno(uint32(status))
	}
	if actualPaths > pathCount || actualModes > modeCount {
		return nil, nativeInvalid("QueryDisplayConfig返回数量超出缓冲区")
	}
	result := make([]nativeDisplayPath, 0, int(actualPaths))
	for _, path := range paths[:actualPaths] {
		if path.Flags&1 == 0 {
			return nil, nativeInvalid("QueryDisplayConfig活动路径缺少ACTIVE标记")
		}
		result = append(result, nativeDisplayPath{
			Source: nativeDisplayID{Adapter: path.Source.Adapter, ID: path.Source.ID},
			Target: nativeDisplayID{Adapter: path.Target.Adapter, ID: path.Target.ID},
		})
	}
	return result, nil
}

func (windowsNativeAPI) sourceName(id nativeDisplayID) (string, error) {
	p, err := windowsProcedures()
	if err != nil {
		return "", err
	}
	var info winSourceDeviceName
	info.Header = winDeviceInfoHeader{Type: 1, Size: uint32(unsafe.Sizeof(info)), Adapter: id.Adapter, ID: id.ID}
	status, _, _ := p.deviceInfo.Call(uintptr(unsafe.Pointer(&info)))
	if uint32(status) != 0 {
		return "", syscall.Errno(uint32(status))
	}
	return winUTF16String(info.GDIName[:])
}

func (windowsNativeAPI) targetName(id nativeDisplayID) (nativeTargetName, error) {
	p, err := windowsProcedures()
	if err != nil {
		return nativeTargetName{}, err
	}
	var info winTargetDeviceName
	info.Header = winDeviceInfoHeader{Type: 2, Size: uint32(unsafe.Sizeof(info)), Adapter: id.Adapter, ID: id.ID}
	status, _, _ := p.deviceInfo.Call(uintptr(unsafe.Pointer(&info)))
	if uint32(status) != 0 {
		return nativeTargetName{}, syscall.Errno(uint32(status))
	}
	path, pathErr := winUTF16String(info.DevicePath[:])
	name, nameErr := winUTF16String(info.FriendlyName[:])
	return nativeTargetName{Path: path, FriendlyName: name}, errors.Join(pathErr, nameErr)
}

type winMonitorEnumeration struct {
	monitors []uintptr
	err      error
}

func winMonitorCallback(monitor, _, _, data uintptr) uintptr {
	state := (*winMonitorEnumeration)(unsafe.Pointer(data))
	if len(state.monitors) >= maxNativeDisplayItems {
		state.err = nativeInvalid("EnumDisplayMonitors超过安全数量上限")
		return 0
	}
	state.monitors = append(state.monitors, monitor)
	return 1
}

func (windowsNativeAPI) enumDisplayMonitors() ([]uintptr, error) {
	p, err := windowsProcedures()
	if err != nil {
		return nil, err
	}
	// NewCallback的永久桩数量有限，只创建一次无捕获回调。LPARAM指向本次调用独有的
	// 已固定Go对象；EnumDisplayMonitors同步返回后才解除固定，回调不会持有跨调用状态。
	winCallbackOnce.Do(func() { winCallback = syscall.NewCallback(winMonitorCallback) })
	state := &winMonitorEnumeration{}
	var pin runtime.Pinner
	pin.Pin(state)
	defer pin.Unpin()
	ok, _, callErr := p.enumMonitors.Call(0, 0, winCallback, uintptr(unsafe.Pointer(state)))
	runtime.KeepAlive(state)
	if state.err != nil {
		return state.monitors, state.err
	}
	return state.monitors, nativeBOOLResult("EnumDisplayMonitors", ok, callErr)
}

func (windowsNativeAPI) monitorName(handle uintptr) (string, error) {
	p, err := windowsProcedures()
	if err != nil {
		return "", err
	}
	var info winMonitorInfo
	info.Size = uint32(unsafe.Sizeof(info))
	ok, _, callErr := p.monitorInfo.Call(handle, uintptr(unsafe.Pointer(&info)))
	if err := nativeBOOLResult("GetMonitorInfoW", ok, callErr); err != nil {
		return "", err
	}
	return winUTF16String(info.Device[:])
}

func (windowsNativeAPI) physicalMonitorCount(handle uintptr) (uint32, error) {
	p, err := windowsProcedures()
	if err != nil {
		return 0, err
	}
	var count uint32
	ok, _, callErr := p.physicalNum.Call(handle, uintptr(unsafe.Pointer(&count)))
	return count, nativeBOOLResult("GetNumberOfPhysicalMonitorsFromHMONITOR", ok, callErr)
}

func (windowsNativeAPI) physicalMonitors(handle uintptr, count uint32) ([]nativePhysicalMonitor, error) {
	p, err := windowsProcedures()
	if err != nil {
		return nil, err
	}
	// 先限制数量，确保264*count在32/64位均无溢出且分配有界；不信任驱动提供的数量。
	if count == 0 || count > maxNativeDisplayItems {
		return nil, nativeInvalid("物理显示器缓冲区数量无效")
	}
	buffer := make([]winPhysicalMonitor, int(count))
	ok, _, callErr := p.physical.Call(handle, uintptr(count), uintptr(unsafe.Pointer(&buffer[0])))
	runtime.KeepAlive(buffer)
	result := make([]nativePhysicalMonitor, 0, len(buffer))
	resultErr := nativeBOOLResult("GetPhysicalMonitorsFromHMONITOR", ok, callErr)
	for _, item := range buffer {
		// BOOL失败也检查已填充的非零HANDLE，不能把部分获得的资源丢弃。成功返回的零句柄
		// 则保留给共享扫描校验；描述损坏也先交回句柄，再合并格式错误供清理路径处理。
		if ok == 0 && item.Handle == 0 {
			continue
		}
		name, nameErr := winUTF16String(item.Description[:])
		result = append(result, nativePhysicalMonitor{Handle: item.Handle, Description: name})
		resultErr = errors.Join(resultErr, nameErr)
	}
	return result, resultErr
}

func (windowsNativeAPI) capabilitiesLength(handle uintptr) (uint32, error) {
	p, err := windowsProcedures()
	if err != nil {
		return 0, err
	}
	var length uint32
	ok, _, callErr := p.capLength.Call(handle, uintptr(unsafe.Pointer(&length)))
	return length, nativeBOOLResult("GetCapabilitiesStringLength", ok, callErr)
}

func (windowsNativeAPI) capabilities(handle uintptr, length uint32) ([]byte, error) {
	p, err := windowsProcedures()
	if err != nil {
		return nil, err
	}
	if length < 2 || length > maxNativeCapabilities {
		return nil, nativeInvalid("MCCS能力缓冲区长度无效")
	}
	buffer := make([]byte, int(length))
	ok, _, callErr := p.capReply.Call(handle, uintptr(unsafe.Pointer(&buffer[0])), uintptr(length))
	runtime.KeepAlive(buffer)
	return buffer, nativeBOOLResult("CapabilitiesRequestAndCapabilitiesReply", ok, callErr)
}

func (windowsNativeAPI) getVCP(handle uintptr, code uint8) (uint32, uint32, error) {
	p, err := windowsProcedures()
	if err != nil {
		return 0, 0, err
	}
	var value uint32
	// 输入源是非连续特性，不请求没有端口范围含义的maximum；类型输出同样可选。
	ok, _, callErr := p.getVCP.Call(handle, uintptr(code), 0, uintptr(unsafe.Pointer(&value)), 0)
	return value, 0, nativeBOOLResult("GetVCPFeatureAndVCPFeatureReply", ok, callErr)
}

func (windowsNativeAPI) setVCP(handle uintptr, code uint8, value uint32) error {
	p, err := windowsProcedures()
	if err != nil {
		return err
	}
	ok, _, callErr := p.setVCP.Call(handle, uintptr(code), uintptr(value))
	return nativeBOOLResult("SetVCPFeature", ok, callErr)
}

func (windowsNativeAPI) destroyPhysicalMonitor(handle uintptr) error {
	p, err := windowsProcedures()
	if err != nil {
		return err
	}
	ok, _, callErr := p.destroy.Call(handle)
	return nativeBOOLResult("DestroyPhysicalMonitor", ok, callErr)
}

func winUTF16String(data []uint16) (string, error) {
	end := -1
	for i, value := range data {
		if value == 0 {
			end = i
			break
		}
	}
	if end < 0 {
		return "", nativeInvalid("Windows UTF-16字段缺少终止符")
	}
	for offset := 0; offset < end; offset++ {
		value := data[offset]
		if value >= 0xd800 && value <= 0xdbff {
			if offset+1 >= end || data[offset+1] < 0xdc00 || data[offset+1] > 0xdfff {
				return "", nativeInvalid("Windows UTF-16字段含损坏的代理对")
			}
			offset++
		} else if value >= 0xdc00 && value <= 0xdfff {
			return "", nativeInvalid("Windows UTF-16字段含孤立的低代理项")
		}
	}
	return string(utf16.Decode(data[:end])), nil
}
