package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/model"
)

const settingsProductWithoutDesktop = `{"kind":"displaykvm-settings","version":1,"configuration":{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]},"rules":[],"schedules":[],"runtime":{"tool_path":""}}`

func TestSettingsProductMissingDesktopDefaultsInMemoryWithoutRewrite(t *testing.T) {
	if got := NewDocument().Desktop.Language; got != "system" {
		t.Fatalf("新文档语言应默认跟随系统: %q", got)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(settingsProductWithoutDesktop + "\n")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, loadErr := Load(file)
	closeErr := file.Close()
	if loadErr != nil || closeErr != nil || doc.Desktop.Language != "system" {
		t.Fatalf("缺少desktop的完整文档只应在内存补system: %+v %v %v", doc, loadErr, closeErr)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, doc); err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(encoded.Bytes(), &root); err != nil || root["desktop"] == nil {
		t.Fatalf("下一次显式编码应包含desktop分区: %s %v", encoded.Bytes(), err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("读取或内存编码不得改写原文件: %q %v", after, err)
	}
}

func TestSettingsProductExplicitLanguagePreferencesRoundTrip(t *testing.T) {
	for _, preference := range []string{"system", "zh-CN", "en"} {
		t.Run(preference, func(t *testing.T) {
			raw := strings.TrimSuffix(settingsProductWithoutDesktop, "}") + `,"desktop":{"language":"` + preference + `"}}`
			doc, err := Load(strings.NewReader(raw))
			if err != nil || doc.Desktop.Language != preference {
				t.Fatalf("明确语言偏好读取失败: %+v %v", doc, err)
			}
			var encoded bytes.Buffer
			if err := Encode(&encoded, doc); err != nil {
				t.Fatal(err)
			}
			loaded, err := Load(&encoded)
			if err != nil || !reflect.DeepEqual(doc, loaded) {
				t.Fatalf("明确语言偏好往返丢失: %+v %v", loaded, err)
			}
		})
	}
}

func TestSettingsProductExplicitDesktopRejectsInvalidFields(t *testing.T) {
	for name, desktop := range map[string]string{
		"null分区":    `null`,
		"空对象":       `{}`,
		"数组":        `[]`,
		"字符串":       `"system"`,
		"空语言":       `{"language":""}`,
		"null语言":    `{"language":null}`,
		"错误类型":      `{"language":true}`,
		"未知值":       `{"language":"zh"}`,
		"偏好区分大小写":   `{"language":"SYSTEM"}`,
		"未知字段":      `{"language":"system","future":true}`,
		"语言字段大小写别名": `{"Language":"system"}`,
		"重复字段":      `{"language":"system","language":"en"}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.TrimSuffix(settingsProductWithoutDesktop, "}") + `,"desktop":` + desktop + `}`
			_, err := Load(strings.NewReader(raw))
			var validation *model.Error
			if !errors.As(err, &validation) || validation.Code != model.CodeInvalidConfig {
				t.Fatalf("明确但错误的desktop不得当成缺省: %v", err)
			}
		})
	}
	for name, suffix := range map[string]string{
		"根字段大小写别名":    `,"Desktop":{"language":"system"}}`,
		"重复desktop分区": `,"desktop":{"language":"system"},"desktop":{"language":"en"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(strings.NewReader(strings.TrimSuffix(settingsProductWithoutDesktop, "}") + suffix)); err == nil {
				t.Fatal("desktop根字段必须沿用完整文档严格解析")
			}
		})
	}
}

func TestSettingsProductMemoryZeroDesktopNormalizesOnlyAtEncode(t *testing.T) {
	doc := NewDocument()
	doc.Desktop = DesktopSettings{}
	before := doc
	if err := doc.Validate(); err != nil {
		t.Fatalf("现有字段构造产生的内存零值应可校验: %v", err)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, doc); err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(encoded.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	var desktop DesktopSettings
	if err := json.Unmarshal(root["desktop"], &desktop); err != nil || desktop.Language != "system" {
		t.Fatalf("内存零值必须标准编码为明确system: %s %v", encoded.Bytes(), err)
	}
	if !reflect.DeepEqual(doc, before) || doc.Desktop.Language != "" {
		t.Fatal("校验或编码不能修改调用方仍在编辑的草稿")
	}
	loaded, err := Load(&encoded)
	if err != nil || loaded.Desktop.Language != "system" {
		t.Fatalf("标准编码必须能被严格加载: %+v %v", loaded, err)
	}
	for _, preference := range []string{"zh", "SYSTEM", " en"} {
		doc.Desktop.Language = preference
		encoded.Reset()
		if err := Encode(&encoded, doc); err == nil || encoded.Len() != 0 {
			t.Fatalf("非法非零内存偏好不得输出半份文档: %q %v", encoded.Bytes(), err)
		}
	}
}

func TestSettingsProductOtherSectionsPreserveDesktopAndRuntime(t *testing.T) {
	for name, change := range map[string]func(*Document){
		"设备": func(d *Document) {
			d.Configuration.Displays = append(d.Configuration.Displays, model.Display{ID: "right"})
		},
		"规则": func(d *Document) { d.Rules[0].Enabled = false },
		"日程": func(d *Document) { d.Schedules[0].Time = "10:00" },
		"校准": func(d *Document) {
			confirmed := time.Unix(1000, 0).UTC()
			d.Configuration.Calibrations = []config.CalibrationMetadata{{
				DisplayID: "left", TargetID: "work", PortName: "HDMI 1",
				Scheme: d.Configuration.InputSchemes[0], Basis: "manual", ConfirmedAt: &confirmed,
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc := unifiedFixture(t)
			doc.Desktop.Language = "zh-CN"
			doc.Runtime.ToolPath = filepath.Join(t.TempDir(), "override")
			change(&doc)
			var encoded bytes.Buffer
			if err := Encode(&encoded, doc); err != nil {
				t.Fatal(err)
			}
			loaded, err := Load(&encoded)
			if err != nil || !reflect.DeepEqual(doc, loaded) {
				t.Fatalf("其它分区改动不得丢失桌面语言或工具设置: %+v %v", loaded, err)
			}
		})
	}
}

func settingsProductTools(t *testing.T) (controltools.Resolver, controltools.Paths) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "displaykvmd")
	paths := controltools.Paths{
		ToolPath: filepath.Join(directory, "ddcutil"), BusctlPath: filepath.Join(directory, "busctl"),
		UdevadmPath: filepath.Join(directory, "udevadm"),
	}
	for _, path := range []string{executable, paths.ToolPath, paths.BusctlPath, paths.UdevadmPath} {
		if err := os.WriteFile(path, []byte("isolated fixture, never executed"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return controltools.Resolver{GOOS: "linux", Executable: executable}, paths
}

func TestSettingsProductServiceResolutionDoesNotPersistInstalledPaths(t *testing.T) {
	type resolutionCase struct {
		name        string
		resolver    controltools.Resolver
		paths, want controltools.Paths
	}
	var cases []resolutionCase
	if runtime.GOOS == "windows" {
		// Windows文件元信息不提供POSIX执行位，强制Linux解析会先拒绝本机夹具。
		// 实际调用Windows分支覆盖空配置及缺失/现存历史工具；运行用空路径不得
		// 清除文档中的历史覆盖，否则切回原平台会丢失高级配置。
		directory := t.TempDir()
		_, legacy := settingsProductTools(t)
		cases = []resolutionCase{
			{name: "empty", resolver: controltools.Resolver{GOOS: "windows"}},
			{
				name:     "missing-legacy",
				resolver: controltools.Resolver{GOOS: "windows", Executable: filepath.Join(directory, "missing-displaykvmd.exe")},
				paths: controltools.Paths{
					ToolPath: filepath.Join(directory, "missing-ddcutil"), BusctlPath: filepath.Join(directory, "missing-busctl"),
					UdevadmPath: filepath.Join(directory, "missing-udevadm"),
				},
			},
			{name: "present-legacy", resolver: controltools.Resolver{GOOS: "windows", Executable: "relative-displaykvmd.exe"}, paths: legacy},
		}
	} else {
		// 非Windows保留Linux完整随包夹具，持续核对三项工具解析及安装路径不回写。
		resolver, want := settingsProductTools(t)
		cases = []resolutionCase{{name: "bundled-linux", resolver: resolver, want: want}}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := NewDocument()
			doc.Runtime = tc.paths
			before := doc
			resolved, err := doc.ValidateForService(tc.resolver)
			if err != nil || resolved != tc.want {
				t.Fatalf("服务工具解析结果不符: got=%+v want=%+v %v", resolved, tc.want, err)
			}
			if !reflect.DeepEqual(doc, before) || doc.Runtime != tc.paths {
				t.Fatal("解析不得回写安装绝对路径或清除文档中的历史覆盖")
			}
			var encoded bytes.Buffer
			if err := Encode(&encoded, doc); err != nil {
				t.Fatal(err)
			}
			loaded, err := Load(&encoded)
			if err != nil || loaded.Runtime != tc.paths {
				t.Fatalf("服务就绪校验后保存应保留原覆盖且不新增安装路径: got=%+v want=%+v %v", loaded.Runtime, tc.paths, err)
			}
		})
	}

	doc := NewDocument()
	doc.Desktop.Language = "invalid"
	resolved, err := doc.ValidateForService(controltools.Resolver{GOOS: "unsupported"})
	var validation *model.Error
	if !errors.As(err, &validation) || validation.Code != model.CodeInvalidConfig || validation.Field != "desktop.language" || resolved != (controltools.Paths{}) {
		t.Fatalf("必须先报告文档错误，不得先调用解析器或返回部分路径: %+v %v", resolved, err)
	}
}

func TestSettingsProductMissingBundledToolsFailServiceButAllowSave(t *testing.T) {
	resolver, paths := settingsProductTools(t)
	if err := os.Remove(paths.ToolPath); err != nil {
		t.Fatal(err)
	}
	doc := NewDocument()
	resolved, err := doc.ValidateForService(resolver)
	if err == nil || resolved != (controltools.Paths{}) {
		t.Fatalf("缺少随包工具必须严格失败且不返回部分路径: %+v %v", resolved, err)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, doc); err != nil {
		t.Fatalf("保存语言和草稿不应依赖工具已安装: %v", err)
	}
	if doc.Runtime != (controltools.Paths{}) {
		t.Fatal("解析失败不得改写草稿工具设置")
	}
}
