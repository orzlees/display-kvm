package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/loginstart"
	"github.com/orzlee/displayKvm/internal/settings"
)

func unifiedCLIStore(t *testing.T) (*settings.Store, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.NewStore(settings.Options{Environment: func() (settings.Environment, error) {
		return settings.Environment{ConfigDir: root, UID: os.Getuid()}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return store, root
}

func unifiedCLI(t *testing.T, store *settings.Store, args ...string) (int, Envelope) {
	t.Helper()
	var output bytes.Buffer
	code := Run(context.Background(), append([]string{"--json"}, args...), Options{Stdout: &output, Stderr: &output, NewSettings: func() (*settings.Store, error) { return store, nil }, Connect: func(string) (Client, error) { t.Fatal("离线配置命令连接后台"); return nil, nil }, NewLoginStart: func() (*loginstart.Manager, error) { t.Fatal("配置命令选择了启动档案"); return nil, nil }})
	var envelope Envelope
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatalf("无JSON外壳: %s %v", output.String(), err)
	}
	return code, envelope
}

func TestUnifiedSettingsHelpOnlyAdvertisesCompleteDocuments(t *testing.T) {
	for _, removed := range []string{"config import", "--rules", "--schedules", "迁移"} {
		if strings.Contains(usage, removed) {
			t.Fatalf("帮助仍宣传已删除入口: %s", removed)
		}
	}
	for _, supported := range []string{"config check --file", "config new --file", "config set-tool --file", "startup configure --config COMPLETE_FILE"} {
		if !strings.Contains(usage, supported) {
			t.Fatalf("帮助遗漏完整文档入口: %s", supported)
		}
	}
}

func TestUnifiedSettingsCLINewCheckSetToolAndOverwrite(t *testing.T) {
	store, root := unifiedCLIStore(t)
	path := filepath.Join(root, "settings.json")
	if code, result := unifiedCLI(t, store, "config", "new", "--file", path); code != ExitOK || result.Command != "config new" {
		t.Fatalf("新建失败: %+v %d", result, code)
	}
	before, err := store.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := unifiedCLI(t, store, "config", "new", "--file", path); code == ExitOK {
		t.Fatal("默认覆盖已有完整文档")
	}
	if code, result := unifiedCLI(t, store, "config", "check", "--file", path); code != ExitOK || result.Command != "config check" {
		t.Fatalf("完整文档检查失败: %+v", result)
	}
	tool := filepath.Join(root, "tool")
	if code, _ := unifiedCLI(t, store, "config", "set-tool", "--file", path, "--tool", tool); code != ExitOK {
		t.Fatal("设置工具失败")
	}
	after, err := store.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wanted := before.Document
	wanted.Runtime.ToolPath = tool
	if !reflect.DeepEqual(after.Document, wanted) {
		t.Fatalf("工具修改覆盖其它分区: %+v", after.Document)
	}
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := unifiedCLI(t, store, "config", "set-tool", "--file", path, "--tool", ""); code == ExitOK {
		t.Fatal("持锁文件可由CLI修改")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if code, _ := unifiedCLI(t, store, "config", "set-tool", "--file", path); code != ExitUsage {
		t.Fatal("set-tool未要求显式工具参数")
	}
	if code, _ := unifiedCLI(t, store, "config", "set-tool", "--file", path, "--tool", ""); code != ExitOK {
		t.Fatal("不能显式清空待选择工具")
	}
	cleared, err := store.ReadFile(path)
	if err != nil || !reflect.DeepEqual(cleared.Document, before.Document) {
		t.Fatalf("清空工具覆盖其它分区: %+v %v", cleared.Document, err)
	}
	if code, _ := unifiedCLI(t, store, "config", "new", "--file", path, "--overwrite"); code != ExitOK {
		t.Fatal("明确覆盖合法完整目标失败")
	}
	if code, _ := unifiedCLI(t, store, "config", "new", "--file", filepath.Join(root, "absent.json"), "--overwrite"); code == ExitOK {
		t.Fatal("overwrite未要求已有有效目标")
	}
	for _, content := range []string{`{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`, `{"kind":"displaykvm-settings","version":999}`, `broken`} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if code, _ := unifiedCLI(t, store, "config", "new", "--file", path, "--overwrite"); code == ExitOK {
			t.Fatalf("覆盖无效或旧格式目标: %s", content)
		}
		actual, _ := os.ReadFile(path)
		if string(actual) != content {
			t.Fatal("拒绝覆盖仍改变文件")
		}
	}
}

func TestUnifiedSettingsCLIRejectsRemovedImportAndBareConfiguration(t *testing.T) {
	store, root := unifiedCLIStore(t)
	source := filepath.Join(root, "bare.json")
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, fixtureConfig()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "import"},
		{"config", "import", "--config", source, "--output", filepath.Join(root, "output.json")},
		{"config", "check", "--file", source},
		{"config", "set-tool", "--file", source, "--tool", filepath.Join(root, "tool")},
	} {
		if code, result := unifiedCLI(t, store, args...); code != ExitUsage || result.OK || result.Error == nil {
			payload, _ := json.Marshal(result)
			t.Fatalf("接受已删除入口或裸配置: args=%v exit=%d envelope=%s", args, code, payload)
		}
	}
	actual, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(actual, encoded.Bytes()) {
		t.Fatal("拒绝裸配置时修改了来源原件")
	}
	if _, err := os.Stat(filepath.Join(root, "output.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("已删除导入入口仍产生文件: %v", err)
	}
}

func TestUnifiedSettingsCLIStartupFormats(t *testing.T) {
	store, root := unifiedCLIStore(t)
	path := filepath.Join(root, "settings.json")
	tool := filepath.Join(root, "tool")
	if err := os.WriteFile(tool, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	if code, _ := unifiedCLI(t, store, "config", "new", "--file", path, "--tool", tool); code != ExitOK {
		t.Fatal("新建失败")
	}
	for _, args := range [][]string{{"--config", path, "--tool", tool}, {"--config", path, "--rules", ""}, {"--config", path, "--schedules", ""}} {
		var output bytes.Buffer
		code := Run(context.Background(), append([]string{"--json", "startup", "configure"}, args...), Options{Stdout: &output, Stderr: &output, NewSettings: func() (*settings.Store, error) { return store, nil }, NewLoginStart: func() (*loginstart.Manager, error) {
			t.Fatal("完整文档混用旧参数仍创建管理器")
			return nil, nil
		}})
		if code != ExitUsage {
			t.Fatalf("未拒绝混用: %v %s", args, output.String())
		}
	}
	if runtime.GOOS != "darwin" {
		t.Skip("v2私有档案保存仅macOS提供")
	}
	env := loginstart.Environment{ConfigDir: root, HomeDir: root, UID: os.Getuid()}
	manager, err := loginstart.New(loginstart.Options{Environment: func() (loginstart.Environment, error) { return env, nil }, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "/bin/launchctl" && len(args) > 0 {
			if args[0] == "print-disabled" {
				return []byte("disabled services = {\n}\n"), nil
			}
			if args[0] == "print" {
				return []byte(fmt.Sprintf("Could not find service %q in domain for user gui: %d", filepath.Base(args[1]), env.UID)), errors.New("not loaded")
			}
		}
		t.Fatalf("保存启动选择调用系统写命令: %s %v", name, args)
		return nil, errors.New("禁止系统写调用")
	}})
	if err != nil {
		t.Fatal(err)
	}
	var outputBuffer bytes.Buffer
	code := Run(context.Background(), []string{"--json", "startup", "configure", "--config", path}, Options{Stdout: &outputBuffer, Stderr: &outputBuffer, NewSettings: func() (*settings.Store, error) { return store, nil }, NewLoginStart: func() (*loginstart.Manager, error) { return manager, nil }})
	if code != ExitOK {
		t.Fatalf("完整文档启动选择保存失败: %s", outputBuffer.String())
	}
	profile, err := manager.Profile(context.Background())
	if err != nil || profile.Version != 2 || profile.ConfigPath != path {
		t.Fatalf("未写v2档案: %+v %v", profile, err)
	}
	legacy := filepath.Join(root, "legacy.json")
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, fixtureConfig()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	outputBuffer.Reset()
	code = Run(context.Background(), []string{"--json", "startup", "configure", "--config", legacy}, Options{Stdout: &outputBuffer, Stderr: &outputBuffer, NewSettings: func() (*settings.Store, error) { return store, nil }, NewLoginStart: func() (*loginstart.Manager, error) {
		t.Fatal("裸配置仍创建启动管理器")
		return nil, nil
	}})
	if code != ExitUsage {
		t.Fatalf("接受裸配置启动选择: exit=%d %s", code, outputBuffer.String())
	}
	profile, err = manager.Profile(context.Background())
	if err != nil || profile.Version != 2 || profile.ConfigPath != path {
		t.Fatalf("拒绝裸配置时改变了原启动选择: %+v %v", profile, err)
	}
}
