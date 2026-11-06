//go:build darwin

package loginstart

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/settings"
)

func writeSettingsProductDocument(t *testing.T, f *loginFixture, language string, paths controltools.Paths) ServiceProfile {
	t.Helper()
	document := settings.NewDocument()
	document.Desktop.Language = language
	document.Runtime = paths
	var encoded bytes.Buffer
	if err := settings.Encode(&encoded, document); err != nil {
		t.Fatal(err)
	}
	profile := ServiceProfile{Version: 2, ConfigPath: filepath.Join(filepath.Dir(f.profile.ConfigPath), "product-settings.json")}
	if err := os.WriteFile(profile.ConfigPath, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestSettingsProductSaveSelectsDocumentWithoutToolReadiness(t *testing.T) {
	for _, kind := range []string{"bundled-default", "missing-override", "non-executable-override", "directory-override"} {
		t.Run(kind, func(t *testing.T) {
			f := newLoginFixture(t)
			paths := controltools.Paths{}
			switch kind {
			case "missing-override":
				paths.ToolPath = filepath.Join(f.env.HomeDir, "absent-tool")
			case "non-executable-override":
				paths.ToolPath = f.tool
				if err := os.Chmod(f.tool, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory-override":
				paths.ToolPath = f.env.HomeDir
			}
			profile := writeSettingsProductDocument(t, f, "en", paths)
			before, err := os.ReadFile(profile.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(f.app); err != nil {
				t.Fatal(err)
			}
			result, err := f.manager.SaveProfile(context.Background(), profile)
			if err != nil || result.Status.Profile == nil || *result.Status.Profile != profile {
				t.Fatalf("设置选择被应用或工具就绪阻断: %+v %v", result, err)
			}
			requireLoginBool(t, result.Saved, true)
			after, err := os.ReadFile(profile.ConfigPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("保存选择改写了语言或工具设置")
			}
			file, err := os.Open(profile.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			document, loadErr := settings.Load(file)
			if err := errors.Join(loadErr, file.Close()); err != nil || document.Desktop.Language != "en" || document.Runtime != paths {
				t.Fatalf("完整设置的语言或原始override丢失: %+v %v", document, err)
			}
			for _, call := range f.calls {
				if call[0] != "/bin/launchctl" || (call[1] != "print" && call[1] != "print-disabled") {
					t.Fatalf("保存选择执行了工具或系统写命令: %v", call)
				}
			}
			if _, err := os.Stat(filepath.Join(f.env.HomeDir, "Library")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("保存设置选择登记了登录任务: %v", err)
			}
		})
	}
}

func TestSettingsProductEnableAndReadyUseSelectedDaemonResolver(t *testing.T) {
	f := newLoginFixture(t)
	profile := writeSettingsProductDocument(t, f, "zh-CN", controltools.Paths{})
	tool := filepath.Join(f.app, "Contents", "MacOS", "m1ddc")
	marker := filepath.Join(f.env.HomeDir, "tool-was-executed")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n: > '"+marker+"'\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(profile.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := settings.Load(bytes.NewReader(before))
	if err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(f.app, "Contents", "MacOS", "displaykvmd")
	resolved, err := document.ValidateForService(controltools.Resolver{GOOS: "darwin", Executable: daemon})
	if err != nil || resolved.ToolPath != tool {
		t.Fatalf("daemon实际位置未定位同包控制工具: %+v %v", resolved, err)
	}
	if _, err := f.manager.SaveProfile(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	result, err := f.manager.Enable(context.Background(), Service, f.app)
	if err != nil {
		t.Fatalf("登录服务未复用daemon定位规则: %+v %v", result, err)
	}
	requireLoginBool(t, result.Saved, true)
	requireLoginBool(t, result.Status.Service.Ready, true)
	requireLoginBool(t, result.Status.Service.Loaded, false)
	status, err := f.manager.Status(context.Background())
	if err != nil || status.Service.AppPath != f.app {
		t.Fatalf("状态未保留所选应用: %+v %v", status, err)
	}
	requireLoginBool(t, status.Service.Ready, true)
	after, err := os.ReadFile(profile.ConfigPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("工具解析把安装绝对路径写回了文档")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("就绪检查执行了控制工具: %v", err)
	}
}

func TestSettingsProductMissingToolsDoNotBlockSaveDisableOrDesktop(t *testing.T) {
	for _, kind := range []string{"missing", "not-executable", "symlink", "missing-override"} {
		t.Run(kind, func(t *testing.T) {
			f := newLoginFixture(t)
			profile := writeSettingsProductDocument(t, f, "system", controltools.Paths{})
			tool := filepath.Join(f.app, "Contents", "MacOS", "m1ddc")
			if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := f.manager.SaveProfile(context.Background(), profile); err != nil {
				t.Fatal(err)
			}
			for _, target := range []Target{Service, Desktop} {
				if _, err := f.manager.Enable(context.Background(), target, f.app); err != nil {
					t.Fatal(err)
				}
			}
			paths := controltools.Paths{}
			switch kind {
			case "missing":
				if err := os.Remove(tool); err != nil {
					t.Fatal(err)
				}
			case "not-executable":
				if err := os.Chmod(tool, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(tool); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.tool, tool); err != nil {
					t.Fatal(err)
				}
			case "missing-override":
				paths.ToolPath = filepath.Join(f.env.HomeDir, "absent-override")
			}
			writeSettingsProductDocument(t, f, "en", paths)
			before, err := os.ReadFile(profile.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.manager.SaveProfile(context.Background(), profile)
			if err != nil {
				t.Fatalf("服务未就绪阻断了语言文档选择: %+v %v", result, err)
			}
			requireLoginBool(t, result.Saved, true)
			requireLoginBool(t, result.Status.Service.Enabled, true)
			requireLoginBool(t, result.Status.Service.Ready, false)
			requireLoginBool(t, result.Status.Desktop.Ready, true)
			result, err = f.manager.Enable(context.Background(), Service, f.app)
			if err == nil {
				t.Fatal("无效控制工具仍允许登记服务")
			}
			requireLoginBool(t, result.Saved, false)
			if _, err := f.manager.Enable(context.Background(), Desktop, f.app); err != nil {
				t.Fatalf("控制工具不可用阻断独立桌面登录: %v", err)
			}
			desktopBefore, err := os.ReadFile(f.manager.agentPath(Desktop))
			if err != nil {
				t.Fatal(err)
			}
			f.calls = nil
			result, err = f.manager.Disable(context.Background(), Service)
			if err != nil {
				t.Fatalf("缺工具阻断了安全关闭: %+v %v", result, err)
			}
			requireLoginBool(t, result.Saved, true)
			requireLoginBool(t, result.Status.Service.Present, false)
			requireLoginBool(t, result.Status.Desktop.Ready, true)
			after, readErr := os.ReadFile(profile.ConfigPath)
			desktopAfter, desktopErr := os.ReadFile(f.manager.agentPath(Desktop))
			if readErr != nil || desktopErr != nil || !bytes.Equal(before, after) || !bytes.Equal(desktopBefore, desktopAfter) {
				t.Fatal("关闭服务改写了文档或独立桌面任务")
			}
			for _, call := range f.calls {
				if call[0] != "/bin/launchctl" || (call[1] != "print" && call[1] != "print-disabled") {
					t.Fatalf("关闭触发了进程或工具操作: %v", call)
				}
			}
		})
	}
}

func TestSettingsProductMovingApplicationUsesNewBundledLocation(t *testing.T) {
	f := newLoginFixture(t)
	profile := writeSettingsProductDocument(t, f, "en", controltools.Paths{})
	tool := filepath.Join(f.app, "Contents", "MacOS", "m1ddc")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.SaveProfile(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(profile.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := filepath.Join(f.app, "Contents", "MacOS", "displaykvmd")
	movedHome := filepath.Join(filepath.Dir(f.env.HomeDir), "moved-home")
	if err := os.Rename(f.env.HomeDir, movedHome); err != nil {
		t.Fatal(err)
	}
	f.env.HomeDir = movedHome
	f.app = filepath.Join(movedHome, "Applications", "DisplayKvm.app")
	f.manager, err = New(Options{Environment: func() (Environment, error) { return f.env, nil }, Run: f.run})
	if err != nil {
		t.Fatal(err)
	}
	document, err := settings.Load(bytes.NewReader(before))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := document.ValidateForService(controltools.Resolver{GOOS: "darwin", Executable: oldDaemon}); err == nil {
		t.Fatal("应用移动后仍把旧绝对工具位置当作就绪")
	}
	daemon := filepath.Join(f.app, "Contents", "MacOS", "displaykvmd")
	resolved, err := document.ValidateForService(controltools.Resolver{GOOS: "darwin", Executable: daemon})
	if err != nil || resolved.ToolPath != filepath.Join(f.app, "Contents", "MacOS", "m1ddc") {
		t.Fatalf("移动后的daemon未使用同包工具: %+v %v", resolved, err)
	}
	result, err := f.manager.Enable(context.Background(), Service, f.app)
	if err != nil {
		t.Fatalf("新稳定位置不能登记: %+v %v", result, err)
	}
	requireLoginBool(t, result.Status.Service.Ready, true)
	registeredApp, _, err := f.manager.readAgent(Service)
	if err != nil || registeredApp != f.app {
		t.Fatalf("登录项仍指向旧应用: %s %v", registeredApp, err)
	}
	after, err := os.ReadFile(profile.ConfigPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("移动应用导致配置写入安装绝对路径")
	}
}

func TestSettingsProductSavePreservesConfirmedWriteWithUnknownStatus(t *testing.T) {
	f := newLoginFixture(t)
	profile := writeSettingsProductDocument(t, f, "en", controltools.Paths{})
	f.queryError = true
	result, err := f.manager.SaveProfile(context.Background(), profile)
	if err == nil {
		t.Fatal("状态查询失败被静默忽略")
	}
	requireLoginBool(t, result.Saved, true)
	if result.Status.Profile == nil || *result.Status.Profile != profile || result.Status.Service.Loaded != nil || result.Status.Desktop.Loaded != nil {
		t.Fatalf("后续查询失败丢失已确认选择或伪造加载状态: %+v", result)
	}
	got, loadErr := f.manager.Profile(context.Background())
	if loadErr != nil || got != profile {
		t.Fatalf("已确认写入与实际档案不符: %+v %v", got, loadErr)
	}
}

func TestSettingsProductBadDocumentDoesNotReplaceSelectionOrLanguage(t *testing.T) {
	for _, kind := range []string{"broken", "invalid-language", "missing"} {
		t.Run(kind, func(t *testing.T) {
			f := newLoginFixture(t)
			if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
				t.Fatal(err)
			}
			selectedBefore, err := os.ReadFile(f.manager.profilePath())
			if err != nil {
				t.Fatal(err)
			}
			profile := writeSettingsProductDocument(t, f, "en", controltools.Paths{})
			switch kind {
			case "broken":
				if err := os.WriteFile(profile.ConfigPath, []byte(`{"desktop":`), 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid-language":
				data, err := os.ReadFile(profile.ConfigPath)
				if err != nil {
					t.Fatal(err)
				}
				// 绕过正常编码器构造外部坏文件，确保保存入口也核对语言结构，
				// 不能只依赖页面选项或把非法语言静默回退后覆盖原文档。
				data = bytes.Replace(data, []byte(`"language": "en"`), []byte(`"language": "invalid"`), 1)
				if !bytes.Contains(data, []byte(`"invalid"`)) {
					t.Fatal("无效语言夹具未修改实际文档")
				}
				if err := os.WriteFile(profile.ConfigPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(profile.ConfigPath); err != nil {
					t.Fatal(err)
				}
			}
			candidateBefore, candidateErr := os.ReadFile(profile.ConfigPath)
			result, err := f.manager.SaveProfile(context.Background(), profile)
			if err == nil || result.Status.Profile == nil || *result.Status.Profile != f.profile {
				t.Fatalf("坏文档被选中或丢失原选择: %+v %v", result, err)
			}
			requireLoginBool(t, result.Saved, false)
			selectedAfter, readErr := os.ReadFile(f.manager.profilePath())
			if readErr != nil || !bytes.Equal(selectedBefore, selectedAfter) {
				t.Fatal("坏文档导致已确认档案发生变化")
			}
			candidateAfter, afterErr := os.ReadFile(profile.ConfigPath)
			if kind == "missing" {
				if !errors.Is(candidateErr, os.ErrNotExist) || !errors.Is(afterErr, os.ErrNotExist) {
					t.Fatal("缺失文档被默认内容覆盖")
				}
			} else if candidateErr != nil || afterErr != nil || !bytes.Equal(candidateBefore, candidateAfter) {
				t.Fatal("损坏文档被自动修复或丢失原始语言")
			}
		})
	}
}
