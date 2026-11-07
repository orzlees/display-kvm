//go:build windows

package windows

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// 标准DDC、SetupAPI和ADL复用同一安全DLL定位方式；只从系统目录加载已安装组件。
func windowsSystemDirectory() (string, error) {
	kernel := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel.NewProc("GetSystemDirectoryW")
	if err := proc.Find(); err != nil {
		return "", nativeError("查找GetSystemDirectoryW", err)
	}
	buffer := make([]uint16, 32768)
	length, _, callErr := proc.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	runtime.KeepAlive(buffer)
	if length == 0 {
		return "", nativeBOOLResult("GetSystemDirectoryW", 0, callErr)
	}
	if length >= uintptr(len(buffer)) || buffer[length] != 0 {
		return "", nativeInvalid("GetSystemDirectoryW返回不可信的目录长度")
	}
	directory, err := winUTF16String(buffer[:length+1])
	if err != nil || !filepath.IsAbs(directory) || strings.Contains(directory, "\x00") || len(utf16.Encode([]rune(directory))) != int(length) {
		return "", errors.Join(nativeInvalid("Windows系统目录无效"), err)
	}
	return directory, nil
}

type winMonitorGUID struct {
	Data1        uint32
	Data2, Data3 uint16
	Data4        [8]byte
}

// GUID_DEVINTERFACE_MONITOR标识显示器实例接口，不能改成显卡类GUID或显卡PNP字符串。
var winMonitorInterfaceGUID = winMonitorGUID{0xe6f07b5f, 0xee97, 0x4a90, [8]byte{0xb0, 0x76, 0x33, 0xf5, 0x7b, 0xf4, 0xea, 0xa7}}

type winDeviceInterfaceData struct {
	Size      uint32
	ClassGUID winMonitorGUID
	Flags     uint32
	Reserved  uintptr
}

type winDeviceInstanceData struct {
	Size           uint32
	ClassGUID      winMonitorGUID
	DeviceInstance uint32
	Reserved       uintptr
}

type winEDIDProcedures struct {
	getClass, enumInterface, interfaceDetail, openKey, destroyList *syscall.LazyProc
	queryValue, closeKey                                           *syscall.LazyProc
}

var (
	winEDIDOnce     sync.Once
	winEDIDBindings *winEDIDProcedures
	winEDIDErr      error
)

func windowsEDIDProcedures() (*winEDIDProcedures, error) {
	winEDIDOnce.Do(func() {
		directory, err := windowsSystemDirectory()
		if err != nil {
			winEDIDErr = err
			return
		}
		setup, registry := syscall.NewLazyDLL(filepath.Join(directory, "setupapi.dll")), syscall.NewLazyDLL(filepath.Join(directory, "advapi32.dll"))
		p := &winEDIDProcedures{
			getClass: setup.NewProc("SetupDiGetClassDevsW"), enumInterface: setup.NewProc("SetupDiEnumDeviceInterfaces"),
			interfaceDetail: setup.NewProc("SetupDiGetDeviceInterfaceDetailW"), openKey: setup.NewProc("SetupDiOpenDevRegKey"),
			destroyList: setup.NewProc("SetupDiDestroyDeviceInfoList"), queryValue: registry.NewProc("RegQueryValueExW"), closeKey: registry.NewProc("RegCloseKey"),
		}
		for _, proc := range []*syscall.LazyProc{p.getClass, p.enumInterface, p.interfaceDetail, p.openKey, p.destroyList, p.queryValue, p.closeKey} {
			if err := proc.Find(); err != nil {
				winEDIDErr = nativeError("查找显示实例EDID入口 "+proc.Name, err)
				return
			}
		}
		winEDIDBindings = p
	})
	return winEDIDBindings, winEDIDErr
}

func (windowsNativeAPI) openADL() (adlSession, error) { return openPlatformADL() }

// 通过与QueryDisplayConfig完全相同的monitorDevicePath查找SetupAPI接口，再取得其
// monitor实例的DIREG_DEV键；不拼接注册表路径、不解析名称猜实例、不使用显卡PNP。
// EDID值是实例缓存，必须与ADL当前输出的全部有效块一致才有写入资格。
func (windowsNativeAPI) monitorEDID(path string) (data []byte, err error) {
	p, err := windowsEDIDProcedures()
	if err != nil {
		return nil, err
	}
	set, _, callErr := p.getClass.Call(uintptr(unsafe.Pointer(&winMonitorInterfaceGUID)), 0, 0, 0x12) // PRESENT | DEVICEINTERFACE
	if set == ^uintptr(0) {
		return nil, nativeBOOLResult("SetupDiGetClassDevsW(MONITOR)", 0, callErr)
	}
	defer func() {
		ok, _, cause := p.destroyList.Call(set)
		err = errors.Join(err, nativeBOOLResult("SetupDiDestroyDeviceInfoList", ok, cause))
	}()
	var matched winDeviceInstanceData
	matches := 0
	for index := uint32(0); ; index++ {
		var iface winDeviceInterfaceData
		iface.Size = uint32(unsafe.Sizeof(iface))
		ok, _, cause := p.enumInterface.Call(set, 0, uintptr(unsafe.Pointer(&winMonitorInterfaceGUID)), uintptr(index), uintptr(unsafe.Pointer(&iface)))
		if ok == 0 {
			if errors.Is(cause, syscall.Errno(259)) {
				break
			} // ERROR_NO_MORE_ITEMS
			return nil, nativeBOOLResult("SetupDiEnumDeviceInterfaces", ok, cause)
		}
		if index >= maxNativeDisplayItems {
			return nil, nativeInvalid("SetupAPI显示接口数量超出安全上限")
		}
		name, instance, detailErr := p.monitorInterfaceDetail(set, &iface)
		if detailErr != nil {
			return nil, detailErr
		}
		if normalizeNativeName(name) == normalizeNativeName(path) {
			matched = instance
			matches++
		}
	}
	if matches == 0 {
		return nil, lgUnsupported("monitorDevicePath没有对应的活动Windows显示实例")
	}
	if matches != 1 {
		return nil, nativeAmbiguous("monitorDevicePath对应多个Windows显示实例")
	}
	key, _, callErr := p.openKey.Call(set, uintptr(unsafe.Pointer(&matched)), 1, 0, 1, 1) // GLOBAL, DIREG_DEV, KEY_QUERY_VALUE
	if key == ^uintptr(0) {
		return nil, nativeBOOLResult("SetupDiOpenDevRegKey(DIREG_DEV)", 0, callErr)
	}
	defer func() {
		status, _, _ := p.closeKey.Call(key)
		if uint32(status) != 0 {
			err = errors.Join(err, nativeError("RegCloseKey(EDID)", syscall.Errno(uint32(status))))
		}
	}()
	return p.readMonitorEDID(key)
}

func (p *winEDIDProcedures) monitorInterfaceDetail(set uintptr, iface *winDeviceInterfaceData) (string, winDeviceInstanceData, error) {
	var required uint32
	ok, _, cause := p.interfaceDetail.Call(set, uintptr(unsafe.Pointer(iface)), 0, 0, uintptr(unsafe.Pointer(&required)), 0)
	if ok != 0 || !errors.Is(cause, syscall.Errno(122)) {
		return "", winDeviceInstanceData{}, nativeInvalid("SetupAPI接口长度查询未返回预期缓冲区状态")
	}
	if required < 8 || required > 4+32768*2 || required%2 != 0 {
		return "", winDeviceInstanceData{}, nativeInvalid("SetupAPI显示接口长度无效")
	}
	buffer := make([]byte, int(required))
	// Unicode DETAIL_DATA的cbSize在64位为8、32位为6，但DevicePath偏移始终为4。
	// 不能按Go含指针结构的sizeof推导路径起点，否则会截掉首字符并失去精确路径匹配。
	detailSize := uint32(6)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		detailSize = 8
	}
	*(*uint32)(unsafe.Pointer(&buffer[0])) = detailSize
	var instance winDeviceInstanceData
	instance.Size = uint32(unsafe.Sizeof(instance))
	capacity := required
	ok, _, cause = p.interfaceDetail.Call(set, uintptr(unsafe.Pointer(iface)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(capacity), uintptr(unsafe.Pointer(&required)), uintptr(unsafe.Pointer(&instance)))
	runtime.KeepAlive(buffer)
	if err := nativeBOOLResult("SetupDiGetDeviceInterfaceDetailW", ok, cause); err != nil {
		return "", instance, err
	}
	if required > capacity || required < 8 || required%2 != 0 {
		return "", instance, nativeInvalid("SetupAPI显示接口长度在读取期间改变")
	}
	name, err := winUTF16String(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[4])), int(required-4)/2))
	return name, instance, err
}

func (p *winEDIDProcedures) readMonitorEDID(key uintptr) ([]byte, error) {
	name := []uint16{'E', 'D', 'I', 'D', 0}
	var kind, length uint32
	status, _, _ := p.queryValue.Call(key, uintptr(unsafe.Pointer(&name[0])), 0, uintptr(unsafe.Pointer(&kind)), 0, uintptr(unsafe.Pointer(&length)))
	if uint32(status) != 0 {
		return nil, nativeError("RegQueryValueExW(EDID长度)", syscall.Errno(uint32(status)))
	}
	if kind != 3 || length < 128 || length > 256 {
		return nil, lgUnsupported("Windows实例EDID类型、完整长度或扩展块范围不受支持")
	}
	buffer := make([]byte, int(length))
	capacity := length
	status, _, _ = p.queryValue.Call(key, uintptr(unsafe.Pointer(&name[0])), 0, uintptr(unsafe.Pointer(&kind)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&length)))
	runtime.KeepAlive(buffer)
	if uint32(status) != 0 {
		return nil, nativeError("RegQueryValueExW(EDID内容)", syscall.Errno(uint32(status)))
	}
	if kind != 3 || length != capacity {
		return nil, lgUnsupported("Windows实例EDID在读取期间发生变化")
	}
	return validatedLGEDID(buffer)
}
