//go:build darwin

package loginstart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/orzlee/displayKvm/internal/settings"
)

func writeUnifiedProfileDocument(t *testing.T, f *loginFixture, tool string) ServiceProfile {
	t.Helper()
	document := settings.NewDocument()
	document.Runtime.ToolPath = tool
	var encoded bytes.Buffer
	if err := settings.Encode(&encoded, document); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(f.profile.ConfigPath), "settings.json")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return ServiceProfile{Version: 2, ConfigPath: path}
}

func TestUnifiedSettingsProfileV2StrictEncodingAndReadback(t *testing.T) {
	f := newLoginFixture(t)
	profile := writeUnifiedProfileDocument(t, f, f.tool)
	sourceBefore, err := os.ReadFile(profile.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.manager.SaveProfile(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	requireLoginBool(t, result.Saved, true)
	data, err := os.ReadFile(f.manager.profilePath())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 2 || string(fields["version"]) != "2" {
		t.Fatalf("v2磁盘形状不是version/config_path两个字段: %s %v", data, err)
	}
	var configPath string
	if err := json.Unmarshal(fields["config_path"], &configPath); err != nil || configPath != profile.ConfigPath {
		t.Fatalf("v2选定路径未保持: %s %v", data, err)
	}
	for _, load := range []func() (ServiceProfile, error){
		func() (ServiceProfile, error) { return f.manager.Profile(context.Background()) },
		func() (ServiceProfile, error) { return LoadServiceProfile(f.manager.profilePath()) },
	} {
		got, err := load()
		if err != nil || got != profile {
			t.Fatalf("v2读取改变了公共字段或选定位置: %+v %v", got, err)
		}
	}
	sourceAfter, err := os.ReadFile(profile.ConfigPath)
	if err != nil || !bytes.Equal(sourceBefore, sourceAfter) {
		t.Fatal("保存启动选择修改了完整文档")
	}
	info, err := os.Stat(f.manager.profilePath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("v2档案未保留0600权限: %v %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(f.env.HomeDir, "Library")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("保存启动选择创建了注册目录")
	}
	for _, call := range f.calls {
		if call[1] != "print" && call[1] != "print-disabled" {
			t.Fatalf("保存启动选择触发了系统写操作: %v", call)
		}
	}
}

func TestUnifiedSettingsProfileV2RejectsLegacyKeysAndBadStructure(t *testing.T) {
	valid := `{"version":2,"config_path":"/settings.json"}`
	if profile, err := decodeProfile([]byte(valid)); err != nil || profile != (ServiceProfile{Version: 2, ConfigPath: "/settings.json"}) {
		t.Fatalf("合法v2结构读取失败: %+v %v", profile, err)
	}
	for _, text := range []string{
		`{"version":2,"config_path":"/settings.json","rules_path":""}`,
		`{"version":2,"config_path":"/settings.json","schedules_path":""}`,
		`{"version":2,"config_path":"/settings.json","tool_path":""}`,
		`{"version":2,"config_path":"/settings.json","rules_path":"/rules.json"}`,
		`{"version":2,"config_path":"/settings.json","schedules_path":"/schedules.json"}`,
		`{"version":2,"config_path":"/settings.json","tool_path":"/tool"}`,
		`{"version":2,"config_path":"/settings.json","tool_path":null}`,
		`{"version":2,"config_path":"/settings.json","extra":true}`,
		`{"version":2,"config_path":"/a","config_path":"/b"}`,
		`{"version":2,"config_path":null}`,
		`{"version":null,"config_path":"/settings.json"}`,
		`{"version":2,"config_path":""}`,
		`{"version":2,"config_path":"relative.json"}`,
		`{"version":2,"config_path":"/a/../settings.json"}`,
		`{"version":2}`,
		`{"config_path":"/settings.json"}`,
		`{"version":3,"config_path":"/settings.json"}`,
		valid + `{}`,
		`null`,
	} {
		if _, err := decodeProfile([]byte(text)); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Errorf("坏v2结构被接受或误判为未存在: %s %v", text, err)
		}
	}
}

func TestUnifiedSettingsProfileRejectsV1WithoutChangingSource(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	for _, target := range []Target{Service, Desktop} {
		if _, err := f.manager.Enable(context.Background(), target, f.app); err != nil {
			t.Fatal(err)
		}
	}
	legacy, err := json.Marshal(map[string]any{"version": 1, "config_path": f.profile.ConfigPath, "rules_path": "", "schedules_path": "", "tool_path": f.tool})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.manager.profilePath(), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	for _, load := range []func() (ServiceProfile, error){
		func() (ServiceProfile, error) { return LoadServiceProfile(f.manager.profilePath()) },
		func() (ServiceProfile, error) { return f.manager.Profile(context.Background()) },
	} {
		if _, err := load(); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("旧档案被接受或误当作首次缺失: %v", err)
		}
	}
	for _, candidate := range []ServiceProfile{f.profile, {Version: 1, ConfigPath: f.profile.ConfigPath}} {
		result, err := f.manager.SaveProfile(context.Background(), candidate)
		if err == nil {
			t.Fatal("普通保存覆盖或重新写入了旧档案")
		}
		requireLoginBool(t, result.Saved, false)
	}
	result, err := f.manager.Enable(context.Background(), Service, f.app)
	if err == nil {
		t.Fatal("服务启用接受旧档案")
	}
	requireLoginBool(t, result.Saved, false)
	result, err = f.manager.Disable(context.Background(), Service)
	if err == nil || result.Status.Profile != nil || result.Status.ProfileMessage == "" {
		t.Fatalf("关闭后的状态查询吞掉了旧档案错误: %+v %v", result, err)
	}
	requireLoginBool(t, result.Saved, true)
	requireLoginBool(t, result.Status.Service.Present, false)
	requireLoginBool(t, result.Status.Desktop.Present, true)
	after, readErr := os.ReadFile(f.manager.profilePath())
	if readErr != nil || !bytes.Equal(legacy, after) {
		t.Fatalf("拒旧或禁用操作迁移了原件: %s %v", after, readErr)
	}
}

func TestUnifiedSettingsProfileV2RequiresReadableCompleteDocument(t *testing.T) {
	for _, kind := range []string{"broken-document", "legacy-configuration", "missing-document"} {
		t.Run(kind, func(t *testing.T) {
			f := newLoginFixture(t)
			if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(f.manager.profilePath())
			if err != nil {
				t.Fatal(err)
			}
			profile := writeUnifiedProfileDocument(t, f, f.tool)
			switch kind {
			case "broken-document":
				if err := os.WriteFile(profile.ConfigPath, []byte(`{"kind":"displaykvm-settings"`), 0600); err != nil {
					t.Fatal(err)
				}
			case "legacy-configuration":
				if err := os.WriteFile(profile.ConfigPath, []byte(`{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-document":
				if err := os.Remove(profile.ConfigPath); err != nil {
					t.Fatal(err)
				}
			}
			result, err := f.manager.SaveProfile(context.Background(), profile)
			if err == nil {
				t.Fatal("不可读或损坏的完整文档被保存为启动选择")
			}
			requireLoginBool(t, result.Saved, false)
			after, readErr := os.ReadFile(f.manager.profilePath())
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("文档验证失败仍改变旧档案")
			}
			if result.Status.Profile == nil || *result.Status.Profile != f.profile {
				t.Fatalf("候选文档错误被当作档案不存在: %+v", result.Status)
			}
		})
	}
}

func TestUnifiedSettingsProfileV2SeparatesLoadEnableAndDisable(t *testing.T) {
	for _, kind := range []string{"empty-tool", "broken-document", "missing-document"} {
		t.Run(kind, func(t *testing.T) {
			f := newLoginFixture(t)
			profile := writeUnifiedProfileDocument(t, f, f.tool)
			if _, err := f.manager.SaveProfile(context.Background(), profile); err != nil {
				t.Fatal(err)
			}
			for _, target := range []Target{Service, Desktop} {
				if _, err := f.manager.Enable(context.Background(), target, f.app); err != nil {
					t.Fatal(err)
				}
			}
			profileBefore, err := os.ReadFile(f.manager.profilePath())
			if err != nil {
				t.Fatal(err)
			}
			desktopBefore, err := os.ReadFile(f.manager.agentPath(Desktop))
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "empty-tool":
				writeUnifiedProfileDocument(t, f, "")
			case "broken-document":
				if err := os.WriteFile(profile.ConfigPath, []byte(`{"invalid":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-document":
				if err := os.Remove(profile.ConfigPath); err != nil {
					t.Fatal(err)
				}
			}
			got, err := LoadServiceProfile(f.manager.profilePath())
			if err != nil || got != profile {
				t.Fatalf("业务文档就绪变化污染了私有档案结构读取: %+v %v", got, err)
			}
			status, err := f.manager.Status(context.Background())
			if err != nil || status.Profile == nil || *status.Profile != profile {
				t.Fatalf("未就绪文档被当作未知或缺失档案: %+v %v", status, err)
			}
			requireLoginBool(t, status.Service.Enabled, true)
			requireLoginBool(t, status.Service.Ready, false)
			requireLoginBool(t, status.Desktop.Enabled, true)
			requireLoginBool(t, status.Desktop.Ready, true)
			result, err := f.manager.Enable(context.Background(), Service, f.app)
			if err == nil {
				t.Fatal("服务重新登记未再次验证完整文档和工具")
			}
			requireLoginBool(t, result.Saved, false)
			f.calls = nil
			result, err = f.manager.Disable(context.Background(), Service)
			if err != nil {
				t.Fatalf("坏配置阻止关闭服务登录项: %v", err)
			}
			requireLoginBool(t, result.Saved, true)
			requireLoginBool(t, result.Status.Service.Enabled, false)
			requireLoginBool(t, result.Status.Desktop.Enabled, true)
			profileAfter, profileErr := os.ReadFile(f.manager.profilePath())
			desktopAfter, desktopErr := os.ReadFile(f.manager.agentPath(Desktop))
			if profileErr != nil || desktopErr != nil || !bytes.Equal(profileBefore, profileAfter) || !bytes.Equal(desktopBefore, desktopAfter) {
				t.Fatal("关闭修改了档案或独立桌面任务")
			}
			for _, call := range f.calls {
				if call[1] != "print" && call[1] != "print-disabled" {
					t.Fatalf("关闭调用了系统写命令: %v", call)
				}
			}
		})
	}
}

func TestUnifiedSettingsProfileV2BadProfileAndUnknownQueryStayDistinct(t *testing.T) {
	f := newLoginFixture(t)
	profile := writeUnifiedProfileDocument(t, f, f.tool)
	if _, err := f.manager.Profile(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("首次缺失丢失os.ErrNotExist错误链: %v", err)
	}
	if _, err := f.manager.SaveProfile(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	for _, target := range []Target{Service, Desktop} {
		if _, err := f.manager.Enable(context.Background(), target, f.app); err != nil {
			t.Fatal(err)
		}
	}
	f.queryError = true
	status, err := f.manager.Status(context.Background())
	if err == nil || status.Profile == nil || *status.Profile != profile || status.Service.Enabled != nil || status.Desktop.Enabled != nil || status.Service.Loaded != nil || status.Desktop.Loaded != nil {
		t.Fatalf("查询失败丢失已知档案或伪装关闭: %+v %v", status, err)
	}
	f.queryError = false
	bad, err := json.Marshal(map[string]any{"version": 2, "config_path": profile.ConfigPath, "tool_path": ""})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.manager.profilePath(), bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Profile(context.Background()); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("坏v2档案被当作首次缺失: %v", err)
	}
	result, err := f.manager.SaveProfile(context.Background(), profile)
	if err == nil {
		t.Fatal("保存覆盖了结构不明的v2档案")
	}
	requireLoginBool(t, result.Saved, false)
	after, readErr := os.ReadFile(f.manager.profilePath())
	if readErr != nil || !bytes.Equal(bad, after) || result.Status.Profile != nil || result.Status.ProfileMessage == "" {
		t.Fatalf("坏档案未保留并明确报告: %s %+v %v", after, result.Status, readErr)
	}
}
