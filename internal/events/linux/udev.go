package linux

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/orzlee/displayKvm/internal/model"
)

type udevRecord struct {
	kind       model.EventKind
	device     model.USBDevice
	identified bool
}

func checkUdevVersion(data []byte) error {
	v := strings.TrimSpace(string(data))
	if v != "257" && v != "258" {
		return model.NewError(model.CodeUnsupportedVersion, "udevadm就绪协议仅核查systemd 257/258", nil)
	}
	return nil
}

func udevInfrastructure() error {
	info, err := os.Stat("/run/udev/control")
	if err != nil {
		return commandError(context.Background(), err, "无法确认udev服务控制socket")
	}
	if info.Mode()&os.ModeSocket == 0 {
		return model.NewError(model.CodeDaemonUnavailable, "udev控制路径不是服务socket", nil)
	}
	file, err := os.Open("/proc/net/unix")
	if err != nil {
		return commandError(context.Background(), err, "无法核查udev控制socket是否仍有监听者")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	closeErr := file.Close()
	if readErr != nil {
		return model.NewError(model.CodeIO, "读取udev服务监听状态失败", readErr)
	}
	if closeErr != nil {
		return model.NewError(model.CodeIO, "关闭udev服务状态句柄失败", closeErr)
	}
	if len(data) > 1<<20 {
		return model.NewError(model.CodeMessageTooLarge, "系统Unix socket状态超过容量限制", nil)
	}
	if !hasUdevListener(data) {
		return model.NewError(model.CodeDaemonUnavailable, "udev控制socket没有当前网络命名空间中的活跃监听者", nil)
	}
	// v257/v258在基础设施缺席时可能使用MONITOR_GROUP_NONE仍打印UDEV标记。
	// 保守要求控制socket及与PID1相同的网络命名空间，避免把无通知容器当成可用。
	self, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		return commandError(context.Background(), err, "无法核查当前网络命名空间")
	}
	init, err := os.Stat("/proc/1/ns/net")
	if err != nil {
		return commandError(context.Background(), err, "无法核查系统网络命名空间")
	}
	if !os.SameFile(self, init) {
		return model.NewError(model.CodeUnsupportedOperation, "当前网络命名空间不同于PID1，无法确认udev通知可达", nil)
	}
	return nil
}

func hasUdevListener(data []byte) bool {
	// 文件系统里可能遗留已经没有服务进程持有的socket。读取内核监听表而非向
	// root控制接口发送ping，可以识别这种缺席，同时不要求提升普通用户权限。
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 8 || fields[7] != "/run/udev/control" || fields[4] != "0005" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err == nil && flags&0x10000 != 0 {
			return true
		}
	}
	return false
}

func parseUdevRecord(fields map[string]string) (*udevRecord, error) {
	if fields["SUBSYSTEM"] != "usb" || fields["DEVTYPE"] != "usb_device" {
		return nil, nil
	}
	var kind model.EventKind
	switch fields["ACTION"] {
	case "add":
		kind = model.EventUSBAdded
	case "remove":
		kind = model.EventUSBRemoved
	default:
		return nil, nil
	}
	p := fields["DEVPATH"]
	if !strings.HasPrefix(p, "/devices/") || path.Clean(p) != p || len(p) > 1024 {
		return nil, model.NewError(model.CodeUnsupportedProtocol, "udev设备路径缺失或无效", nil)
	}
	r := &udevRecord{kind: kind, device: model.USBDevice{Path: p, Serial: fields["ID_SERIAL_SHORT"]}}
	if len(r.device.Serial) > 1024 {
		return nil, model.NewError(model.CodeMessageTooLarge, "USB序列号超过容量限制", nil)
	}
	vendor, product := fields["ID_VENDOR_ID"], fields["ID_MODEL_ID"]
	if vendor == "" || product == "" {
		if raw := fields["PRODUCT"]; raw != "" {
			parts := strings.Split(raw, "/")
			if len(parts) != 3 {
				return nil, model.NewError(model.CodeUnsupportedProtocol, "USB PRODUCT属性格式无效", nil)
			}
			vendor, product = parts[0], parts[1]
		}
	}
	if vendor == "" && product == "" && kind == model.EventUSBRemoved {
		return r, nil
	}
	v, e1 := strconv.ParseUint(vendor, 16, 16)
	pn, e2 := strconv.ParseUint(product, 16, 16)
	if e1 != nil || e2 != nil {
		return nil, model.NewError(model.CodeUnsupportedProtocol, "USB设备缺少有效VID/PID身份", nil)
	}
	r.device.VendorID, r.device.ProductID, r.identified = uint16(v), uint16(pn), true
	return r, nil
}

func parseUSBDatabase(data []byte) (map[string]model.USBDevice, error) {
	devices := make(map[string]model.USBDevice)
	fields := make(map[string]string)
	flush := func() error {
		fields["ACTION"] = "add"
		r, err := parseUdevRecord(fields)
		if err != nil {
			return err
		}
		if r != nil {
			devices[r.device.Path] = r.device
		}
		fields = make(map[string]string)
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if len(line) < 3 || line[1:3] != ": " || line[0] < 'A' || line[0] > 'Z' {
			return nil, model.NewError(model.CodeUnsupportedProtocol, "udev数据库记录格式无效", nil)
		}
		if strings.HasPrefix(line, "P: ") {
			fields["DEVPATH"] = line[3:]
		}
		if strings.HasPrefix(line, "E: ") {
			key, value, ok := strings.Cut(line[3:], "=")
			if !ok {
				return nil, model.NewError(model.CodeUnsupportedProtocol, "udev数据库属性格式错误", nil)
			}
			fields[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, model.NewError(model.CodeMessageTooLarge, "udev数据库行超过限制", err)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return devices, nil
}
