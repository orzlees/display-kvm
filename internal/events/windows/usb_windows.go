//go:build windows

package windows

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/model"
)

var (
	pGetClassDevices   = setupapi.NewProc("SetupDiGetClassDevsW")
	pEnumInterfaces    = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	pInterfaceDetail   = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	pDeviceProperty    = setupapi.NewProc("SetupDiGetDeviceRegistryPropertyW")
	pDestroyDeviceList = setupapi.NewProc("SetupDiDestroyDeviceInfoList")
)

type deviceInterfaceData struct {
	Size     uint32
	Class    winGUID
	Flags    uint32
	Reserved uintptr
}

type deviceInfoData struct {
	Size     uint32
	Class    winGUID
	DevInst  uint32
	Reserved uintptr
}

func enumerateUSB(ctx context.Context) (devices []model.USBDevice, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, model.NewError(model.CodeCanceled, "Windows USB初扫已取消", err)
	}
	list, _, err := pGetClassDevices.Call(uintptr(unsafe.Pointer(&usbInterfaceGUID)), 0, 0, 0x12) // DIGCF_PRESENT | DIGCF_DEVICEINTERFACE
	if list == ^uintptr(0) {
		return nil, winError("SetupDiGetClassDevsW", err)
	}
	defer func() {
		if ok, _, err := pDestroyDeviceList.Call(list); ok == 0 {
			retErr = errors.Join(retErr, winError("SetupDiDestroyDeviceInfoList", err))
		}
		if retErr != nil {
			devices = nil
		}
	}()
	devices = make([]model.USBDevice, 0)
	for index := uint32(0); ; index++ {
		if err := ctx.Err(); err != nil {
			return nil, model.NewError(model.CodeCanceled, "Windows USB扫描已取消", err)
		}
		data := deviceInterfaceData{}
		data.Size = uint32(unsafe.Sizeof(data))
		ok, _, err := pEnumInterfaces.Call(list, 0, uintptr(unsafe.Pointer(&usbInterfaceGUID)), uintptr(index), uintptr(unsafe.Pointer(&data)))
		if ok == 0 {
			if errors.Is(err, syscall.Errno(259)) { // ERROR_NO_MORE_ITEMS，仅此状态表示枚举正常结束。
				return devices, nil
			}
			return nil, winError("SetupDiEnumDeviceInterfaces", err)
		}
		device, err := readUSBDevice(list, &data)
		if err != nil {
			// 枚举列表创建后设备仍可能被拔出。只把明确的PnP“设备已消失”
			// 视为正常竞争；权限、属性损坏和其他IO错误必须使扫描失败。
			if errors.Is(err, syscall.Errno(0xe000020b)) || errors.Is(err, syscall.Errno(1167)) || errors.Is(err, syscall.Errno(433)) {
				continue
			}
			return nil, err
		}
		devices = append(devices, device)
		if index == ^uint32(0) {
			return nil, model.NewError(model.CodeIO, "Windows USB接口枚举索引超出范围", nil)
		}
	}
}

func readUSBDevice(list uintptr, data *deviceInterfaceData) (model.USBDevice, error) {
	var required uint32
	ok, _, err := pInterfaceDetail.Call(list, uintptr(unsafe.Pointer(data)), 0, 0, uintptr(unsafe.Pointer(&required)), 0)
	if ok != 0 || !errors.Is(err, syscall.Errno(122)) {
		return model.USBDevice{}, winError("SetupDiGetDeviceInterfaceDetailW查询长度", err)
	}
	if required < 6 || required > maxNativeBuffer || required%2 != 0 {
		return model.USBDevice{}, model.NewError(model.CodeIO, "Windows USB接口详情缓冲区大小无效", nil)
	}
	buffer := make([]byte, int(required))
	// SP_DEVICE_INTERFACE_DETAIL_DATA_W的cbSize在64位为8、32位为6，
	// 但变长DevicePath在两种布局中都从偏移4开始，不能用cbSize作为路径偏移。
	cbSize := uint32(6)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		cbSize = 8
	}
	binary.LittleEndian.PutUint32(buffer, cbSize)
	info := deviceInfoData{}
	info.Size = uint32(unsafe.Sizeof(info))
	ok, _, err = pInterfaceDetail.Call(list, uintptr(unsafe.Pointer(data)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unsafe.Pointer(&required)), uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return model.USBDevice{}, winError("SetupDiGetDeviceInterfaceDetailW读取详情", err)
	}
	if required > uint32(len(buffer)) || required < 6 {
		return model.USBDevice{}, model.NewError(model.CodeIO, "Windows USB接口详情长度发生无效变化", nil)
	}
	path, err := readUTF16String(buffer[4:required])
	if err != nil || path == "" {
		return model.USBDevice{}, model.NewError(model.CodeIO, "Windows USB接口路径无效", err)
	}
	hardwareIDs, err := readHardwareIDs(list, &info)
	if err != nil {
		return model.USBDevice{}, err
	}
	for _, id := range hardwareIDs {
		if vid, pid, ok := parseHardwareID(id); ok {
			// SetupAPI接口符号路径和实例路径都不是可靠的硬件序列号来源。
			// 保留空Serial；路径只用于本轮同一系统内的存在状态对账。
			return model.USBDevice{VendorID: vid, ProductID: pid, Path: path}, nil
		}
	}
	return model.USBDevice{}, model.NewError(model.CodeIO, "Windows USB硬件属性没有有效VID/PID，不能构造可信设备身份", nil)
}

func readHardwareIDs(list uintptr, info *deviceInfoData) ([]string, error) {
	var size, registryType uint32
	ok, _, err := pDeviceProperty.Call(list, uintptr(unsafe.Pointer(info)), 1, uintptr(unsafe.Pointer(&registryType)), 0, 0, uintptr(unsafe.Pointer(&size))) // SPDRP_HARDWAREID
	if ok != 0 || !errors.Is(err, syscall.Errno(122)) {
		return nil, winError("SetupDiGetDeviceRegistryPropertyW查询长度", err)
	}
	if size < 4 || size > maxNativeBuffer || size%2 != 0 {
		return nil, model.NewError(model.CodeIO, "Windows USB硬件属性缓冲区大小无效", nil)
	}
	buffer := make([]byte, int(size))
	ok, _, err = pDeviceProperty.Call(list, uintptr(unsafe.Pointer(info)), 1, uintptr(unsafe.Pointer(&registryType)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unsafe.Pointer(&size)))
	if ok == 0 {
		return nil, winError("SetupDiGetDeviceRegistryPropertyW读取硬件ID", err)
	}
	if registryType != 7 || size < 4 || size > uint32(len(buffer)) || size%2 != 0 { // REG_MULTI_SZ
		return nil, model.NewError(model.CodeIO, "Windows USB硬件属性不是有效REG_MULTI_SZ", nil)
	}
	buffer = buffer[:size]
	if binary.LittleEndian.Uint16(buffer[len(buffer)-2:]) != 0 || binary.LittleEndian.Uint16(buffer[len(buffer)-4:]) != 0 {
		return nil, model.NewError(model.CodeIO, "Windows USB硬件ID列表缺少双终止符", nil)
	}
	characters := make([]uint16, len(buffer)/2)
	for i := range characters {
		characters[i] = binary.LittleEndian.Uint16(buffer[2*i:])
	}
	text := string(utf16.Decode(characters))
	var ids []string
	for _, id := range strings.Split(text, "\x00") {
		if id == "" {
			break
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func readUTF16String(data []byte) (string, error) {
	if len(data)%2 != 0 {
		return "", model.NewError(model.CodeIO, "Windows字符串长度未按UTF-16对齐", nil)
	}
	characters := make([]uint16, 0, len(data)/2)
	for i := 0; i < len(data); i += 2 {
		character := binary.LittleEndian.Uint16(data[i:])
		if character == 0 {
			return string(utf16.Decode(characters)), nil
		}
		characters = append(characters, character)
	}
	return "", model.NewError(model.CodeIO, "Windows字符串缺少终止符", nil)
}
