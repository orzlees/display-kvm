package config_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

func TestManagementLegacyConfigurationAndOptionalMetadata(t *testing.T) {
	TestMigrationPreservesPerDisplayProtocolAndCalibration(t)
	cfg, err := config.Load(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != config.CurrentVersion || len(cfg.Calibrations) != 0 {
		t.Fatalf("旧v2迁移不能凭空校准: %+v", cfg)
	}
	var plain bytes.Buffer
	if err := config.Encode(&plain, cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), `"calibrations"`) {
		t.Fatal("缺省元数据应保持省略语义")
	}
	confirmed := time.Now().UTC()
	cfg.Calibrations = []config.CalibrationMetadata{{DisplayID: "left", TargetID: "work", PortName: "HDMI 1", Scheme: cfg.InputSchemes[0], Basis: "manual", ConfirmedAt: &confirmed}}
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(&encoded)
	if err != nil || len(loaded.Calibrations) != 1 || loaded.Calibrations[0].PortName != "HDMI 1" {
		t.Fatalf("元数据须同文档往返: %+v %v", loaded, err)
	}
	loaded.Calibrations[0].Basis = "readback"
	if err := loaded.Validate(); err == nil {
		t.Fatal("无真实读回不能声明readback")
	}
	if _, err := config.Load(strings.NewReader(strings.Replace(validConfig, `"version": 2`, `"version": 99`, 1))); model.CodeOf(err) != model.CodeUnsupportedVersion {
		t.Fatalf("未知格式不能静默迁移: %v", err)
	}
}

const validConfig = `{
  "version": 2,
  "displays": [
    {"id":"left","name":"左屏","identity":{"manufacturer":"ABC","serial":"same"}},
    {"id":"right","name":"右屏","identity":{"manufacturer":"ABC","serial":"same"}}
  ],
  "input_schemes": [
    {"id":"hdmi-left","protocol":"vcp","vcp_code":96,"value":17},
    {"id":"dp-right","protocol":"lg_input_alt","value":144}
  ],
  "targets": [
    {"id":"work","name":"工作机","inputs":[
      {"display_id":"left","scheme_id":"hdmi-left"},
      {"display_id":"right","scheme_id":"dp-right"}
    ]}
  ],
  "groups":[{"id":"desk","name":"桌面","display_ids":["left","right"]}]
}`

func TestLoadAllowsDuplicateHardwareHintsAndPreservesLogicalIdentity(t *testing.T) {
	cfg, err := config.Load(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Displays) != 2 || cfg.Displays[0].ID == cfg.Displays[1].ID {
		t.Fatal("相同序列号不能自动合并两台逻辑显示器")
	}
	cfg.Displays[0].Identity.Serial = ""
	cfg.Displays[1].Identity.Serial = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("空序列号不能破坏人工逻辑身份：%v", err)
	}
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load(&encoded)
	if err != nil || reloaded.Targets[0].Inputs[1].DisplayID != "right" || reloaded.Groups[0].DisplayIDs[1] != "right" || reloaded.InputSchemes[1].Value != 144 {
		t.Fatalf("目标、分组或校准值往返不一致：%v", err)
	}
}

func TestConfigRejectsInvalidReferencesAndDuplicateIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"display ID", func(c *config.Config) { c.Displays[1].ID = c.Displays[0].ID }},
		{"scheme ID", func(c *config.Config) { c.InputSchemes[1].ID = c.InputSchemes[0].ID }},
		{"target ID", func(c *config.Config) { c.Targets = append(c.Targets, c.Targets[0]) }},
		{"group ID", func(c *config.Config) { c.Groups = append(c.Groups, c.Groups[0]) }},
		{"unknown display", func(c *config.Config) { c.Targets[0].Inputs[0].DisplayID = "missing" }},
		{"unknown scheme", func(c *config.Config) { c.Targets[0].Inputs[0].SchemeID = "missing" }},
		{"duplicate mapping", func(c *config.Config) { c.Targets[0].Inputs = append(c.Targets[0].Inputs, c.Targets[0].Inputs[0]) }},
		{"unknown group member", func(c *config.Config) { c.Groups[0].DisplayIDs[0] = "missing" }},
		{"duplicate group member", func(c *config.Config) { c.Groups[0].DisplayIDs[1] = "left" }},
		{"empty target", func(c *config.Config) { c.Targets[0].Inputs = nil }},
		{"empty group", func(c *config.Config) { c.Groups[0].DisplayIDs = nil }},
		{"invalid digest", func(c *config.Config) { c.Displays[0].Identity.EDIDSHA256 = "1234" }},
		{"conflicting manual binding", func(c *config.Config) {
			c.Displays[0].Binding = &model.Binding{Path: "same-path"}
			c.Displays[1].Binding = &model.Binding{Path: "same-path"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(strings.NewReader(validConfig))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&cfg)
			if err := cfg.Validate(); model.CodeOf(err) != model.CodeInvalidConfig {
				t.Fatalf("非法配置应明确拒绝：%v", err)
			}
		})
	}
}

func TestMigrationPreservesPerDisplayProtocolAndCalibration(t *testing.T) {
	// v1是本契约定义的兼容输入格式，不代表项目曾发布或存在真实旧配置。
	legacy := `{
      "version":1,
      "displays":[{"id":"left","name":"左屏","identity":{"serial":""}}, {"id":"right","name":"右屏","identity":{"serial":""}}],
      "targets":[{"id":"work","name":"工作机","inputs":[
        {"display_id":"left","protocol":"vcp","vcp_code":96,"value":17},
        {"display_id":"right","protocol":"lg_input_alt","value":144}
      ]}],
      "groups":[{"id":"desk","name":"桌面","display_ids":["left","right"]}]
    }`
	cfg, err := config.Load(strings.NewReader(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != config.CurrentVersion || len(cfg.InputSchemes) != 2 || cfg.InputSchemes[0].Protocol != model.ProtocolVCP || cfg.InputSchemes[0].VCPCode != 96 || cfg.InputSchemes[0].Value != 17 || cfg.InputSchemes[1].Protocol != model.ProtocolLGInputAlt || cfg.InputSchemes[1].Value != 144 {
		t.Fatalf("迁移改变了原有每屏协议或校准值：%+v", cfg)
	}
	if cfg.Targets[0].Inputs[1].SchemeID != cfg.InputSchemes[1].ID || cfg.Groups[0].DisplayIDs[1] != "right" {
		t.Fatal("迁移丢失引用或分组")
	}
	again, err := config.Load(strings.NewReader(legacy))
	if err != nil || cfg.InputSchemes[0].ID != again.InputSchemes[0].ID {
		t.Fatalf("迁移必须确定性生成方案ID：%v", err)
	}
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded.String(), `"version": 3`) {
		t.Fatal("编码必须输出当前版本")
	}
	if _, err := config.Load(strings.NewReader(strings.Replace(legacy, `"lg_input_alt"`, `"future"`, 1))); model.CodeOf(err) != model.CodeUnsupportedProtocol {
		t.Fatalf("迁移不能默默改写未知厂商协议：%v", err)
	}
}

func TestLoadRejectsUnsupportedMalformedOrOversizedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		code model.ErrorCode
	}{
		{"future version", `{"version":99}`, model.CodeUnsupportedVersion},
		{"missing version", `{}`, model.CodeInvalidConfig},
		{"unknown field", strings.Replace(validConfig, `"version": 2`, `"version":2,"typo":true`, 1), model.CodeInvalidConfig},
		{"duplicate field", `{"version":1,"version":2}`, model.CodeInvalidConfig},
		{"extra document", validConfig + `{}`, model.CodeInvalidConfig},
		{"null", `null`, model.CodeInvalidConfig},
		{"out of range value", strings.Replace(validConfig, `"value":17`, `"value":65536`, 1), model.CodeInvalidConfig},
		{"unknown protocol", strings.Replace(validConfig, `"vcp"`, `"future"`, 1), model.CodeUnsupportedProtocol},
		{"too large", strings.Repeat(" ", config.MaxConfigBytes+1), model.CodeMessageTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := config.Load(strings.NewReader(tc.raw)); model.CodeOf(err) != tc.code {
				t.Fatalf("期望错误 %s，得到 %v", tc.code, err)
			}
		})
	}
}

type failingReader struct{ cause error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.cause }

type failingWriter struct{ cause error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.cause }

func TestConfigurationIOErrorsAreNotSilentlyIgnored(t *testing.T) {
	cause := errors.New("storage unavailable")
	if _, err := config.Load(failingReader{cause}); !errors.Is(err, cause) || model.CodeOf(err) != model.CodeIO {
		t.Fatalf("读取失败必须保留根因：%v", err)
	}
	cfg, err := config.Load(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Encode(failingWriter{io.ErrClosedPipe}, cfg); !errors.Is(err, io.ErrClosedPipe) || model.CodeOf(err) != model.CodeIO {
		t.Fatalf("写入失败必须保留根因：%v", err)
	}
	var untouched bytes.Buffer
	cfg.Targets[0].Inputs[0].SchemeID = "missing"
	if err := config.Encode(&untouched, cfg); err == nil || untouched.Len() != 0 {
		t.Fatal("必须在写入任何字节前拒绝非法配置")
	}
}
