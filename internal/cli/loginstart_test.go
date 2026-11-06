package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/orzlee/displayKvm/internal/loginstart"
	"github.com/orzlee/displayKvm/internal/settings"
)

func TestLoginStartCLIRejectsAmbiguousArgumentsWithoutDaemon(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "--config-dir", "/ipc", "startup", "status"},
		{"startup", "status", "--config-dir", "/ipc", "--json"},
		{"--json", "startup", "configure"},
		{"--json", "startup", "configure", "--config", "/config.json", "--tool", ""},
		{"--json", "startup", "configure", "--config", "/config.json", "--rules", ""},
		{"--json", "startup", "configure", "--config", "/config.json", "--schedules", ""},
		{"--json", "startup", "enable", "service"},
		{"--json", "startup", "disable", "other"},
		{"--json", "startup", "enable", "desktop", "--app", "/Applications/DisplayKvm.app", "extra"},
	} {
		var output bytes.Buffer
		code := Run(context.Background(), args, Options{Stdout: &output, Stderr: &output, Connect: func(string) (Client, error) { t.Fatal("startup连接了daemon"); return nil, nil }, NewLoginStart: func() (*loginstart.Manager, error) { t.Fatal("无效参数创建管理器"); return nil, nil }})
		if code != ExitUsage {
			t.Fatalf("参数=%v code=%d 输出=%s", args, code, output.String())
		}
		var result Envelope
		if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.OK || result.Error == nil {
			t.Fatalf("未沿用JSON错误外壳: %s %v", output.String(), err)
		}
	}
}

func TestLoginStartCLIConfigureAndStatusUseRealPrivateFiles(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("仅macOS提供登录任务管理")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := loginstart.Environment{ConfigDir: filepath.Join(root, "config"), HomeDir: filepath.Join(root, "home"), UID: os.Getuid()}
	for _, path := range []string{env.ConfigDir, env.HomeDir} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	configPath, toolPath := filepath.Join(root, "business.json"), filepath.Join(root, "tool")
	document := settings.NewDocument()
	document.Configuration, document.Runtime.ToolPath = fixtureConfig(), toolPath
	var encoded bytes.Buffer
	if err := settings.Encode(&encoded, document); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(toolPath, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := loginstart.New(loginstart.Options{Environment: func() (loginstart.Environment, error) { return env, nil }, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "/bin/launchctl" {
			t.Fatalf("不允许命令: %s", name)
		}
		if args[0] == "print-disabled" {
			return []byte("disabled services = {\n}\n"), nil
		}
		if args[0] != "print" {
			t.Fatalf("配置调用了系统写入命令: %v", args)
		}
		label := filepath.Base(args[1])
		return []byte(fmt.Sprintf("Could not find service %q in domain for user gui: %d", label, env.UID)), errors.New("not loaded")
	}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.NewStore(settings.Options{Environment: func() (settings.Environment, error) {
		return settings.Environment{ConfigDir: env.ConfigDir, UID: env.UID}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--json", "startup", "configure", "--config", configPath}, {"startup", "status", "--json"}} {
		var output bytes.Buffer
		code := Run(context.Background(), args, Options{Stdout: &output, Stderr: &output, NewSettings: func() (*settings.Store, error) { return store, nil }, NewLoginStart: func() (*loginstart.Manager, error) { return manager, nil }, Connect: func(string) (Client, error) { t.Fatal("连接了后台"); return nil, nil }})
		var result struct {
			Command string          `json:"command"`
			OK      bool            `json:"ok"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(output.Bytes(), &result); err != nil || code != ExitOK || !result.OK {
			t.Fatalf("离线CLI失败: %s %v code=%d", output.String(), err, code)
		}
		if result.Command == "startup configure" {
			var saved loginstart.Result
			if err := json.Unmarshal(result.Data, &saved); err != nil || saved.Saved == nil || !*saved.Saved {
				t.Fatalf("未反馈实际保存: %s %v", result.Data, err)
			}
		}
	}
	profile, err := manager.Profile(context.Background())
	if err != nil || profile.Version != 2 || profile.ConfigPath != configPath {
		t.Fatalf("没有读取同一核心档案: %+v %v", profile, err)
	}
	if _, err := os.Stat(filepath.Join(env.HomeDir, "Library")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("configure创建了登录项")
	}
}

func TestLoginStartCLIStatusUsesIndependentManager(t *testing.T) {
	var output bytes.Buffer
	called := false
	code := Run(context.Background(), []string{"--json", "startup", "status"}, Options{Stdout: &output, Stderr: &output, Connect: func(string) (Client, error) { t.Fatal("连接后台"); return nil, nil }, NewLoginStart: func() (*loginstart.Manager, error) { called = true; return nil, errors.New("isolated manager failure") }})
	var envelope Envelope
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || !called || code != ExitFailure || envelope.Command != "startup status" || envelope.OK {
		t.Fatalf("没有独立执行: %s code=%d err=%v", output.String(), code, err)
	}
}
