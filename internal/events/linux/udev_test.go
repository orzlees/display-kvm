package linux

import (
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

func TestUdevVersionReadinessCompatibility(t *testing.T) {
	for _, v := range []string{"257\n", "258\n"} {
		if err := checkUdevVersion([]byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"256\n", "259\n", "garbage"} {
		if err := checkUdevVersion([]byte(v)); model.CodeOf(err) != model.CodeUnsupportedVersion {
			t.Fatalf("未核查版本被默认支持: %q %v", v, err)
		}
	}
}

func TestUSBDatabaseAndDRMSeparation(t *testing.T) {
	devices, err := parseUSBDatabase([]byte(usbDatabase + "P: /devices/drm/card0\nE: SUBSYSTEM=drm\nE: DEVPATH=/devices/drm/card0\n\n"))
	if err != nil || len(devices) != 1 || devices["/devices/usb1/1-2"].VendorID != 0x1234 {
		t.Fatalf("数据库解析错误: %+v %v", devices, err)
	}
	for _, fields := range []map[string]string{
		{"ACTION": "change", "SUBSYSTEM": "drm", "DEVPATH": "/devices/drm/card0"},
		{"ACTION": "add", "SUBSYSTEM": "usb", "DEVTYPE": "usb_interface", "DEVPATH": "/devices/usb1/1-2:1.0"},
	} {
		r, err := parseUdevRecord(fields)
		if err != nil || r != nil {
			t.Fatalf("显示变化/USB接口被当成USB设备: %+v %v", r, err)
		}
	}
	if _, err := parseUdevRecord(map[string]string{"ACTION": "add", "SUBSYSTEM": "usb", "DEVTYPE": "usb_device", "DEVPATH": "/devices/x", "PRODUCT": "wrong"}); err == nil {
		t.Fatal("缺失身份被伪造为零")
	}
}

func TestConfirmedEmptyUSBDistinctFromUnknown(t *testing.T) {
	devices, err := parseUSBDatabase(nil)
	if err != nil || devices == nil || len(devices) != 0 {
		t.Fatalf("已确认空列表语义错误: %+v %v", devices, err)
	}
}

func TestUdevControlSocketMustHaveLiveKernelListener(t *testing.T) {
	live := []byte("Num RefCount Protocol Flags Type St Inode Path\n000: 00000002 00000000 00010000 0005 01 123 /run/udev/control\n")
	if !hasUdevListener(live) {
		t.Fatal("活跃的udev顺序包监听者未识别")
	}
	for _, data := range [][]byte{nil, []byte("000: 00000002 00000000 00000000 0005 01 123 /run/udev/control\n"), []byte("000: 00000002 00000000 00010000 0005 01 123 /other/socket\n")} {
		if hasUdevListener(data) {
			t.Fatal("残留路径或其他socket被当成udev服务")
		}
	}
}
