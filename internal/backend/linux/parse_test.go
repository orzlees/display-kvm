package linux

import (
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

// 样本按2.2.7-release的i2c_report_active_bus简洁输出构造，包含新版连接器字段；
// 测试不调用ddcutil，不把文档中旧版省略字段的示例当作本版本的完整输出。
const displayFixture = "Display 1\n   I2C bus: /dev/i2c-7\n   DRM connector: card0-DP-1\n   drm_connector_id: 95\n   Monitor: DEL:DELL P2411H:SERIAL7\n"

// 3.0.2样本由官方源码i2c_report_active_bus的简洁格式构造，非实机采集。
// DRM connector可省略，drm_connector_id仍必须存在；两版共享解析器，不能放宽未知字段。
const displayFixture302 = "Display 1\n   I2C bus: /dev/i2c-7\n   drm_connector_id: -1\n   Monitor: DEL:DELL P2411H:SERIAL7\n"

func TestParseDisplays302SourceFormatAndAbnormalFields(t *testing.T) {
	for _, text := range []string{displayFixture302, strings.ReplaceAll(displayFixture302, "   drm_connector_id:", "   DRM connector: card0-DP-1\n   drm_connector_id:")} {
		devices, err := parseDisplays(CommandOutput{Stdout: text})
		if err != nil || len(devices) != 1 || devices[0] != fixtureDevice() {
			t.Fatalf("3.0.2源码格式应复用既有解析：%+v %v", devices, err)
		}
	}
	for _, text := range []string{
		strings.ReplaceAll(displayFixture302, "   drm_connector_id: -1\n", ""),
		strings.ReplaceAll(displayFixture302, "drm_connector_id: -1", "drm_connector_id: -2"),
		strings.ReplaceAll(displayFixture302, "drm_connector_id: -1", "drm_connector_id: unknown"),
		strings.ReplaceAll(displayFixture302, "   Monitor:", "   DRM connector: card0/DP-1\n   Monitor:"),
		displayFixture302 + "   Future field: value\n",
		displayFixture302 + "   drm_connector_id: 95\n",
	} {
		devices, err := parseDisplays(CommandOutput{Stdout: text})
		if model.CodeOf(err) != model.CodeTransport || len(devices) != 0 {
			t.Fatalf("异常3.0.2字段不得返回部分设备：%+v %v", devices, err)
		}
	}
}

func TestParseDisplaysKeepsSeparateBusesAndMissingSerials(t *testing.T) {
	text := strings.ReplaceAll(displayFixture, ":SERIAL7", ":") + "\n" +
		"Display 2\n I2C bus: /dev/i2c-8\n drm_connector_id: 0\n Monitor: DEL:DELL P2411H:\n"
	devices, err := parseDisplays(CommandOutput{Stdout: text})
	if err != nil || len(devices) != 2 {
		t.Fatalf("相同型号及空序列号仍须保留两个候选：%+v, %v", devices, err)
	}
	if devices[0].BackendID != "i2c-7" || devices[1].BackendID != "i2c-8" || devices[0].Identity.Path != "/dev/i2c-7" {
		t.Fatalf("设备必须按总线标识而非易变显示序号选择：%+v", devices)
	}
	if devices[0].Identity.Manufacturer != "DEL" || devices[0].Identity.Serial != "" || devices[0].Name != "DELL P2411H" || devices[0].Identity.ProductCode != "" || devices[0].Identity.EDIDSHA256 != "" {
		t.Fatalf("只填实际取得的身份字段，不能把型号冒充产品代码或EDID：%+v", devices[0])
	}
}

func TestParseDisplaysRejectsAmbiguousAndMalformedOutput(t *testing.T) {
	tests := []struct {
		name string
		text string
		code model.ErrorCode
	}{
		{"empty", "", model.CodeTransport},
		{"unknown_line", displayFixture + "unrecognized output\n", model.CodeTransport},
		{"missing_bus", strings.ReplaceAll(displayFixture, "   I2C bus: /dev/i2c-7\n", ""), model.CodeTransport},
		{"missing_monitor", strings.ReplaceAll(displayFixture, "   Monitor: DEL:DELL P2411H:SERIAL7\n", ""), model.CodeTransport},
		{"missing_connector_id", strings.ReplaceAll(displayFixture, "   drm_connector_id: 95\n", ""), model.CodeTransport},
		{"bad_bus", strings.ReplaceAll(displayFixture, "/dev/i2c-7", "/dev/i2c-7;reboot"), model.CodeTransport},
		{"large_bus", strings.ReplaceAll(displayFixture, "/dev/i2c-7", "/dev/i2c-256"), model.CodeTransport},
		{"duplicate_bus", displayFixture + strings.ReplaceAll(displayFixture, "Display 1", "Display 2"), model.CodeIdentityAmbiguous},
		{"duplicate_ordinal", displayFixture + strings.ReplaceAll(displayFixture, "i2c-7", "i2c-8"), model.CodeIdentityAmbiguous},
		{"duplicate_field", strings.ReplaceAll(displayFixture, "   Monitor:", "   I2C bus: /dev/i2c-9\n   Monitor:"), model.CodeTransport},
		{"bad_monitor", strings.ReplaceAll(displayFixture, "DEL:DELL P2411H:SERIAL7", "DEL:missing-delimiter"), model.CodeTransport},
		{"nul", displayFixture + "\x00", model.CodeTransport},
		{"partial_empty_marker", displayFixture + "No displays found.\n", model.CodeTransport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			devices, err := parseDisplays(CommandOutput{Stdout: tt.text})
			if model.CodeOf(err) != tt.code || len(devices) != 0 {
				t.Fatalf("异常枚举不得返回部分设备：devices=%+v err=%v want=%s", devices, err, tt.code)
			}
		})
	}
}

func TestParseDisplaysExplicitEmptyAndInvalidBlocks(t *testing.T) {
	devices, err := parseDisplays(CommandOutput{Stdout: "No displays found.\n"})
	if err != nil || len(devices) != 0 {
		t.Fatalf("明确无设备应为空列表：%+v %v", devices, err)
	}
	invalid := "Invalid display\n I2C bus: /dev/i2c-8\n drm_connector_id: 0\n Monitor: DEL:DELL P2411H:SERIAL8\n"
	devices, err = parseDisplays(CommandOutput{Stdout: displayFixture + "\n" + invalid})
	if err != nil || len(devices) != 1 || devices[0].BackendID != "i2c-7" {
		t.Fatalf("无效DDC对象不得被当作可操作显示器：%+v %v", devices, err)
	}
	devices, err = parseDisplays(CommandOutput{Stdout: invalid})
	if err != nil || len(devices) != 0 {
		t.Fatalf("仅有明确无效对象时不得虚构设备：%+v %v", devices, err)
	}
}

func TestParseInputUsesHexAndRejectsUnknownStatus(t *testing.T) {
	for text, want := range map[string]uint16{"VCP 60 SNC x11\n": 17, "VCP 60 SNC x0f\n": 15, "VCP 60 SNC xFFFF\n": 65535} {
		value, err := parseInput(CommandOutput{Stdout: text})
		if err != nil || value != want {
			t.Fatalf("SNC值按十六进制解码：%q => %d %v", text, value, err)
		}
	}
	for _, text := range []string{"", "VCP 60 ERR\n", "VCP 60 C 17 100\n", "VCP 61 SNC x11\n", "VCP 60 SNC 17\n", "VCP 60 SNC x00\n", "VCP 60 SNC x10000\n", "VCP 60\nSNC x11\n", "VCP 60 SNC x11\nVCP 60 SNC x12\n"} {
		if _, err := parseInput(CommandOutput{Stdout: text}); model.CodeOf(err) != model.CodeTransport {
			t.Fatalf("未知/异常读回不能宣称成功或不支持：%q %v", text, err)
		}
	}
}
