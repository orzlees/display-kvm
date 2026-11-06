//go:build windows

package windows

import (
	"testing"
	"unsafe"
)

// 交叉编译不能执行Windows测试；双向数组长度在编译时锁定关键大小及偏移，
// 防止仅生成测试程序却漏掉模式union或物理句柄字宽的布局回归。
var (
	_ [unsafe.Sizeof(winPathInfo{}) - 72]byte
	_ [72 - unsafe.Sizeof(winPathInfo{})]byte
	_ [unsafe.Sizeof(winModeInfo{}) - 64]byte
	_ [64 - unsafe.Sizeof(winModeInfo{})]byte
	_ [unsafe.Offsetof(winModeInfo{}.Data) - 16]byte
	_ [16 - unsafe.Offsetof(winModeInfo{}.Data)]byte
	_ [unsafe.Sizeof(winDeviceInfoHeader{}) - 20]byte
	_ [20 - unsafe.Sizeof(winDeviceInfoHeader{})]byte
	_ [unsafe.Sizeof(winSourceDeviceName{}) - 84]byte
	_ [84 - unsafe.Sizeof(winSourceDeviceName{})]byte
	_ [unsafe.Sizeof(winTargetDeviceName{}) - 420]byte
	_ [420 - unsafe.Sizeof(winTargetDeviceName{})]byte
	_ [unsafe.Offsetof(winTargetDeviceName{}.DevicePath) - 164]byte
	_ [164 - unsafe.Offsetof(winTargetDeviceName{}.DevicePath)]byte
	_ [unsafe.Sizeof(winMonitorInfo{}) - 104]byte
	_ [104 - unsafe.Sizeof(winMonitorInfo{})]byte
	_ [unsafe.Sizeof(winPhysicalMonitor{}) - unsafe.Sizeof(uintptr(0)) - 256]byte
	_ [unsafe.Sizeof(uintptr(0)) + 256 - unsafe.Sizeof(winPhysicalMonitor{})]byte
)

func TestNativeWindowsABIAndConstruction(t *testing.T) {
	// 这里只检查声明布局和构造接线，不加载DLL、不枚举或操作实际显示器。
	if _, ok := NewNative().transport.(*nativeTransport); !ok {
		t.Fatal("Windows原生构造未接入真实适配器")
	}
	if _, ok := NewNative().transport.(*nativeTransport).api.(windowsNativeAPI); !ok {
		t.Fatal("Windows原生适配未接入真实API边界")
	}
	checks := []struct {
		name      string
		got, want uintptr
	}{
		{"LUID", unsafe.Sizeof(nativeLUID{}), 8},
		{"DISPLAYCONFIG_PATH_SOURCE_INFO", unsafe.Sizeof(winPathSourceInfo{}), 20},
		{"DISPLAYCONFIG_PATH_TARGET_INFO", unsafe.Sizeof(winPathTargetInfo{}), 48},
		{"DISPLAYCONFIG_PATH_INFO", unsafe.Sizeof(winPathInfo{}), 72},
		{"DISPLAYCONFIG_PATH_INFO.source", unsafe.Offsetof(winPathInfo{}.Source), 0},
		{"DISPLAYCONFIG_PATH_INFO.target", unsafe.Offsetof(winPathInfo{}.Target), 20},
		{"DISPLAYCONFIG_PATH_INFO.flags", unsafe.Offsetof(winPathInfo{}.Flags), 68},
		{"DISPLAYCONFIG_MODE_INFO", unsafe.Sizeof(winModeInfo{}), 64},
		{"DISPLAYCONFIG_MODE_INFO.union", unsafe.Offsetof(winModeInfo{}.Data), 16},
		{"DISPLAYCONFIG_DEVICE_INFO_HEADER", unsafe.Sizeof(winDeviceInfoHeader{}), 20},
		{"DISPLAYCONFIG_DEVICE_INFO_HEADER.type", unsafe.Offsetof(winDeviceInfoHeader{}.Type), 0},
		{"DISPLAYCONFIG_DEVICE_INFO_HEADER.size", unsafe.Offsetof(winDeviceInfoHeader{}.Size), 4},
		{"DISPLAYCONFIG_DEVICE_INFO_HEADER.adapter", unsafe.Offsetof(winDeviceInfoHeader{}.Adapter), 8},
		{"DISPLAYCONFIG_DEVICE_INFO_HEADER.id", unsafe.Offsetof(winDeviceInfoHeader{}.ID), 16},
		{"DISPLAYCONFIG_SOURCE_DEVICE_NAME", unsafe.Sizeof(winSourceDeviceName{}), 84},
		{"DISPLAYCONFIG_SOURCE_DEVICE_NAME.gdi", unsafe.Offsetof(winSourceDeviceName{}.GDIName), 20},
		{"DISPLAYCONFIG_TARGET_DEVICE_NAME", unsafe.Sizeof(winTargetDeviceName{}), 420},
		{"DISPLAYCONFIG_TARGET_DEVICE_NAME.friendly", unsafe.Offsetof(winTargetDeviceName{}.FriendlyName), 36},
		{"DISPLAYCONFIG_TARGET_DEVICE_NAME.path", unsafe.Offsetof(winTargetDeviceName{}.DevicePath), 164},
		{"MONITORINFOEXW", unsafe.Sizeof(winMonitorInfo{}), 104},
		{"MONITORINFOEXW.device", unsafe.Offsetof(winMonitorInfo{}.Device), 40},
		{"PHYSICAL_MONITOR", unsafe.Sizeof(winPhysicalMonitor{}), unsafe.Sizeof(uintptr(0)) + 256},
		{"PHYSICAL_MONITOR.description", unsafe.Offsetof(winPhysicalMonitor{}.Description), unsafe.Sizeof(uintptr(0))},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s布局=%d，期望%d", check.name, check.got, check.want)
		}
	}
}

func TestNativeWindowsUTF16Boundaries(t *testing.T) {
	for _, data := range [][]uint16{{'a'}, {0xd800, 0}, {0xdc00, 0}} {
		if _, err := winUTF16String(data); err == nil {
			t.Fatalf("损坏UTF-16字段未拒绝：%v", data)
		}
	}
	value, err := winUTF16String([]uint16{'a', 0xd83d, 0xde00, 0})
	if err != nil || value != "a😀" {
		t.Fatalf("有效UTF-16解码错误：%q %v", value, err)
	}
}
