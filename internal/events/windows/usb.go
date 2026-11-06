package windows

import (
	"sort"
	"strconv"
	"strings"

	"github.com/orzlee/displayKvm/internal/model"
)

// USBFilter的nil数字字段及空路径表示不限制；零VID/PID仍可作为明确筛选值。
// 不提供序列号筛选，因为当前系统采集不能可靠地区分硬件序列号与端口身份。
type USBFilter struct {
	VendorID  *uint16
	ProductID *uint16
	Path      string
}

func cloneFilters(filters []USBFilter) []USBFilter {
	result := append([]USBFilter(nil), filters...)
	for i := range result {
		if result[i].VendorID != nil {
			value := *result[i].VendorID
			result[i].VendorID = &value
		}
		if result[i].ProductID != nil {
			value := *result[i].ProductID
			result[i].ProductID = &value
		}
	}
	return result
}

func matchesUSB(device model.USBDevice, filters []USBFilter) bool {
	if len(filters) == 0 {
		return true
	}
	for _, filter := range filters {
		if (filter.VendorID == nil || *filter.VendorID == device.VendorID) &&
			(filter.ProductID == nil || *filter.ProductID == device.ProductID) &&
			(filter.Path == "" || strings.EqualFold(filter.Path, device.Path)) {
			return true
		}
	}
	return false
}

func (s *Source) selectSnapshot(snapshot Snapshot) (Snapshot, error) {
	if snapshot.ObservedAt.IsZero() || (!snapshot.USBKnown && len(snapshot.USBDevices) != 0) {
		return Snapshot{}, model.NewError(model.CodeIO, "Windows启动快照缺少观测时间或混淆未知USB状态", nil)
	}
	selected := cloneSnapshot(snapshot)
	if selected.USBDevices != nil {
		selected.USBDevices = selected.USBDevices[:0]
	}
	seen := make(map[string]model.USBDevice)
	for _, device := range snapshot.USBDevices {
		if device.Path == "" {
			return Snapshot{}, model.NewError(model.CodeIO, "Windows USB设备缺少可用于存在状态对账的接口路径", nil)
		}
		key := strings.ToLower(device.Path)
		if previous, ok := seen[key]; ok {
			if previous.VendorID != device.VendorID || previous.ProductID != device.ProductID || previous.Serial != device.Serial {
				return Snapshot{}, model.NewError(model.CodeIO, "同一Windows USB接口路径报告了冲突身份", nil)
			}
			continue
		}
		seen[key] = device
		if matchesUSB(device, s.filters) {
			selected.USBDevices = append(selected.USBDevices, device)
		}
	}
	sort.Slice(selected.USBDevices, func(i, j int) bool {
		return strings.ToLower(selected.USBDevices[i].Path) < strings.ToLower(selected.USBDevices[j].Path)
	})
	return selected, nil
}

type usbChange struct {
	kind   model.EventKind
	device model.USBDevice
}

func usbChanges(before, after []model.USBDevice) []usbChange {
	old, next := make(map[string]model.USBDevice, len(before)), make(map[string]model.USBDevice, len(after))
	for _, device := range before {
		old[strings.ToLower(device.Path)] = device
	}
	for _, device := range after {
		next[strings.ToLower(device.Path)] = device
	}
	var result []usbChange
	// VID/PID可能相同，序列号也可能为空；仅在本轮按不透明接口路径对账。
	// 元数据在同一路径变化时先移除旧身份再增加新身份，避免沿用错误筛选结果。
	for key, device := range old {
		other, ok := next[key]
		if !ok || other.VendorID != device.VendorID || other.ProductID != device.ProductID || other.Serial != device.Serial {
			result = append(result, usbChange{kind: model.EventUSBRemoved, device: device})
		}
	}
	for key, device := range next {
		other, ok := old[key]
		if !ok || other.VendorID != device.VendorID || other.ProductID != device.ProductID || other.Serial != device.Serial {
			result = append(result, usbChange{kind: model.EventUSBAdded, device: device})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].kind != result[j].kind {
			return result[i].kind == model.EventUSBRemoved
		}
		return strings.ToLower(result[i].device.Path) < strings.ToLower(result[j].device.Path)
	})
	return result
}

// 只解析SetupAPI的SPDRP_HARDWAREID标准USB标记，设备接口路径是系统不透明值，
// 即使其中看似包含VID/PID或末尾端口字段，也不能据此推导序列号或硬件身份。
func parseHardwareID(text string) (uint16, uint16, bool) {
	prefix, attributes, ok := strings.Cut(text, `\`)
	if !ok || !strings.EqualFold(prefix, "USB") {
		return 0, 0, false
	}
	var vid, pid uint16
	var hasVID, hasPID bool
	for _, part := range strings.Split(strings.ToUpper(attributes), "&") {
		if !strings.HasPrefix(part, "VID_") && !strings.HasPrefix(part, "PID_") {
			continue
		}
		if len(part) != 8 {
			return 0, 0, false
		}
		value, err := strconv.ParseUint(part[4:], 16, 16)
		if err != nil {
			return 0, 0, false
		}
		if strings.HasPrefix(part, "VID_") {
			if hasVID {
				return 0, 0, false
			}
			vid, hasVID = uint16(value), true
		} else {
			if hasPID {
				return 0, 0, false
			}
			pid, hasPID = uint16(value), true
		}
	}
	return vid, pid, hasVID && hasPID
}
