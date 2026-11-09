//go:build windows && amd64

package windows

import (
	"bytes"
	"errors"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/model"
)

// 双向数组长度断言让Win64交叉编译直接核对真实Go布局；任一字段偏移
// 或结构大小变化都会使编译失败，不能只凭版本常量掩盖ABI错位。
var (
	_ [64 - unsafe.Sizeof(nvapiI2CInfo{})]byte
	_ [unsafe.Sizeof(nvapiI2CInfo{}) - 64]byte
	_ [272 - unsafe.Sizeof(nvapiEDID{})]byte
	_ [unsafe.Sizeof(nvapiEDID{}) - 272]byte
	_ [16 - unsafe.Offsetof(nvapiI2CInfo{}.Register)]byte
	_ [unsafe.Offsetof(nvapiI2CInfo{}.Register) - 16]byte
	_ [24 - unsafe.Offsetof(nvapiI2CInfo{}.RegisterSize)]byte
	_ [unsafe.Offsetof(nvapiI2CInfo{}.RegisterSize) - 24]byte
	_ [32 - unsafe.Offsetof(nvapiI2CInfo{}.Data)]byte
	_ [unsafe.Offsetof(nvapiI2CInfo{}.Data) - 32]byte
	_ [40 - unsafe.Offsetof(nvapiI2CInfo{}.DataSize)]byte
	_ [unsafe.Offsetof(nvapiI2CInfo{}.DataSize) - 40]byte
	_ [56 - unsafe.Offsetof(nvapiI2CInfo{}.IsPortIDSet)]byte
	_ [unsafe.Offsetof(nvapiI2CInfo{}.IsPortIDSet) - 56]byte
	_ [260 - unsafe.Offsetof(nvapiEDID{}.Size)]byte
	_ [unsafe.Offsetof(nvapiEDID{}.Size) - 260]byte
	_ [264 - unsafe.Offsetof(nvapiEDID{}.ID)]byte
	_ [unsafe.Offsetof(nvapiEDID{}.ID) - 264]byte
	_ [268 - unsafe.Offsetof(nvapiEDID{}.Offset)]byte
	_ [unsafe.Offsetof(nvapiEDID{}.Offset) - 268]byte
)

// DISPLAY_DEVICEW来自Win32公开定义；只枚举显卡层，不能把monitor实例的
// DeviceID当PCI厂商。相同GDI名称重复、镜像驱动和不完整PCI标识都拒绝。
type nvapiDisplayDevice struct {
	Size        uint32
	Name        [32]uint16
	Description [128]uint16
	StateFlags  uint32
	DeviceID    [128]uint16
	DeviceKey   [128]uint16
}

func (windowsNativeAPI) displayVendor(gdi string) (uint32, error) {
	directory, err := windowsSystemDirectory()
	if err != nil {
		return 0, err
	}
	proc := syscall.NewLazyDLL(filepath.Join(directory, "user32.dll")).NewProc("EnumDisplayDevicesW")
	if err := proc.Find(); err != nil {
		return 0, nativeError("查找EnumDisplayDevicesW", err)
	}
	var vendor uint32
	matched := false
	for index := uint32(0); ; index++ {
		var device nvapiDisplayDevice
		device.Size = uint32(unsafe.Sizeof(device))
		result, _, _ := proc.Call(0, uintptr(index), uintptr(unsafe.Pointer(&device)), 0)
		runtime.KeepAlive(device)
		// Win32规定连续枚举直到FALSE；不把最后一次的遗留GetLastError当错误。
		if result == 0 {
			break
		}
		if index >= maxNativeDisplayItems {
			return 0, nativeInvalid("EnumDisplayDevicesW显卡数量超出安全上限")
		}
		name, err := winUTF16String(device.Name[:])
		if err != nil {
			return 0, err
		}
		if normalizeNativeName(name) != normalizeNativeName(gdi) {
			continue
		}
		if matched {
			return 0, nativeAmbiguous("活动GDI源对应多个Windows显卡")
		}
		if device.StateFlags&1 == 0 || device.StateFlags&8 != 0 {
			return 0, lgUnsupported("活动GDI源不是独立且连接桌面的显卡")
		}
		id, err := winUTF16String(device.DeviceID[:])
		if err != nil {
			return 0, err
		}
		id = strings.ToUpper(id)
		if !strings.HasPrefix(id, `PCI\VEN_`) || len(id) < 12 || (len(id) > 12 && id[12] != '&') {
			return 0, lgUnsupported("活动GDI源没有可核验的PCI厂商标识")
		}
		value, err := strconv.ParseUint(id[8:12], 16, 16)
		if err != nil {
			return 0, model.NewError(model.CodeUnsupportedOperation, "活动GDI源PCI厂商标识无效", err)
		}
		vendor, matched = uint32(value), true
	}
	if !matched {
		return 0, lgUnsupported("活动GDI源没有对应Windows显卡")
	}
	return vendor, nil
}

type windowsNVAPISession struct {
	dll                                            *syscall.DLL
	enumDisplay, displayNameProc, physicalGPUsProc uintptr
	outputIDProc, edidProc, writeProc, unloadProc  uintptr
	initialized, closed                            bool
}

func (windowsNativeAPI) openNVAPI() (nvapiSession, error) { return openPlatformNVAPI() }

func openPlatformNVAPI() (result nvapiSession, err error) {
	if err := validateNVAPIABI(); err != nil {
		return nil, err
	}
	directory, err := windowsSystemDirectory()
	if err != nil {
		return nil, err
	}
	// 与既有ADL相同，只按真实系统目录绝对路径加载用户已安装的驱动。
	// 不搜索当前目录/PATH，不附带SDK/DLL，也不尝试其他架构的nvapi.dll。
	dll, err := syscall.LoadDLL(filepath.Join(directory, "nvapi64.dll"))
	if err != nil {
		return nil, model.NewError(model.CodeUnsupportedOperation, "已安装的NVIDIA nvapi64.dll不可用："+err.Error(), err)
	}
	session := &windowsNVAPISession{dll: dll}
	defer func() {
		if err != nil {
			err = errors.Join(err, session.close())
		}
	}()
	query, err := dll.FindProc("nvapi_QueryInterface")
	if err != nil {
		return nil, model.NewError(model.CodeUnsupportedOperation, "NVIDIA驱动缺少nvapi_QueryInterface："+err.Error(), err)
	}
	var initialize uintptr
	entries := []struct {
		name string
		id   uint32
		dest *uintptr
	}{
		{"NvAPI_Initialize", 0x0150e828, &initialize},
		{"NvAPI_Unload", 0xd22bdd7e, &session.unloadProc},
		{"NvAPI_EnumNvidiaDisplayHandle", 0x9abdd40d, &session.enumDisplay},
		{"NvAPI_GetAssociatedNvidiaDisplayName", 0x22a78b05, &session.displayNameProc},
		{"NvAPI_GetPhysicalGPUsFromDisplay", 0x34ef9506, &session.physicalGPUsProc},
		{"NvAPI_GetAssociatedDisplayOutputId", 0xd995937e, &session.outputIDProc},
		{"NvAPI_GPU_GetEDID", 0x37d32e69, &session.edidProc},
		{"NvAPI_I2CWrite", 0xe812eb07, &session.writeProc},
	}
	for _, entry := range entries {
		address, _, _ := query.Call(uintptr(entry.id))
		if address == 0 {
			return nil, lgUnsupported("NVIDIA驱动缺少" + entry.name)
		}
		*entry.dest = address
	}
	// NVAPI公开入口为cdecl；Windows x64统一采用其四寄存器ABI，Go的
	// SyscallN可直接调用QueryInterface给出的函数地址。本文件严格限amd64，
	// 不把该调用方式外推到具有栈清理差异的32位Windows。
	status, _, _ := syscall.SyscallN(initialize)
	if err := nvapiStatus("NvAPI_Initialize", status); err != nil {
		return nil, err
	}
	session.initialized = true
	return session, nil
}

func (s *windowsNVAPISession) displays() ([]uintptr, error) {
	var result []uintptr
	for index := uint32(0); ; index++ {
		var handle uintptr
		status, _, _ := syscall.SyscallN(s.enumDisplay, uintptr(index), uintptr(unsafe.Pointer(&handle)))
		if int32(uint32(status)) == -7 { // NVAPI_END_ENUMERATION是唯一正常结束状态。
			return result, nil
		}
		if err := nvapiStatus("NvAPI_EnumNvidiaDisplayHandle", status); err != nil {
			return nil, err
		}
		if index >= maxNativeDisplayItems {
			return nil, nativeInvalid("NVAPI显示源数量超出安全上限")
		}
		result = append(result, handle)
	}
}

func (s *windowsNVAPISession) displayName(display uintptr) (string, error) {
	var name [64]byte // NvAPI_ShortString固定64字节，包含NUL终止符。
	status, _, _ := syscall.SyscallN(s.displayNameProc, display, uintptr(unsafe.Pointer(&name[0])))
	runtime.KeepAlive(name)
	if err := nvapiStatus("NvAPI_GetAssociatedNvidiaDisplayName", status); err != nil {
		return "", err
	}
	end := bytes.IndexByte(name[:], 0)
	if end <= 0 {
		return "", nativeInvalid("NVAPI显示源名称为空或缺少NUL终止符")
	}
	for _, value := range name[:end] {
		if value < 0x20 || value > 0x7e {
			return "", nativeInvalid("NVAPI GDI名称不是有效ASCII")
		}
	}
	return normalizeNativeName(string(name[:end])), nil
}

func (s *windowsNVAPISession) physicalGPUs(display uintptr) ([]uintptr, error) {
	var handles [64]uintptr // NVAPI_MAX_PHYSICAL_GPUS，不能按预期单GPU缩小驱动输出缓冲区。
	var count uint32
	status, _, _ := syscall.SyscallN(s.physicalGPUsProc, display, uintptr(unsafe.Pointer(&handles[0])), uintptr(unsafe.Pointer(&count)))
	runtime.KeepAlive(handles)
	if err := nvapiStatus("NvAPI_GetPhysicalGPUsFromDisplay", status); err != nil {
		return nil, err
	}
	if count > uint32(len(handles)) {
		return nil, nativeInvalid("NVAPI物理GPU数量超过官方缓冲区上限")
	}
	return append([]uintptr(nil), handles[:count]...), nil
}

func (s *windowsNVAPISession) outputID(display uintptr) (uint32, error) {
	var output uint32
	status, _, _ := syscall.SyscallN(s.outputIDProc, display, uintptr(unsafe.Pointer(&output)))
	return output, nvapiStatus("NvAPI_GetAssociatedDisplayOutputId", status)
}

func (s *windowsNVAPISession) edid(gpu uintptr, output uint32) ([]byte, error) {
	var result []byte
	var size, id uint32
	for offset := uint32(0); ; offset += 256 {
		page := nvapiEDID{Version: uint32(unsafe.Sizeof(nvapiEDID{})) | 3<<16, Offset: offset}
		status, _, _ := syscall.SyscallN(s.edidProc, gpu, uintptr(output), uintptr(unsafe.Pointer(&page)))
		runtime.KeepAlive(page)
		if err := nvapiStatus("NvAPI_GPU_GetEDID", status); err != nil {
			return nil, err
		}
		if page.Version != uint32(unsafe.Sizeof(page))|3<<16 || page.Offset != offset || page.Size < 128 || page.Size > 1024 || page.Size%128 != 0 {
			return nil, lgUnsupported("NVAPI EDID版本、分页偏移或完整长度无法核验")
		}
		if offset == 0 {
			size, id = page.Size, page.ID
		} else if page.Size != size || page.ID != id {
			return nil, model.NewError(model.CodeUnreachable, "NVAPI EDID在分页读取期间已变化", nil)
		}
		// offset以字节计，每页256字节；必须检查跨页edidId和总长度一致。
		// Windows/ADL现有身份链仅接受最多256字节，更多有效页仍会在共享
		// validatedLGEDID处保守拒绝，不能截取基础块冒充完整EDID匹配。
		count := size - offset
		if count > uint32(len(page.Data)) {
			count = uint32(len(page.Data))
		}
		result = append(result, page.Data[:count]...)
		if uint32(len(result)) == size {
			return result, nil
		}
	}
}

func (s *windowsNVAPISession) writeI2C(gpu uintptr, output uint32, address byte, register, data []byte) error {
	if gpu == 0 || output == 0 || output&(output-1) != 0 || address != 0x6e || len(register) != 1 || register[0] != 0x50 || len(data) != 6 {
		return lgUnsupported("NVIDIA入口仅接受唯一物理输出与固定LG输入报文")
	}
	expected, err := lgInputPacket(uint16(data[3])<<8 | uint16(data[4]))
	if err != nil || !bytes.Equal(data, expected[2:]) {
		return lgUnsupported("NVIDIA入口拒绝非固定格式或校验错误的LG输入报文")
	}
	info := nvapiI2CInfo{
		Version:     uint32(unsafe.Sizeof(nvapiI2CInfo{})) | 3<<16,
		DisplayMask: output, IsDDCPort: 1, DeviceAddress: address,
		Register: &register[0], RegisterSize: uint32(len(register)),
		Data: &data[0], DataSize: uint32(len(data)), Speed: 0xffff,
	}
	// i2cSpeed已废弃，必须为0xffff；SpeedKhz=0使用驱动默认速度。
	// portId未设置，IsPortIDSet保持0；显示目标只由已核验的单bit mask指定。
	// 指针仅在同步调用内有效，必须等驱动返回后才释放会话或允许GC回收报文。
	status, _, _ := syscall.SyscallN(s.writeProc, gpu, uintptr(unsafe.Pointer(&info)))
	runtime.KeepAlive(info)
	runtime.KeepAlive(register)
	runtime.KeepAlive(data)
	return nvapiStatus("NvAPI_I2CWrite(LG输入提交)", status)
}

func (s *windowsNVAPISession) close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.initialized {
		// Initialize会增加驱动引用计数，只与成功的初始化配对一次Unload。
		// 调用由LG进程锁串行包围，关闭必须发生在所有同步枚举/写入返回以后。
		status, _, _ := syscall.SyscallN(s.unloadProc)
		err = nvapiStatus("NvAPI_Unload", status)
	}
	return errors.Join(err, nativeError("FreeLibrary(nvapi64.dll)", s.dll.Release()))
}
