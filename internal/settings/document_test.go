package settings

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/automation"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/scheduler"
)

func TestUnifiedSettingsDefaultsAndStrictDocument(t *testing.T) {
	doc := NewDocument()
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	if doc.Kind != Kind || doc.Version != Version || doc.Configuration.Version != config.CurrentVersion || doc.Rules == nil || doc.Schedules == nil {
		t.Fatalf("默认文档缺少明确版本或空列表: %+v", doc)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, doc); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(bytes.NewReader(encoded.Bytes()))
	if err != nil || !reflect.DeepEqual(loaded, doc) {
		t.Fatalf("默认文档往返失败: %+v %v", loaded, err)
	}
	const valid = `{"kind":"displaykvm-settings","version":1,"configuration":{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]},"rules":[],"schedules":[],"runtime":{"tool_path":""}}`
	for name, raw := range map[string]string{
		"未知根字段":      strings.Replace(valid, `"version":1`, `"version":1,"future":true`, 1),
		"未知嵌套字段":     strings.Replace(valid, `"tool_path":""`, `"tool_path":"","future":true`, 1),
		"未知业务字段":     strings.Replace(valid, `"groups":[]`, `"groups":[],"future":true`, 1),
		"键名大小写别名":    strings.Replace(valid, `"kind":`, `"Kind":`, 1),
		"重复根字段":      strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"重复嵌套字段":     strings.Replace(valid, `"tool_path":""`, `"tool_path":"","tool_path":""`, 1),
		"错误种类":       strings.Replace(valid, Kind, "other", 1),
		"未来版本":       strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"配置版本不得隐式迁移": strings.Replace(valid, `"version":3`, `"version":2`, 1),
		"缺少配置":       strings.Replace(valid, `"configuration":{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]},`, "", 1),
		"空配置":        strings.Replace(valid, `{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`, "null", 1),
		"缺少规则":       strings.Replace(valid, `"rules":[],`, "", 1),
		"空规则":        strings.Replace(valid, `"rules":[]`, `"rules":null`, 1),
		"缺少日程":       strings.Replace(valid, `"schedules":[],`, "", 1),
		"空日程":        strings.Replace(valid, `"schedules":[]`, `"schedules":null`, 1),
		"空运行配置":      strings.Replace(valid, `{"tool_path":""}`, "null", 1),
		"缺少运行配置":     strings.Replace(valid, `,"runtime":{"tool_path":""}`, "", 1),
		"缺少工具字段":     strings.Replace(valid, `"tool_path":""`, "", 1),
		"空工具字段":      strings.Replace(valid, `"tool_path":""`, `"tool_path":null`, 1),
		"额外对象":       valid + `{}`,
		"非对象":        `null`,
		"超限":         strings.Repeat(" ", MaxBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(strings.NewReader(raw)); err == nil {
				t.Fatal("非法完整文档未被拒绝")
			}
		})
	}
	if _, err := Load(strings.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(nil); err == nil {
		t.Fatal("空读取器未被拒绝")
	}
	if _, err := Load(strings.NewReader(valid + strings.Repeat(" ", MaxBytes-len(valid)))); err != nil {
		t.Fatalf("恰好1MiB应可读取: %v", err)
	}
}

func unifiedFixture(t *testing.T) Document {
	t.Helper()
	doc := NewDocument()
	doc.Configuration.Displays = []model.Display{{ID: "left"}}
	doc.Configuration.InputSchemes = []model.InputScheme{{ID: "hdmi", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}}
	doc.Configuration.Targets = []model.Target{{ID: "work", Inputs: []model.TargetInput{{DisplayID: "left", SchemeID: "hdmi"}}}}
	doc.Rules = []automation.Rule{{ID: "dock", Enabled: true, When: automation.Condition{Kind: model.EventCustom, Name: "dock"}, Action: model.SwitchRequest{TargetID: "work"}}}
	doc.Schedules = []scheduler.Schedule{{ID: "morning", Enabled: true, Time: "09:00", Timezone: "UTC"}}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestUnifiedSettingsCalibrationRemainsInsideConfiguration(t *testing.T) {
	doc := unifiedFixture(t)
	confirmed := time.Unix(1000, 0).UTC()
	doc.Configuration.Calibrations = []config.CalibrationMetadata{{
		DisplayID: "left", TargetID: "work", PortName: "HDMI 1",
		Scheme: doc.Configuration.InputSchemes[0], Basis: "manual", ConfirmedAt: &confirmed,
	}}
	var data bytes.Buffer
	if err := Encode(&data, doc); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(&data)
	if err != nil || !reflect.DeepEqual(doc, loaded) {
		t.Fatalf("完整配置往返不得丢失v3校准依据: %+v %v", loaded, err)
	}
}

func TestUnifiedSettingsReusesBusinessValidationAndServiceReadiness(t *testing.T) {
	for name, change := range map[string]func(*Document){
		"业务引用":   func(d *Document) { d.Configuration.Targets[0].Inputs[0].SchemeID = "missing" },
		"规则引用":   func(d *Document) { d.Rules[0].Action.TargetID = "missing" },
		"规则条件":   func(d *Document) { d.Rules[0].When.Kind = "future" },
		"日程时间":   func(d *Document) { d.Schedules[0].Time = "24:00" },
		"日程时区":   func(d *Document) { d.Schedules[0].Timezone = "Local" },
		"相对工具":   func(d *Document) { d.Runtime.ToolPath = "tool" },
		"相对辅助工具": func(d *Document) { d.Runtime.BusctlPath = "busctl" },
	} {
		t.Run(name, func(t *testing.T) {
			doc := unifiedFixture(t)
			change(&doc)
			if err := doc.Validate(); err == nil {
				t.Fatal("现有校验规则未被复用")
			}
			resolved, err := doc.ValidateForService(controltools.Resolver{GOOS: "windows"})
			if err == nil || resolved != (controltools.Paths{}) {
				t.Fatalf("不依赖外部工具的平台仍须先拒绝业务和路径错误: %+v %v", resolved, err)
			}
		})
	}
	doc := unifiedFixture(t)
	if runtime.GOOS == "windows" {
		// Windows不提供POSIX执行位，也不依赖外部控制工具；不能强制Linux解析器
		// 检查本机假程序。业务拒绝用例在分支前执行，非Windows继续覆盖原Linux模拟夹具。
		resolved, err := doc.ValidateForService(controltools.Resolver{GOOS: "windows"})
		if err != nil || resolved != (controltools.Paths{}) || doc.Runtime != (controltools.Paths{}) {
			t.Fatalf("Windows空覆盖路径应无需外部工具即可就绪且不回写路径: %+v %v", resolved, err)
		}
		return
	}
	resolver, want := settingsProductTools(t)
	if resolved, err := doc.ValidateForService(resolver); err != nil || resolved != want {
		t.Fatalf("空覆盖路径应解析为随包工具: got=%+v want=%+v %v", resolved, want, err)
	}
	tool := filepath.Join(filepath.Dir(resolver.Executable), "tool")
	doc.Runtime.ToolPath = tool
	if err := doc.Validate(); err != nil {
		t.Fatalf("尚不存在的绝对工具位置仍可保存: %v", err)
	}
	if _, err := doc.ValidateForService(resolver); err == nil {
		t.Fatal("不存在的工具未被拒绝")
	}
	if err := os.WriteFile(tool, []byte("fake executable, never run"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.ValidateForService(resolver); err == nil {
		t.Fatal("不可执行工具未被拒绝")
	}
	if err := os.Chmod(tool, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.ValidateForService(resolver); err != nil {
		t.Fatal(err)
	}
	doc.Runtime.UdevadmPath = filepath.Dir(tool)
	if _, err := doc.ValidateForService(resolver); err == nil {
		t.Fatal("目录不能作为工具")
	}
}

type unifiedShortWriter struct{}

func (unifiedShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestUnifiedSettingsEncodeValidatesBeforeOutput(t *testing.T) {
	doc := NewDocument()
	doc.Configuration.Version = 99
	var output bytes.Buffer
	if err := Encode(&output, doc); err == nil || output.Len() != 0 {
		t.Fatalf("无效草稿不应产生半份输出: %v %q", err, output.Bytes())
	}
	if err := Encode(unifiedShortWriter{}, NewDocument()); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("短写错误丢失: %v", err)
	}
}

func TestUnifiedSettingsEmbeddedEmptyConfigurationAndNilListsUseExistingSemantics(t *testing.T) {
	const prefix = `{"kind":"displaykvm-settings","version":1,"configuration":`
	const suffix = `,"rules":[],"schedules":[],"runtime":{"tool_path":""}}`
	for name, configuration := range map[string]string{
		"缺省空列表":   `{"version":3}`,
		"null空列表": `{"version":3,"displays":null,"input_schemes":null,"targets":null,"groups":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Config{Version: config.CurrentVersion}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("原业务校验器应接受该空配置: %v", err)
			}
			doc, err := Load(strings.NewReader(prefix + configuration + suffix))
			if err != nil || !reflect.DeepEqual(doc.Configuration, cfg) {
				t.Fatalf("聚合文档不得加严既有空配置语义: %+v %v", doc, err)
			}
		})
	}
	doc := NewDocument()
	doc.Configuration = config.Config{Version: config.CurrentVersion}
	doc.Rules, doc.Schedules = nil, nil
	if err := doc.Validate(); err != nil {
		t.Fatalf("Prepare/Clone得到的合法nil空列表应可保存: %v", err)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, doc); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(&encoded)
	if err != nil || loaded.Rules == nil || loaded.Schedules == nil || len(loaded.Rules) != 0 || len(loaded.Schedules) != 0 {
		t.Fatalf("编码边界应统一把nil规则和日程规范为[]: %+v %v", loaded, err)
	}
	if !reflect.DeepEqual(doc.Configuration, loaded.Configuration) || doc.Rules != nil || doc.Schedules != nil {
		t.Fatal("编码不应修改业务配置语义或调用者持有的草稿")
	}
}

func TestUnifiedSettingsRejectsStandaloneConfigurationAndPartitions(t *testing.T) {
	for name, raw := range map[string]string{
		"configuration-v1": `{"version":1,"displays":[],"targets":[],"groups":[]}`,
		"configuration-v2": `{"version":2,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`,
		"configuration-v3": `{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`,
		"rules-only":       `{"version":1,"rules":[]}`,
		"schedules-only":   `{"version":1,"schedules":[]}`,
		"missing-kind":     `{"version":1,"configuration":{"version":3},"rules":[],"schedules":[],"runtime":{"tool_path":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(strings.NewReader(raw))
			var validation *model.Error
			if !errors.As(err, &validation) || validation.Code != model.CodeInvalidConfig {
				t.Fatalf("独立业务配置和分区文件必须统一报完整文档校验错误: %v", err)
			}
		})
	}
}
