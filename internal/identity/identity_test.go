package identity

import (
	"reflect"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

func configuredDisplay(id, path, serial string) model.Display {
	return model.Display{
		ID: model.DisplayID(id), Name: "已配置的" + id,
		Identity: model.IdentityHints{Manufacturer: "ACME", ProductCode: "panel", Serial: serial, Path: path},
	}
}

func enumeratedDevice(id, path, serial string) model.Device {
	return model.Device{
		BackendID: id, Name: "当前枚举的" + id,
		Identity: model.IdentityHints{Manufacturer: "ACME", ProductCode: "panel", Serial: serial, Path: path},
	}
}

func requireUnavailable(t *testing.T, resolution Resolution, code model.ErrorCode) {
	t.Helper()
	if resolution.Device != nil || resolution.Error == nil || model.CodeOf(resolution.Error) != code {
		t.Fatalf("不得返回可操作设备：结果=%+v，期望错误=%s", resolution, code)
	}
	if err := resolution.Error.Validate(); err != nil {
		t.Fatalf("结果必须使用有效的共享错误：%v", err)
	}
}

func TestResolveUsesCurrentEnumerationWithoutChangingLogicalIdentity(t *testing.T) {
	displays := []model.Display{configuredDisplay("left", "port-a", "serial-a"), configuredDisplay("right", "port-b", "serial-b")}
	before := []model.Display{configuredDisplay("left", "port-a", "serial-a"), configuredDisplay("right", "port-b", "serial-b")}
	for _, devices := range [][]model.Device{
		{enumeratedDevice("generation-1:a", "port-a", "serial-a"), enumeratedDevice("generation-1:b", "port-b", "serial-b")},
		{enumeratedDevice("generation-2:b", "port-b", "serial-b"), enumeratedDevice("generation-2:a", "port-a", "serial-a")},
	} {
		results, err := Resolve(displays, devices)
		if err != nil || len(results) != len(displays) {
			t.Fatalf("重新枚举应得到逐屏结果：%+v，%v", results, err)
		}
		for i, result := range results {
			if result.Error != nil || result.Device == nil || result.DisplayID != displays[i].ID || result.PathChanged {
				t.Fatalf("稳定身份不应随枚举次序变化：%+v", result)
			}
			if result.Device.Identity.Serial != displays[i].Identity.Serial {
				t.Fatalf("匹配到了其他逻辑屏：%+v", result)
			}
			found := false
			for _, device := range devices {
				found = found || *result.Device == device
			}
			if !found {
				t.Fatalf("必须返回本次枚举对象：%+v", result.Device)
			}
		}
	}
	if !reflect.DeepEqual(displays, before) {
		t.Fatal("重新枚举不得改变持久化逻辑屏或映射")
	}
}

func TestResolveDuplicateOrMissingHardwareNeverMergesDisplays(t *testing.T) {
	digest := strings.Repeat("a", 64)
	cases := []struct {
		name     string
		displays []model.Display
		devices  []model.Device
	}{
		{"同序列", []model.Display{configuredDisplay("left", "a", "same"), configuredDisplay("right", "b", "same")}, []model.Device{enumeratedDevice("one", "a", "same"), enumeratedDevice("two", "b", "same")}},
		{"空序列同型号", []model.Display{configuredDisplay("left", "a", ""), configuredDisplay("right", "b", "")}, []model.Device{enumeratedDevice("one", "a", ""), enumeratedDevice("two", "b", "")}},
		{"重复身份仅剩一台", []model.Display{configuredDisplay("left", "a", "same"), configuredDisplay("right", "b", "same")}, []model.Device{enumeratedDevice("one", "a", "same")}},
		{"同EDID空序列", []model.Display{
			{ID: "left", Identity: model.IdentityHints{EDIDSHA256: digest, Path: "a"}},
			{ID: "right", Identity: model.IdentityHints{EDIDSHA256: digest, Path: "b"}},
		}, []model.Device{
			{BackendID: "one", Identity: model.IdentityHints{EDIDSHA256: digest, Path: "a"}},
			{BackendID: "two", Identity: model.IdentityHints{EDIDSHA256: digest, Path: "b"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for reverse := 0; reverse < 2; reverse++ {
				results, err := Resolve(tc.displays, tc.devices)
				if err != nil || len(results) != len(tc.displays) {
					t.Fatalf("歧义应保留逐屏结果：%+v，%v", results, err)
				}
				for _, result := range results {
					requireUnavailable(t, result, model.CodeIdentityAmbiguous)
					if len(result.Candidates) != len(tc.devices) {
						t.Fatalf("不得隐去重复候选：%+v", result)
					}
				}
				for i, j := 0, len(tc.devices)-1; i < j; i, j = i+1, j-1 {
					tc.devices[i], tc.devices[j] = tc.devices[j], tc.devices[i]
				}
			}
		})
	}
}

func TestResolveRequiresStrongEvidenceAndUnchangedKnownPath(t *testing.T) {
	digest := strings.Repeat("a", 64)
	cases := []struct {
		name        string
		display     model.Display
		device      model.Device
		code        model.ErrorCode
		pathChanged bool
		candidates  int
	}{
		{"只有路径", model.Display{ID: "left", Identity: model.IdentityHints{Path: "a"}}, model.Device{BackendID: "one", Identity: model.IdentityHints{Path: "a"}}, model.CodeIdentityAmbiguous, false, 1},
		{"只有序列", model.Display{ID: "left", Identity: model.IdentityHints{Serial: "same", Path: "a"}}, model.Device{BackendID: "one", Identity: model.IdentityHints{Serial: "same", Path: "a"}}, model.CodeIdentityAmbiguous, false, 1},
		{"完全未知", model.Display{ID: "left"}, model.Device{BackendID: "one"}, model.CodeIdentityAmbiguous, false, 1},
		{"空白序列", configuredDisplay("left", "a", "  "), enumeratedDevice("one", "a", "  "), model.CodeIdentityAmbiguous, false, 1},
		{"无原路径", configuredDisplay("left", "", "serial-a"), enumeratedDevice("one", "a", "serial-a"), model.CodeIdentityAmbiguous, false, 1},
		{"无当前路径", configuredDisplay("left", "a", "serial-a"), enumeratedDevice("one", "", "serial-a"), model.CodeIdentityAmbiguous, false, 1},
		{"路径变化", configuredDisplay("left", "a", "serial-a"), enumeratedDevice("one", "b", "serial-a"), model.CodeIdentityAmbiguous, true, 1},
		{"相同路径被新序列占用", configuredDisplay("left", "a", "serial-a"), enumeratedDevice("one", "a", "serial-b"), model.CodeNotFound, false, 0},
		{"型号冲突", configuredDisplay("left", "a", "serial-a"), model.Device{BackendID: "one", Identity: model.IdentityHints{Manufacturer: "ACME", ProductCode: "other", Serial: "serial-a", Path: "a"}}, model.CodeNotFound, false, 0},
		{"EDID冲突不回退序列", model.Display{ID: "left", Identity: model.IdentityHints{EDIDSHA256: digest, Manufacturer: "ACME", ProductCode: "panel", Serial: "serial-a", Path: "a"}}, model.Device{BackendID: "one", Identity: model.IdentityHints{EDIDSHA256: strings.Repeat("b", 64), Manufacturer: "ACME", ProductCode: "panel", Serial: "serial-a", Path: "a"}}, model.CodeNotFound, false, 0},
		{"相同EDID不忽略序列冲突", model.Display{ID: "left", Identity: model.IdentityHints{EDIDSHA256: digest, Serial: "serial-a", Path: "a"}}, model.Device{BackendID: "one", Identity: model.IdentityHints{EDIDSHA256: digest, Serial: "serial-b", Path: "a"}}, model.CodeNotFound, false, 0},
		{"EDID大小写等价", model.Display{ID: "left", Identity: model.IdentityHints{EDIDSHA256: digest, Path: "a"}}, model.Device{BackendID: "one", Identity: model.IdentityHints{EDIDSHA256: strings.ToUpper(digest), Path: "a"}}, "", false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results, err := Resolve([]model.Display{tc.display}, []model.Device{tc.device})
			if err != nil || len(results) != 1 {
				t.Fatalf("应返回身份结果：%+v，%v", results, err)
			}
			result := results[0]
			if tc.code == "" {
				if result.Error != nil || result.Device == nil || *result.Device != tc.device {
					t.Fatalf("唯一且充分的身份应匹配：%+v", result)
				}
			} else {
				requireUnavailable(t, result, tc.code)
			}
			if result.PathChanged != tc.pathChanged || len(result.Candidates) != tc.candidates {
				t.Fatalf("候选或路径变化证据不符：%+v", result)
			}
		})
	}
}

func TestResolveManualBindingNeverFallsBackOrGuessesRemainingDevice(t *testing.T) {
	left := configuredDisplay("left", "a", "same")
	left.Binding = &model.Binding{Path: "a"}
	right := configuredDisplay("right", "b", "same")
	devices := []model.Device{enumeratedDevice("one", "a", "same"), enumeratedDevice("two", "b", "same")}
	for _, displays := range [][]model.Display{{left, right}, {right, left}} {
		results, err := Resolve(displays, devices)
		if err != nil || len(results) != 2 {
			t.Fatalf("人工绑定应保留其他屏歧义：%+v，%v", results, err)
		}
		for _, result := range results {
			if result.DisplayID == "left" {
				if result.Error != nil || result.Device == nil || result.Device.BackendID != "one" {
					t.Fatalf("人工确认路径应优先：%+v", result)
				}
			} else {
				requireUnavailable(t, result, model.CodeIdentityAmbiguous)
				if len(result.Candidates) != 2 {
					t.Fatal("不能删去人工绑定候选后猜测剩余设备")
				}
			}
		}
	}
	results, err := Resolve([]model.Display{left}, devices[1:])
	if err != nil || len(results) != 1 {
		t.Fatalf("人工路径缺席仍须逐屏返回：%+v，%v", results, err)
	}
	requireUnavailable(t, results[0], model.CodeNotFound)
	if !results[0].PathChanged || len(results[0].Candidates) != 1 {
		t.Fatal("人工路径失效只能展示其他候选，不能自动迁移")
	}
}

func TestResolveManualBindingRejectsConflictsAndDuplicatePaths(t *testing.T) {
	bound := configuredDisplay("left", "old", "serial-a")
	bound.Binding = &model.Binding{Path: "a"}
	for _, devices := range [][]model.Device{
		{enumeratedDevice("replacement", "a", "serial-b"), enumeratedDevice("similar", "other", "serial-a")},
		{enumeratedDevice("one", "a", "serial-a"), enumeratedDevice("two", "a", "serial-b")},
	} {
		results, err := Resolve([]model.Display{bound}, devices)
		if err != nil || len(results) != 1 {
			t.Fatalf("应显示绑定冲突：%+v，%v", results, err)
		}
		requireUnavailable(t, results[0], model.CodeIdentityAmbiguous)
		if len(results[0].Candidates) != 2 {
			t.Fatal("绑定路径上的冲突对象也必须可见")
		}
	}
	pathOnly := model.Device{BackendID: "current-uuid", Identity: model.IdentityHints{Path: "a"}}
	results, err := Resolve([]model.Display{bound}, []model.Device{pathOnly})
	if err != nil || len(results) != 1 || results[0].Error != nil || results[0].Device == nil || *results[0].Device != pathOnly {
		t.Fatalf("已人工确认路径允许后端缺少硬件提示，但不能编造提示：%+v，%v", results, err)
	}
}

func TestResolveUnavailableDevicesPreserveConfigurationAndKnownPathOwnership(t *testing.T) {
	left := configuredDisplay("left", "a", "serial-a")
	left.Binding = &model.Binding{Path: "a"}
	right := configuredDisplay("right", "b", "serial-b")
	displays := []model.Display{left, right}
	results, err := Resolve(displays, []model.Device{enumeratedDevice("unknown", "a", "serial-c"), enumeratedDevice("right-now", "b", "serial-b")})
	if err != nil || len(results) != 2 {
		t.Fatalf("拓扑变化须独立返回每屏状态：%+v，%v", results, err)
	}
	requireUnavailable(t, results[0], model.CodeIdentityAmbiguous)
	if results[1].Error != nil || results[1].Device == nil || results[1].Device.BackendID != "right-now" {
		t.Fatalf("可独立识别的另一屏不受缺席屏影响：%+v", results[1])
	}
	results, err = Resolve(displays, nil)
	if err != nil || len(results) != 2 {
		t.Fatalf("空枚举不是删除配置：%+v，%v", results, err)
	}
	for _, result := range results {
		requireUnavailable(t, result, model.CodeNotFound)
	}
	if !reflect.DeepEqual(displays, []model.Display{left, right}) || displays[0].Binding.Path != "a" {
		t.Fatal("缺席和冲突不得清除逻辑显示器或人工绑定")
	}
	claimant := configuredDisplay("claimant", "a", "serial-b")
	results, err = Resolve([]model.Display{left, claimant}, []model.Device{enumeratedDevice("replacement", "a", "serial-b")})
	if err != nil || len(results) != 2 {
		t.Fatalf("应保留人工路径所有权：%+v，%v", results, err)
	}
	requireUnavailable(t, results[0], model.CodeIdentityAmbiguous)
	requireUnavailable(t, results[1], model.CodeIdentityAmbiguous)
}

func TestResolveUnknownTopologyAndDuplicatePathsCannotCreateAutomaticOwnership(t *testing.T) {
	known := configuredDisplay("left", "a", "serial-a")
	for _, devices := range [][]model.Device{
		{enumeratedDevice("known", "a", "serial-a"), {BackendID: "unknown", Identity: model.IdentityHints{Path: "new"}}},
		{enumeratedDevice("known", "a", "serial-a"), enumeratedDevice("other", "a", "serial-b")},
		{{BackendID: "unknown", Identity: model.IdentityHints{Path: "a"}}},
	} {
		results, err := Resolve([]model.Display{known}, devices)
		if err != nil || len(results) != 1 {
			t.Fatalf("未知拓扑应产生逐屏歧义：%+v，%v", results, err)
		}
		requireUnavailable(t, results[0], model.CodeIdentityAmbiguous)
	}
}

func TestResolveReturnsDetachedCurrentDevicesAndCandidates(t *testing.T) {
	devices := []model.Device{enumeratedDevice("one", "a", "serial-a")}
	results, err := Resolve([]model.Display{configuredDisplay("left", "a", "serial-a")}, devices)
	if err != nil || len(results) != 1 || results[0].Device == nil || len(results[0].Candidates) != 1 {
		t.Fatalf("应取得可操作的当前对象：%+v，%v", results, err)
	}
	results[0].Device.BackendID = "changed-result"
	results[0].Candidates[0].Identity.Path = "changed-candidate"
	if devices[0] != enumeratedDevice("one", "a", "serial-a") {
		t.Fatal("修改返回结果不得修改输入枚举对象")
	}
}

func TestBindUpdatesOnlyChosenDisplayAndDeepCopiesBindings(t *testing.T) {
	left := configuredDisplay("left", "old-left", "old-serial")
	left.Binding = &model.Binding{Path: "old-left"}
	right := configuredDisplay("right", "right", "serial-b")
	right.Binding = &model.Binding{Path: "right"}
	displays := []model.Display{left, right}
	devices := []model.Device{enumeratedDevice("chosen-current", "new-left", "new-serial"), enumeratedDevice("right-current", "right", "serial-b")}
	updated, err := Bind(displays, devices, "left", "chosen-current")
	if err != nil || len(updated) != 2 {
		t.Fatalf("显式选择允许更新旧身份快照：%+v，%v", updated, err)
	}
	if updated[0].ID != left.ID || updated[0].Name != left.Name || updated[0].Identity != devices[0].Identity || updated[0].Binding == nil || updated[0].Binding.Path != "new-left" {
		t.Fatalf("重绑应保留逻辑ID和名称，更新设备快照：%+v", updated[0])
	}
	if !reflect.DeepEqual(displays, []model.Display{left, right}) || !reflect.DeepEqual(updated[1], right) {
		t.Fatal("绑定不能修改输入或其他逻辑屏")
	}
	updated[0].Binding.Path = "changed-left"
	updated[1].Binding.Path = "changed-right"
	updated[0].Identity.Serial = "changed-serial"
	updated[1].Name = "changed-name"
	if left.Binding.Path != "old-left" || right.Binding.Path != "right" || displays[0].Identity.Serial != "old-serial" || displays[1].Name != right.Name || devices[0].Identity.Serial != "new-serial" {
		t.Fatal("所有绑定指针和返回切片必须与输入分离")
	}
}

func TestBindCanExplicitlyDisambiguateDuplicateHardwareAndPathOnlyDevices(t *testing.T) {
	for _, devices := range [][]model.Device{
		{enumeratedDevice("one", "a", "same"), enumeratedDevice("two", "b", "same")},
		{{BackendID: "one", Identity: model.IdentityHints{Path: "a"}}, {BackendID: "two", Identity: model.IdentityHints{Path: "b"}}},
	} {
		displays := []model.Display{{ID: "left", Name: "左屏"}, {ID: "right", Name: "右屏"}}
		updated, err := Bind(displays, devices, "left", "one")
		if err != nil {
			t.Fatalf("显式选择第一屏失败：%v", err)
		}
		updated, err = Bind(updated, devices, "right", "two")
		if err != nil {
			t.Fatalf("显式选择第二屏失败：%v", err)
		}
		results, err := Resolve(updated, devices)
		if err != nil || len(results) != 2 {
			t.Fatalf("人工消歧应得到逐屏结果：%+v，%v", results, err)
		}
		for i, result := range results {
			if result.Error != nil || result.Device == nil || result.Device.BackendID != devices[i].BackendID {
				t.Fatalf("人工消歧不能合并重复硬件：%+v", result)
			}
		}
		if displays[0].Binding != nil || displays[1].Binding != nil {
			t.Fatal("绑定副本不得隐式保存到原配置")
		}
	}
}

func TestBindRejectsMissingOrOccupiedSelection(t *testing.T) {
	left := configuredDisplay("left", "old", "serial-a")
	boundOther := configuredDisplay("right", "other-old", "serial-b")
	boundOther.Binding = &model.Binding{Path: "a"}
	cases := []struct {
		name      string
		displays  []model.Display
		devices   []model.Device
		id        model.DisplayID
		backendID string
		code      model.ErrorCode
	}{
		{"逻辑屏不存在", []model.Display{left}, []model.Device{enumeratedDevice("one", "a", "serial-a")}, "missing", "one", model.CodeNotFound},
		{"枚举对象不存在", []model.Display{left}, []model.Device{enumeratedDevice("one", "a", "serial-a")}, "left", "stale-generation", model.CodeNotFound},
		{"空选择", []model.Display{left}, []model.Device{enumeratedDevice("one", "a", "serial-a")}, "left", "", model.CodeInvalidRequest},
		{"空逻辑选择", []model.Display{left}, []model.Device{enumeratedDevice("one", "a", "serial-a")}, "", "one", model.CodeInvalidRequest},
		{"无路径", []model.Display{left}, []model.Device{enumeratedDevice("one", "", "serial-a")}, "left", "one", model.CodeInvalidRequest},
		{"空白路径", []model.Display{left}, []model.Device{enumeratedDevice("one", "  ", "serial-a")}, "left", "one", model.CodeInvalidRequest},
		{"枚举路径重复", []model.Display{left}, []model.Device{enumeratedDevice("one", "a", "serial-a"), enumeratedDevice("two", "a", "serial-b")}, "left", "one", model.CodeIdentityAmbiguous},
		{"其他人工绑定占用", []model.Display{left, boundOther}, []model.Device{enumeratedDevice("one", "a", "serial-a")}, "left", "one", model.CodeIdentityAmbiguous},
		{"其他已知路径占用", []model.Display{left, configuredDisplay("right", "a", "serial-b")}, []model.Device{enumeratedDevice("one", "a", "serial-a")}, "left", "one", model.CodeIdentityAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, err := Bind(tc.displays, tc.devices, tc.id, tc.backendID)
			if updated != nil || err == nil || model.CodeOf(err) != tc.code {
				t.Fatalf("非法选择不得生成可保存配置：%+v，%v；期望%s", updated, err, tc.code)
			}
		})
	}
}

func TestIdentityRejectsInvalidConfigurationAndEnumerationSnapshots(t *testing.T) {
	bound := configuredDisplay("left", "a", "serial-a")
	bound.Binding = &model.Binding{Path: "a"}
	duplicateBinding := configuredDisplay("right", "b", "serial-b")
	duplicateBinding.Binding = &model.Binding{Path: "a"}
	cases := []struct {
		name     string
		displays []model.Display
		devices  []model.Device
		code     model.ErrorCode
	}{
		{"重复逻辑ID", []model.Display{configuredDisplay("left", "a", "serial-a"), configuredDisplay("left", "b", "serial-b")}, nil, model.CodeInvalidConfig},
		{"无效逻辑ID", []model.Display{{ID: "bad id"}}, nil, model.CodeInvalidConfig},
		{"重复人工路径", []model.Display{bound, duplicateBinding}, nil, model.CodeInvalidConfig},
		{"空人工路径", []model.Display{{ID: "left", Binding: &model.Binding{}}}, nil, model.CodeInvalidConfig},
		{"无效配置摘要", []model.Display{{ID: "left", Identity: model.IdentityHints{EDIDSHA256: "bad"}}}, nil, model.CodeInvalidConfig},
		{"空枚举ID", []model.Display{bound}, []model.Device{{}}, model.CodeInvalidRequest},
		{"重复枚举ID", []model.Display{bound}, []model.Device{enumeratedDevice("same", "a", "serial-a"), enumeratedDevice("same", "b", "serial-b")}, model.CodeIdentityAmbiguous},
		{"无效枚举摘要", []model.Display{bound}, []model.Device{{BackendID: "one", Identity: model.IdentityHints{EDIDSHA256: "bad"}}}, model.CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results, err := Resolve(tc.displays, tc.devices)
			if results != nil || err == nil || model.CodeOf(err) != tc.code {
				t.Fatalf("非法快照应在匹配前拒绝：%+v，%v；期望%s", results, err, tc.code)
			}
			updated, err := Bind(tc.displays, tc.devices, "left", "one")
			if updated != nil || err == nil || model.CodeOf(err) != tc.code {
				t.Fatalf("非法快照应在绑定前拒绝：%+v，%v；期望%s", updated, err, tc.code)
			}
		})
	}
}

func TestResolveEmptyConfigurationDoesNotAdoptEnumeratedUnknownDevices(t *testing.T) {
	results, err := Resolve(nil, []model.Device{enumeratedDevice("one", "a", "serial-a")})
	if err != nil || len(results) != 0 {
		t.Fatalf("枚举设备不能自动创建逻辑屏：%+v，%v", results, err)
	}
}
