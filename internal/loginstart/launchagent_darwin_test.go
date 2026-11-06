//go:build darwin

package loginstart

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/orzlee/displayKvm/internal/settings"
)

type loginFixture struct {
	env         Environment
	manager     *Manager
	profile     ServiceProfile
	tool        string
	app         string
	disabled    map[Target]bool
	loaded      map[Target]bool
	calls       [][]string
	queryError  bool
	enableError bool
}

var loginProgramFixtures struct {
	sync.Once
	programs map[string][]byte
	err      error
}

func realLoginPrograms(t *testing.T) map[string][]byte {
	t.Helper()
	loginProgramFixtures.Do(func() {
		// 构建产物只保存字节，不缓存首个测试的临时路径；该测试清理目录后，
		// 同进程其余用例仍可复制真实Go入口样本，且不必反复编译或执行程序。
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/orzlee/displayKvm\n\ngo 1.20\n"), 0600); err != nil {
			loginProgramFixtures.err = err
			return
		}
		for _, directory := range []string{"desktop", "cmd/displaykvmd", "cmd/displaykvm", "bin"} {
			if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
				loginProgramFixtures.err = err
				return
			}
			if directory != "bin" {
				if err := os.WriteFile(filepath.Join(root, directory, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0600); err != nil {
					loginProgramFixtures.err = err
					return
				}
			}
		}
		command := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-o", filepath.Join(root, "bin"), "./desktop", "./cmd/displaykvmd", "./cmd/displaykvm")
		command.Dir = root
		// 样本没有第三方依赖；固定使用运行测试的本地工具链及现有缓存，
		// 关闭工作区、代理与自动工具链下载，避免验证时读写真实工程或访问网络。
		overrides := map[string]string{"GOWORK": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOVCS": "*:off", "GOFLAGS": "", "GOOS": runtime.GOOS, "GOARCH": runtime.GOARCH, "CGO_ENABLED": "0"}
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			if _, replaced := overrides[name]; !replaced {
				command.Env = append(command.Env, entry)
			}
		}
		for name, value := range overrides {
			command.Env = append(command.Env, name+"="+value)
		}
		if output, err := command.CombinedOutput(); err != nil {
			loginProgramFixtures.err = fmt.Errorf("离线构建Go入口样本失败: %w\n%s", err, output)
			return
		}
		loginProgramFixtures.programs = make(map[string][]byte)
		for name, output := range map[string]string{"displaykvm": "desktop", "displaykvmd": "displaykvmd", "displaykvm-cli": "displaykvm"} {
			data, err := os.ReadFile(filepath.Join(root, "bin", output))
			if err != nil {
				loginProgramFixtures.err = err
				return
			}
			loginProgramFixtures.programs[name] = data
		}
	})
	if loginProgramFixtures.err != nil {
		t.Fatal(loginProgramFixtures.err)
	}
	return loginProgramFixtures.programs
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &loginFixture{env: Environment{ConfigDir: filepath.Join(root, "config"), HomeDir: filepath.Join(root, "home"), UID: os.Getuid()}, disabled: map[Target]bool{}, loaded: map[Target]bool{}}
	for _, dir := range []string{f.env.ConfigDir, f.env.HomeDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.profile = ServiceProfile{Version: 2, ConfigPath: filepath.Join(root, "config.json")}
	f.tool = filepath.Join(root, "tool")
	document := settings.NewDocument()
	document.Runtime.ToolPath = f.tool
	var config bytes.Buffer
	if err := settings.Encode(&config, document); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.profile.ConfigPath, config.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.tool, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.app = filepath.Join(f.env.HomeDir, "Applications", "DisplayKvm.app")
	if err := os.MkdirAll(filepath.Join(f.app, "Contents", "MacOS"), 0700); err != nil {
		t.Fatal(err)
	}
	info := `<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>io.github.orzlee.displaykvm</string><key>CFBundleExecutable</key><string>displaykvm</string><key>CFBundlePackageType</key><string>APPL</string><key>DisplayKVMStartupVersion</key><string>1</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(f.app, "Contents", "Info.plist"), []byte(info), 0600); err != nil {
		t.Fatal(err)
	}
	for name, program := range realLoginPrograms(t) {
		if err := os.WriteFile(filepath.Join(f.app, "Contents", "MacOS", name), program, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.manager, err = New(Options{Environment: func() (Environment, error) { return f.env, nil }, Run: f.run})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *loginFixture) run(_ context.Context, command string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{command}, args...))
	if command != "/bin/launchctl" || len(args) == 0 {
		return nil, errors.New("不允许的命令")
	}
	if args[0] == "print-disabled" {
		if f.queryError {
			return []byte("permission denied"), errors.New("query denied")
		}
		return []byte(fmt.Sprintf("disabled services = {\n\t%q => %t\n\t%q => %t\n}\n", label(Service), f.disabled[Service], label(Desktop), f.disabled[Desktop])), nil
	}
	for _, target := range []Target{Service, Desktop} {
		service := fmt.Sprintf("gui/%d/%s", f.env.UID, label(target))
		if len(args) != 2 || args[1] != service {
			continue
		}
		switch args[0] {
		case "print":
			if f.queryError {
				return nil, errors.New("query denied")
			}
			if f.loaded[target] {
				return []byte(service + " = {\n\tstate = running\n}\n"), nil
			}
			return []byte(fmt.Sprintf("Could not find service %q in domain for user gui: %d\n", label(target), f.env.UID)), errors.New("absent")
		case "enable":
			if f.enableError {
				return nil, errors.New("enable denied")
			}
			f.disabled[target] = false
			return nil, nil
		}
	}
	return nil, errors.New("禁止进程启停或未知命令")
}

func requireLoginBool(t *testing.T, value *bool, want bool) {
	t.Helper()
	if value == nil || *value != want {
		t.Fatalf("状态=%v，期望=%t", value, want)
	}
}

func TestLoginStartFourIndependentCombinations(t *testing.T) {
	for _, combination := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprint(combination), func(t *testing.T) {
			f := newLoginFixture(t)
			if result, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
				t.Fatal(err)
			} else {
				requireLoginBool(t, result.Saved, true)
			}
			for i, target := range []Target{Service, Desktop} {
				if combination[i] {
					if _, err := f.manager.Enable(context.Background(), target, f.app); err != nil {
						t.Fatal(err)
					}
				}
			}
			status, err := f.manager.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			requireLoginBool(t, status.Service.Enabled, combination[0])
			requireLoginBool(t, status.Desktop.Enabled, combination[1])
			if !reflect.DeepEqual(status.Profile, &f.profile) {
				t.Fatalf("档案未独立保存: %+v", status)
			}
			if combination[1] {
				path := filepath.Join(f.env.HomeDir, "Library", "LaunchAgents", label(Desktop)+".plist")
				before, _ := os.ReadFile(path)
				if _, err := f.manager.Disable(context.Background(), Service); err != nil {
					t.Fatal(err)
				}
				after, _ := os.ReadFile(path)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("关闭服务修改了桌面任务")
				}
			}
		})
	}
}

func TestLoginStartQueryNeverCreatesAndConfigureNeverStartsProcesses(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.manager.Profile(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("首次缺档案必须可精确识别: %v", err)
	}
	if _, err := f.manager.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(f.env.ConfigDir, "DisplayKVM"), filepath.Join(f.env.HomeDir, "Library")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("查询创建了目录: %s %v", path, err)
		}
	}
	before, _ := os.ReadFile(f.profile.ConfigPath)
	if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.profile.ConfigPath)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("保存修改了业务内容")
	}
	for _, call := range f.calls {
		if call[1] != "print" && call[1] != "print-disabled" {
			t.Fatalf("配置调用了写系统命令: %v", call)
		}
	}
	if _, err := os.Stat(filepath.Join(f.env.HomeDir, "Library")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("配置创建了登录注册")
	}
	profile := filepath.Join(f.env.ConfigDir, "DisplayKVM", "startup.json")
	info, err := os.Stat(profile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("档案非0600: %v %v", info, err)
	}
}

func TestLoginStartBadProfileUnknownAndRefusesOverwrite(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.env.ConfigDir, "DisplayKVM", "startup.json")
	bad := []byte(`{"unrelated":true}`)
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	status, err := f.manager.Status(context.Background())
	if err == nil || status.Profile != nil || status.ProfileMessage == "" {
		t.Fatalf("坏档案未说明: %+v %v", status, err)
	}
	if _, err := f.manager.Profile(context.Background()); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("坏档案被当作首次: %v", err)
	}
	result, err := f.manager.SaveProfile(context.Background(), f.profile)
	if err == nil {
		t.Fatal("覆盖了坏档案")
	}
	requireLoginBool(t, result.Saved, false)
	after, _ := os.ReadFile(path)
	if !reflect.DeepEqual(after, bad) {
		t.Fatal("坏档案被改写")
	}
}

func TestLoginStartExternalStateAndDisappearingApp(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Enable(context.Background(), Service, f.app); err != nil {
		t.Fatal(err)
	}
	f.disabled[Service] = true
	f.loaded[Service] = true
	status, err := f.manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireLoginBool(t, status.Service.Present, true)
	requireLoginBool(t, status.Service.Enabled, false)
	requireLoginBool(t, status.Service.Loaded, true)
	if _, err := f.manager.Enable(context.Background(), Service, f.app); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.app, "Contents", "MacOS", "displaykvmd")); err != nil {
		t.Fatal(err)
	}
	status, err = f.manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireLoginBool(t, status.Service.Enabled, true)
	requireLoginBool(t, status.Service.Ready, false)
	if status.Service.AppPath != f.app {
		t.Fatal("丢失路径被猜成新位置")
	}
	path := filepath.Join(f.env.HomeDir, "Library", "LaunchAgents", label(Service)+".plist")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	status, err = f.manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireLoginBool(t, status.Service.Enabled, false)
	requireLoginBool(t, status.Service.Loaded, true)
}

func TestLoginStartUnknownForeignSymlinkAndOwnerProtection(t *testing.T) {
	for _, kind := range []string{"foreign", "symlink", "permissions", "owner"} {
		t.Run(kind, func(t *testing.T) {
			f := newLoginFixture(t)
			if _, err := f.manager.Enable(context.Background(), Desktop, f.app); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.env.HomeDir, "Library", "LaunchAgents", label(Desktop)+".plist")
			switch kind {
			case "foreign":
				if err := os.WriteFile(path, []byte(`<plist><dict><key>Label</key><string>other</string></dict></plist>`), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.profile.ConfigPath, path); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			case "owner":
				env := f.env
				env.UID++
				var err error
				f.manager, err = New(Options{Environment: func() (Environment, error) { return env, nil }, Run: f.run})
				if err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.Lstat(path)
			result, err := f.manager.Disable(context.Background(), Desktop)
			if err == nil {
				t.Fatal("删除了无法核实所属功能的文件")
			}
			requireLoginBool(t, result.Saved, false)
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("受保护文件被替换或删除")
			}
			status, _ := f.manager.Status(context.Background())
			if status.Desktop.Enabled != nil {
				t.Fatal("未知文件被误报为关闭")
			}
		})
	}
}

func TestLoginStartInvalidPathsAndProfileReferences(t *testing.T) {
	f := newLoginFixture(t)
	for _, path := range []string{"DisplayKvm.app", filepath.Dir(f.app), f.app + "/../DisplayKvm.app", "/tmp/DisplayKvm.app", "/Applications/AppTranslocation/DisplayKvm.app"} {
		if path == f.app {
			continue
		}
		if _, err := f.manager.Enable(context.Background(), Desktop, path); err == nil {
			t.Errorf("接受不稳定位置: %s", path)
		}
	}
	for _, p := range []ServiceProfile{
		{Version: 2, ConfigPath: ""},
		{Version: 2, ConfigPath: "settings.json"},
		{Version: 2, ConfigPath: filepath.Join(f.env.HomeDir, "missing.json")},
		{Version: 2, ConfigPath: f.env.HomeDir},
		{Version: 1, ConfigPath: f.profile.ConfigPath},
		{Version: 3, ConfigPath: f.profile.ConfigPath},
	} {
		if _, err := f.manager.SaveProfile(context.Background(), p); err == nil {
			t.Errorf("接受无效参数: %+v", p)
		}
	}
}

func TestLoginStartPartialFailureKeepsSavedAndUnknownState(t *testing.T) {
	f := newLoginFixture(t)
	f.queryError = true
	result, err := f.manager.SaveProfile(context.Background(), f.profile)
	if err == nil {
		t.Fatal("未报告系统查询失败")
	}
	requireLoginBool(t, result.Saved, true)
	if result.Status.Service.Loaded != nil {
		t.Fatal("查询失败冒充未加载")
	}
	f.queryError = false
	f.disabled[Desktop] = true
	f.enableError = true
	result, err = f.manager.Enable(context.Background(), Desktop, f.app)
	if err == nil {
		t.Fatal("未报告enable失败")
	}
	requireLoginBool(t, result.Saved, true)
	requireLoginBool(t, result.Status.Desktop.Present, true)
	requireLoginBool(t, result.Status.Desktop.Enabled, false)
	count := 0
	for _, call := range f.calls {
		if call[1] == "enable" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("副作用被重复发送: %d", count)
	}
}

func TestLoginStartExactLabelsAndMissingDisabledFields(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.manager.Enable(context.Background(), Desktop, f.app); err != nil {
		t.Fatal(err)
	}
	f.manager.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "print-disabled" {
			return []byte("disabled services = {\n\t\"" + label(Desktop) + ".other\" => false\n}\n"), nil
		}
		return []byte("gui/999/unrelated = {\n}\n"), nil
	}
	status, err := f.manager.Status(context.Background())
	if err == nil || status.Desktop.Enabled != nil || status.Desktop.Loaded != nil {
		t.Fatalf("其它label或缺字段被视为本项: %+v %v", status.Desktop, err)
	}
}

func TestLoginStartDisabledFormatsPreserveBooleanMeaning(t *testing.T) {
	for _, combination := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprint(combination), func(t *testing.T) {
			f := newLoginFixture(t)
			if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
				t.Fatal(err)
			}
			for _, target := range []Target{Service, Desktop} {
				if _, err := f.manager.Enable(context.Background(), target, f.app); err != nil {
					t.Fatal(err)
				}
			}
			for _, format := range []struct {
				name     string
				values   map[bool]string
				trailing string
			}{
				{"boolean", map[bool]string{false: "false", true: "true"}, ";"},
				{"word", map[bool]string{false: "enabled", true: "disabled"}, ""},
			} {
				t.Run(format.name, func(t *testing.T) {
					output := fmt.Sprintf("disabled services = {\n\t\"io.github.orzlee.displaykvm.service\" => %s%s\n\t\"io.github.orzlee.displaykvm.desktop\" => %s%s\n\t\"io.github.orzlee.displaykvm.service.other\" => enabled\n\t\"prefix.io.github.orzlee.displaykvm.desktop\" => disabled\n\t\"com.example.other.enabled\" => enabled\n\t\"com.example.other.disabled\" => disabled\n}\n", format.values[combination[0]], format.trailing, format.values[combination[1]], format.trailing)
					f.manager.run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
						if len(args) > 0 && args[0] == "print-disabled" {
							return []byte(output), nil
						}
						return f.run(ctx, command, args...)
					}
					status, err := f.manager.Status(context.Background())
					if err != nil {
						t.Fatalf("合法词值或其它标签导致完整状态查询失败: %v", err)
					}
					requireLoginBool(t, status.Service.Present, true)
					requireLoginBool(t, status.Desktop.Present, true)
					requireLoginBool(t, status.Service.Enabled, !combination[0])
					requireLoginBool(t, status.Desktop.Enabled, !combination[1])
				})
			}
		})
	}
}

func TestLoginStartDisabledMissingTargetsRemainUnknown(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(fmt.Sprint(registered), func(t *testing.T) {
			f := newLoginFixture(t)
			if registered {
				if _, err := f.manager.Enable(context.Background(), Desktop, f.app); err != nil {
					t.Fatal(err)
				}
			}
			f.manager.run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "print-disabled" {
					return []byte("disabled services = {\n\t\"io.github.orzlee.displaykvm.service.other\" => disabled\n\t\"io.github.orzlee.displaykvm.desktop.other\" => enabled\n\t\"prefix.io.github.orzlee.displaykvm.desktop\" => disabled\n}\n"), nil
				}
				return f.run(ctx, command, args...)
			}
			values, err := f.manager.disabled(context.Background())
			if err != nil || values[Service] != nil || values[Desktop] != nil {
				t.Fatalf("相似标签被当作目标或合法其它标签被拒绝: %+v %v", values, err)
			}
			status, err := f.manager.Status(context.Background())
			requireLoginBool(t, status.Service.Present, false)
			requireLoginBool(t, status.Service.Enabled, false)
			requireLoginBool(t, status.Desktop.Present, registered)
			if registered {
				if err == nil || status.Desktop.Enabled != nil || status.Desktop.Message == "" {
					t.Fatalf("已注册但缺override被推断为已知状态: %+v %v", status.Desktop, err)
				}
			} else {
				if err != nil {
					t.Fatalf("未注册任务被无关合法词值影响: %v", err)
				}
				requireLoginBool(t, status.Desktop.Enabled, false)
			}
		})
	}
}

func TestLoginStartDisabledRejectsInvalidOutputAndQueryFailures(t *testing.T) {
	queryErr := errors.New("isolated disabled query failure")
	for _, test := range []struct {
		name  string
		lines string
		err   error
	}{
		{"unknown-target", `"io.github.orzlee.displaykvm.desktop" => active`, nil},
		{"unknown-unrelated", `"com.example.other" => active`, nil},
		{"duplicate-service", "\"io.github.orzlee.displaykvm.service\" => false\n\"io.github.orzlee.displaykvm.service\" => disabled", nil},
		{"duplicate-desktop", "\"io.github.orzlee.displaykvm.desktop\" => enabled\n\"io.github.orzlee.displaykvm.desktop\" => true", nil},
		{"wrong-separator", `"io.github.orzlee.displaykvm.desktop" = enabled`, nil},
		{"query-failure", `"io.github.orzlee.displaykvm.desktop" => enabled`, queryErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLoginFixture(t)
			if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
				t.Fatal(err)
			}
			for _, target := range []Target{Service, Desktop} {
				if _, err := f.manager.Enable(context.Background(), target, f.app); err != nil {
					t.Fatal(err)
				}
			}
			f.manager.run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "print-disabled" {
					return []byte("disabled services = {\n" + test.lines + "\n}\n"), test.err
				}
				return f.run(ctx, command, args...)
			}
			values, err := f.manager.disabled(context.Background())
			if err == nil || values != nil || test.err != nil && !errors.Is(err, test.err) {
				t.Fatalf("坏输出或命令失败未保留错误及未知状态: %+v %v", values, err)
			}
			status, err := f.manager.Status(context.Background())
			if err == nil || status.Service.Enabled != nil || status.Desktop.Enabled != nil || test.err != nil && !errors.Is(err, test.err) {
				t.Fatalf("查询失败或坏行被吞掉并推断为已知开关: %+v %v", status, err)
			}
			requireLoginBool(t, status.Service.Present, true)
			requireLoginBool(t, status.Desktop.Present, true)
		})
	}
}

func TestLoginStartProfileLoadDoesNotPreloadDocument(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.profile.ConfigPath); err != nil {
		t.Fatal(err)
	}
	profile, err := LoadServiceProfile(f.manager.profilePath())
	if err != nil || profile != f.profile {
		t.Fatalf("参数加载错误地混入了业务文件就绪校验: %+v %v", profile, err)
	}
	if strings.Contains(profile.ConfigPath, "DisplayKVM/startup") {
		t.Fatal("混淆业务与启动档案")
	}
}

func TestLoginStartProfilePrivatePathProtection(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "permissions", "parent-link", "owner"} {
		t.Run(kind, func(t *testing.T) {
			f := newLoginFixture(t)
			if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
				t.Fatal(err)
			}
			path := f.manager.profilePath()
			switch kind {
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.profile.ConfigPath, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(f.env.ConfigDir, "alias.json")); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "parent-link":
				directory := filepath.Dir(path)
				moved := directory + "-moved"
				if err := os.Rename(directory, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, directory); err != nil {
					t.Fatal(err)
				}
			case "owner":
				f.manager.env.UID++
			}
			_, err := f.manager.Profile(context.Background())
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("安全错误等价首次缺档案: %v", err)
			}
			result, err := f.manager.SaveProfile(context.Background(), f.profile)
			if err == nil {
				t.Fatal("覆盖了受保护档案")
			}
			requireLoginBool(t, result.Saved, false)
		})
	}
}

func TestLoginStartShortCrossProcessLockAndStableInode(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(f.manager.profilePath()), "startup.lock")
	lock, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	before, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	result, err := f.manager.Enable(context.Background(), Desktop, f.app)
	if err == nil {
		t.Fatal("未遵守跨进程锁")
	}
	requireLoginBool(t, result.Saved, false)
	if _, err := os.Lstat(filepath.Join(f.env.HomeDir, "Library")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("锁失败仍写入注册目录")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Enable(context.Background(), Desktop, f.app); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("短时锁文件被删除或替换")
	}
}

func TestLoginStartInvalidApplicationIdentityAndLinks(t *testing.T) {
	for _, kind := range []string{"identity", "program-link", "directory-link", "workspace", "translocated"} {
		t.Run(kind, func(t *testing.T) {
			f := newLoginFixture(t)
			app := f.app
			switch kind {
			case "identity":
				path := filepath.Join(app, "Contents", "Info.plist")
				data, _ := os.ReadFile(path)
				data = []byte(strings.Replace(string(data), "io.github.orzlee.displaykvm", "com.example.other", 1))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "program-link":
				path := filepath.Join(app, "Contents", "MacOS", "displaykvm")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.tool, path); err != nil {
					t.Fatal(err)
				}
			case "directory-link":
				path := filepath.Join(app, "Contents", "MacOS")
				if err := os.Rename(path, path+"-other"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-other", path); err != nil {
					t.Fatal(err)
				}
			case "workspace":
				app = filepath.Join(f.env.HomeDir, "workspace", "DisplayKvm.app")
			case "translocated":
				app = filepath.Join(f.env.HomeDir, "AppTranslocation", "DisplayKvm.app")
			}
			result, err := f.manager.Enable(context.Background(), Desktop, app)
			if err == nil {
				t.Fatal("接受了来源不明确的应用")
			}
			requireLoginBool(t, result.Saved, false)
		})
	}
}

func TestLoginStartEnableRequiresCompleteApplication(t *testing.T) {
	for _, target := range []Target{Service, Desktop} {
		for _, missing := range []string{"displaykvm", "displaykvmd", "displaykvm-cli", "legacy-gui-only"} {
			t.Run(string(target)+"/"+missing, func(t *testing.T) {
				f := newLoginFixture(t)
				if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
					t.Fatal(err)
				}
				remove := []string{missing}
				if missing == "legacy-gui-only" {
					remove = []string{"displaykvmd", "displaykvm-cli"}
				}
				for _, name := range remove {
					if err := os.Remove(filepath.Join(f.app, "Contents", "MacOS", name)); err != nil {
						t.Fatal(err)
					}
				}
				result, err := f.manager.Enable(context.Background(), target, f.app)
				if err == nil {
					t.Fatal("登记接受了不完整应用包")
				}
				requireLoginBool(t, result.Saved, false)
				if _, err := os.Lstat(filepath.Join(f.env.HomeDir, "Library")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("身份校验失败仍创建了登录注册目录")
				}
			})
		}
	}
}

func TestLoginStartEnableRequiresKnownStartupVersion(t *testing.T) {
	for _, marker := range []string{"", "<string>2</string>", "<integer>1</integer>", "<true/>"} {
		t.Run(marker, func(t *testing.T) {
			f := newLoginFixture(t)
			path := filepath.Join(f.app, "Contents", "Info.plist")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			replacement := ""
			if marker != "" {
				replacement = "<key>DisplayKVMStartupVersion</key>" + marker
			}
			data = []byte(strings.Replace(string(data), "<key>DisplayKVMStartupVersion</key><string>1</string>", replacement, 1))
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := f.manager.Enable(context.Background(), Desktop, f.app)
			if err == nil {
				t.Fatal("应用入口相同不能代替已知登录启动功能版本")
			}
			requireLoginBool(t, result.Saved, false)
			if _, err := os.Lstat(f.manager.agentPath(Desktop)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("未知版本仍创建了登录任务")
			}
		})
	}
}

func TestLoginStartEnableRejectsMismatchedGoEntrypoints(t *testing.T) {
	for name, replacement := range map[string]string{"displaykvm": "displaykvmd", "displaykvmd": "displaykvm", "displaykvm-cli": "displaykvm"} {
		t.Run(name, func(t *testing.T) {
			f := newLoginFixture(t)
			if err := os.WriteFile(filepath.Join(f.app, "Contents", "MacOS", name), realLoginPrograms(t)[replacement], 0700); err != nil {
				t.Fatal(err)
			}
			result, err := f.manager.Enable(context.Background(), Desktop, f.app)
			if err == nil {
				t.Fatalf("%s接受了另一个真实Go程序%s的副本", name, replacement)
			}
			requireLoginBool(t, result.Saved, false)
			if _, err := os.Lstat(f.manager.agentPath(Desktop)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("入口错配仍创建了登录任务")
			}
		})
	}
}

func TestLoginStartEnableRequiresSafeHelperFiles(t *testing.T) {
	for _, name := range []string{"displaykvmd", "displaykvm-cli"} {
		for _, kind := range []string{"not-executable", "writable", "symlink", "hardlink", "shell"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				f := newLoginFixture(t)
				path := filepath.Join(f.app, "Contents", "MacOS", name)
				var err error
				switch kind {
				case "not-executable":
					err = os.Chmod(path, 0600)
				case "writable":
					err = os.Chmod(path, 0777)
				case "symlink":
					if err = os.Rename(path, path+"-original"); err == nil {
						err = os.Symlink(path+"-original", path)
					}
				case "hardlink":
					err = os.Link(path, path+"-other")
				case "shell":
					err = os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700)
				}
				if err != nil {
					t.Fatal(err)
				}
				result, err := f.manager.Enable(context.Background(), Desktop, f.app)
				if err == nil {
					t.Fatal("登记未校验未选中目标的辅助程序安全边界")
				}
				requireLoginBool(t, result.Saved, false)
			})
		}
	}
}

func TestLoginStartDisableDoesNotRequireValidApplication(t *testing.T) {
	for _, target := range []Target{Service, Desktop} {
		for _, kind := range []string{"legacy-gui-only", "bad-program", "missing-app"} {
			t.Run(string(target)+"/"+kind, func(t *testing.T) {
				f := newLoginFixture(t)
				if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
					t.Fatal(err)
				}
				for _, item := range []Target{Service, Desktop} {
					if _, err := f.manager.Enable(context.Background(), item, f.app); err != nil {
						t.Fatal(err)
					}
				}
				other := Service
				if target == Service {
					other = Desktop
				}
				otherBefore, err := os.ReadFile(f.manager.agentPath(other))
				if err != nil {
					t.Fatal(err)
				}
				profileBefore, err := os.ReadFile(f.manager.profilePath())
				if err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "legacy-gui-only":
					for _, name := range []string{"displaykvmd", "displaykvm-cli"} {
						if err := os.Remove(filepath.Join(f.app, "Contents", "MacOS", name)); err != nil {
							t.Fatal(err)
						}
					}
					path := filepath.Join(f.app, "Contents", "Info.plist")
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					data = []byte(strings.Replace(string(data), "<key>DisplayKVMStartupVersion</key><string>1</string>", "", 1))
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				case "bad-program":
					if err := os.WriteFile(filepath.Join(f.app, "Contents", "MacOS", "displaykvm"), []byte("broken"), 0600); err != nil {
						t.Fatal(err)
					}
				case "missing-app":
					if err := os.RemoveAll(f.app); err != nil {
						t.Fatal(err)
					}
				}
				f.calls = nil
				result, err := f.manager.Disable(context.Background(), target)
				if err != nil {
					t.Fatalf("应用失效阻止关闭登录项: %v", err)
				}
				requireLoginBool(t, result.Saved, true)
				if _, err := os.Lstat(f.manager.agentPath(target)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("关闭未删除已核实的本功能任务")
				}
				otherAfter, otherErr := os.ReadFile(f.manager.agentPath(other))
				profileAfter, profileErr := os.ReadFile(f.manager.profilePath())
				if otherErr != nil || profileErr != nil || !reflect.DeepEqual(otherBefore, otherAfter) || !reflect.DeepEqual(profileBefore, profileAfter) {
					t.Fatal("关闭旧项修改了另一任务或档案")
				}
				for _, call := range f.calls {
					if call[1] != "print" && call[1] != "print-disabled" {
						t.Fatalf("关闭调用了系统写命令: %v", call)
					}
				}
			})
		}
	}
}

func TestLoginStartPlistArgumentsAreExplicitAndNoKeepAlive(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.manager.SaveProfile(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	for _, target := range []Target{Service, Desktop} {
		if _, err := f.manager.Enable(context.Background(), target, f.app); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(f.env.HomeDir, "Library", "LaunchAgents", "io.github.orzlee.displaykvm."+string(target)+".plist")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Dict struct {
				Keys      []string   `xml:"key"`
				Arguments []string   `xml:"array>string"`
				True      []struct{} `xml:"true"`
			} `xml:"dict"`
		}
		if err := xml.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		for _, key := range wire.Dict.Keys {
			if key == "KeepAlive" {
				t.Fatal("登录安排不能引入持续重启策略")
			}
		}
		if len(wire.Dict.True) != 1 {
			t.Fatalf("缺少明确RunAtLoad: %s", data)
		}
		want := []string{filepath.Join(f.app, "Contents", "MacOS", "displaykvm"), "--login-start"}
		if target == Service {
			want = []string{filepath.Join(f.app, "Contents", "MacOS", "displaykvmd"), "-startup-profile", filepath.Join(f.env.ConfigDir, "DisplayKVM", "startup.json")}
		}
		if !reflect.DeepEqual(wire.Dict.Arguments, want) {
			t.Fatalf("实际登录参数=%v，期望=%v", wire.Dict.Arguments, want)
		}
	}
}
