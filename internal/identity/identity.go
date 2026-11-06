// Package identity提供无硬件和文件副作用的身份匹配与人工绑定。逻辑ID始终来自
// 配置，枚举对象只用于当前快照；调用者仍须通过后端核验设备在执行时是否有效。
package identity

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/orzlee/displayKvm/internal/model"
)

// Resolution保留每个已配置显示器的结果。只有Error为nil且Device非nil时，
// Device才是可交给后端的本次枚举对象；Candidates只供展示和人工选择，不能用于I/O。
// 候选包括信息不足的兼容对象，以及人工路径上的冲突对象，不代表已确认物理身份。
type Resolution struct {
	DisplayID  model.DisplayID
	Device     *model.Device
	Candidates []model.Device
	Error      *model.Error
	// PathChanged表示原路径未出现，但其他非空路径存在充分硬件线索相符的候选。
	// 它只提示需要重新确认路径，不证明设备已经迁移，也不表示发生过系统重枚举事件。
	PathChanged bool
}

// Resolve按配置顺序返回逐屏匹配结果，不创建逻辑显示器，不修改配置或缓存设备。
// 整体error表示配置或枚举快照无效；缺席、歧义和需要重绑放入对应Resolution.Error，
// 因而一块屏缺席不会丢失其他屏的结果，也不会隐式删除目标映射。
func Resolve(displays []model.Display, devices []model.Device) ([]Resolution, error) {
	if err := validateInputs(displays, devices); err != nil {
		return nil, err
	}
	paths := make(map[string][]int, len(devices))
	for i, device := range devices {
		if strings.TrimSpace(device.Identity.Path) != "" {
			paths[device.Identity.Path] = append(paths[device.Identity.Path], i)
		}
	}
	boundOwners := make(map[string]model.DisplayID)
	for _, display := range displays {
		if display.Binding != nil {
			boundOwners[display.Binding.Path] = display.ID
		}
	}
	results := make([]Resolution, len(displays))
	candidates := make([][]int, len(displays))
	claims := make([]int, len(devices))
	// 必须先构造完整的双向候选关系，再判断任意一屏。人工绑定不删去其他屏的
	// 硬件候选，否则两台同序列设备会在绑定第一台后被错误地按“剩余一台”自动认领。
	// 缺席的逻辑屏也参与竞争检查，防止两条相同身份配置在仅剩一台设备时自动合并。
	for i, display := range displays {
		results[i].DisplayID = display.ID
		for j, device := range devices {
			compatible, _ := compareHardware(display.Identity, device.Identity)
			if compatible {
				candidates[i] = append(candidates[i], j)
				claims[j]++
			}
			if compatible || display.Binding != nil && display.Binding.Path == device.Identity.Path {
				results[i].Candidates = append(results[i].Candidates, device)
			}
		}
	}
	for i, display := range displays {
		result := &results[i]
		expectedPath := display.Identity.Path
		if display.Binding != nil {
			expectedPath = display.Binding.Path
		}
		if strings.TrimSpace(expectedPath) != "" && len(paths[expectedPath]) == 0 {
			for _, j := range candidates[i] {
				_, strong := compareHardware(display.Identity, devices[j].Identity)
				if strong && strings.TrimSpace(devices[j].Identity.Path) != "" {
					result.PathChanged = true
					break
				}
			}
		}
		if display.Binding != nil {
			// Binding是人工确认的路径约束。即使其他路径有唯一相同EDID，原路径
			// 失效也不能自动迁移；原路径上的硬件信息发生明确冲突时同样必须阻止。
			// 仅提供路径的后端可依赖既有人工确认，但无法辨认同路径且无提示的替换。
			matches := paths[display.Binding.Path]
			switch len(matches) {
			case 0:
				result.Error = model.NewError(model.CodeNotFound, "人工绑定路径当前缺席，须确认设备并重新绑定", nil)
			case 1:
				device := devices[matches[0]]
				if compatible, _ := compareHardware(display.Identity, device.Identity); !compatible {
					result.Error = model.NewError(model.CodeIdentityAmbiguous, "人工绑定路径上的硬件信息冲突，须重新确认设备", nil)
				} else {
					result.Device = &device
				}
			default:
				result.Error = model.NewError(model.CodeIdentityAmbiguous, "人工绑定路径对应多个枚举对象，不能唯一选择设备", nil)
			}
			continue
		}
		switch len(candidates[i]) {
		case 0:
			result.Error = model.NewError(model.CodeNotFound, "当前枚举中没有兼容的显示器候选", nil)
			continue
		case 1:
		default:
			result.Error = model.NewError(model.CodeIdentityAmbiguous, "显示器存在多个兼容候选，须人工绑定", nil)
			continue
		}
		j := candidates[i][0]
		device := devices[j]
		_, strong := compareHardware(display.Identity, device.Identity)
		owner, reserved := boundOwners[device.Identity.Path]
		// 路径只是附加约束，不能单独证明身份；摘要或完整厂商/型号/序列相符也
		// 仍需全配置及全枚举一对一。路径变化一律要求人工确认，避免扩展坞拓扑变化
		// 或同型号设备缺席后把旧映射漂移到另一台相似设备。这里不做任何顺序贪心。
		switch {
		case !strong:
			result.Error = model.NewError(model.CodeIdentityAmbiguous, "硬件身份线索不足，须人工绑定", nil)
		case strings.TrimSpace(expectedPath) == "" || strings.TrimSpace(device.Identity.Path) == "":
			result.Error = model.NewError(model.CodeIdentityAmbiguous, "缺少原路径或当前路径，须人工绑定", nil)
		case expectedPath != device.Identity.Path:
			result.Error = model.NewError(model.CodeIdentityAmbiguous, "候选路径已经变化，须人工重新绑定", nil)
		case len(paths[device.Identity.Path]) != 1:
			result.Error = model.NewError(model.CodeIdentityAmbiguous, "当前路径对应多个枚举对象，不能自动匹配", nil)
		case claims[j] != 1:
			result.Error = model.NewError(model.CodeIdentityAmbiguous, "同一设备属于多个逻辑显示器的候选，须人工绑定", nil)
		case reserved && owner != display.ID:
			result.Error = model.NewError(model.CodeIdentityAmbiguous, "候选路径已由其他逻辑显示器人工绑定", nil)
		default:
			result.Device = &device
		}
	}
	return results, nil
}

// Bind把调用者显式选择的当前枚举对象绑定到已有逻辑显示器，返回独立配置副本。
// backendID不是永久ID，也不能选择旧周期对象；调用者必须传入本次完整枚举快照，
// 并负责用户确认与后续保存。此函数不扫描硬件、不写配置、不自动挑选相似设备。
func Bind(displays []model.Display, devices []model.Device, id model.DisplayID, backendID string) ([]model.Display, error) {
	if err := validateInputs(displays, devices); err != nil {
		return nil, err
	}
	if !model.ValidID(string(id)) || strings.TrimSpace(backendID) == "" {
		return nil, model.NewError(model.CodeInvalidRequest, "人工绑定必须显式提供有效逻辑显示器ID和当前枚举对象ID", nil)
	}
	displayIndex := -1
	for i, display := range displays {
		if display.ID == id {
			displayIndex = i
			break
		}
	}
	if displayIndex < 0 {
		return nil, model.NewError(model.CodeNotFound, "人工绑定引用的逻辑显示器不存在", nil)
	}
	deviceIndex := -1
	for i, device := range devices {
		if device.BackendID == backendID {
			deviceIndex = i
			break
		}
	}
	if deviceIndex < 0 {
		return nil, model.NewError(model.CodeNotFound, "人工绑定选择的对象不在当前枚举中", nil)
	}
	selected := devices[deviceIndex]
	path := selected.Identity.Path
	if strings.TrimSpace(path) == "" {
		return nil, model.NewError(model.CodeInvalidRequest, "当前设备缺少可保存的路径，不能人工绑定", nil)
	}
	for i, device := range devices {
		if i != deviceIndex && device.Identity.Path == path {
			return nil, model.NewError(model.CodeIdentityAmbiguous, "选择的设备路径对应多个枚举对象，不能人工绑定", nil)
		}
	}
	for i, display := range displays {
		if i == displayIndex {
			continue
		}
		occupiedPath := display.Identity.Path
		if display.Binding != nil {
			occupiedPath = display.Binding.Path
		}
		// 缺席不代表放弃路径所有权。人工选择不能顺带抢占其他逻辑屏保存的有效
		// 路径约束，否则下一次重枚举会使原配置与新绑定同时指向同一台设备。
		if occupiedPath == path {
			return nil, model.NewError(model.CodeIdentityAmbiguous, "选择的路径已由其他逻辑显示器占用，须先确认并调整原映射", nil)
		}
	}
	updated := make([]model.Display, len(displays))
	for i, display := range displays {
		updated[i] = display
		// Binding是Display中唯一的指针字段，所有屏都要复制，不能只复制目标屏。
		// 否则调用者编辑返回配置中的另一屏时，会在尚未保存前修改共享输入快照。
		if display.Binding != nil {
			binding := *display.Binding
			updated[i].Binding = &binding
		}
	}
	// 人工重绑允许替换过时的硬件快照，但保留逻辑ID和名称，已有目标/分组引用
	// 因此仍然有效；保留旧序列或摘要会让明确确认后的新绑定继续被旧信息拒绝。
	updated[displayIndex].Identity = selected.Identity
	updated[displayIndex].Binding = &model.Binding{Path: path}
	return updated, nil
}

// validateInputs在两个公开入口统一检查快照完整性，避免非法配置或重复的临时ID
// 绕过一对一匹配。枚举路径重复属于逐屏歧义，不在这里把其他独立设备一并判失败。
func validateInputs(displays []model.Display, devices []model.Device) error {
	ids := make(map[model.DisplayID]bool, len(displays))
	bindings := make(map[string]bool)
	for i, display := range displays {
		if err := display.Validate(); err != nil {
			classified := model.NewError(model.CodeInvalidConfig, "显示器配置无效", err)
			classified.Field = fmt.Sprintf("displays[%d]", i)
			return classified
		}
		if ids[display.ID] {
			return model.NewError(model.CodeInvalidConfig, "显示器逻辑ID重复", nil)
		}
		ids[display.ID] = true
		if display.Binding != nil {
			if strings.TrimSpace(display.Binding.Path) == "" {
				return model.NewError(model.CodeInvalidConfig, "人工绑定路径不能仅含空白", nil)
			}
			if bindings[display.Binding.Path] {
				return model.NewError(model.CodeInvalidConfig, "同一路径不能人工绑定多个逻辑显示器", nil)
			}
			bindings[display.Binding.Path] = true
		}
	}
	backendIDs := make(map[string]bool, len(devices))
	for i, device := range devices {
		if strings.TrimSpace(device.BackendID) == "" {
			return model.NewError(model.CodeInvalidRequest, "枚举对象缺少当前后端ID", nil)
		}
		if backendIDs[device.BackendID] {
			return model.NewError(model.CodeIdentityAmbiguous, "当前枚举对象ID重复，不能唯一选择设备", nil)
		}
		backendIDs[device.BackendID] = true
		// Device没有单独的Validate入口；非空摘要仍必须遵守共享身份字段的格式，
		// 不能把损坏的枚举摘要当成有效强证据，也不能静默丢弃后回退到较弱提示。
		if device.Identity.EDIDSHA256 != "" {
			decoded, err := hex.DecodeString(device.Identity.EDIDSHA256)
			if err != nil || len(decoded) != 32 {
				classified := model.NewError(model.CodeInvalidRequest, "枚举设备的EDID摘要必须为64位十六进制SHA256", err)
				classified.Field = fmt.Sprintf("devices[%d].identity.edid_sha256", i)
				return classified
			}
		}
	}
	return nil
}

// compareHardware返回硬件线索是否兼容、是否足以参与自动匹配。缺失字段只表示
// 未知，不能用来排除候选；例如新出现且无序列的同型号设备仍须阻止猜选另一台。
// 路径和显示名不参与硬件比较。摘要相符或完整厂商/型号/序列相符仅提供强候选，
// 仍须由Resolve验证全局唯一及原路径，不能把这些线索当成永久唯一键。
func compareHardware(known, current model.IdentityHints) (compatible, strong bool) {
	if known.EDIDSHA256 != "" && current.EDIDSHA256 != "" && !strings.EqualFold(known.EDIDSHA256, current.EDIDSHA256) {
		return false, false
	}
	for _, values := range [][2]string{
		{known.Manufacturer, current.Manufacturer},
		{known.ProductCode, current.ProductCode},
		{known.Serial, current.Serial},
	} {
		if strings.TrimSpace(values[0]) != "" && strings.TrimSpace(values[1]) != "" && values[0] != values[1] {
			return false, false
		}
	}
	strong = known.EDIDSHA256 != "" && current.EDIDSHA256 != "" ||
		strings.TrimSpace(known.Manufacturer) != "" && strings.TrimSpace(current.Manufacturer) != "" &&
			strings.TrimSpace(known.ProductCode) != "" && strings.TrimSpace(current.ProductCode) != "" &&
			strings.TrimSpace(known.Serial) != "" && strings.TrimSpace(current.Serial) != ""
	return true, strong
}
