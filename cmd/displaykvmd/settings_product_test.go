package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/settings"
)

// 这些文件只用于验证定位及权限，不会执行。解析器使用注入的程序位置，
// 因而测试不会依赖测试进程、开发机PATH或已安装应用中的真实控制程序。
func settingsProductTools(t *testing.T, goos, directory string) (controltools.Resolver, controltools.Paths) {
	t.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	resolver := controltools.Resolver{GOOS: goos, Executable: filepath.Join(directory, "displaykvmd")}
	paths := controltools.Paths{}
	if goos == "darwin" {
		paths.ToolPath = filepath.Join(directory, "m1ddc")
	} else {
		paths = controltools.Paths{ToolPath: filepath.Join(directory, "ddcutil"), BusctlPath: filepath.Join(directory, "busctl"), UdevadmPath: filepath.Join(directory, "udevadm")}
	}
	for _, path := range []string{resolver.Executable, paths.ToolPath, paths.BusctlPath, paths.UdevadmPath} {
		if path != "" {
			if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
	return resolver, paths
}

func settingsProductDocument(t *testing.T, document settings.Document) (*settings.Store, string) {
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
	path := filepath.Join(root, "settings.json")
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	result, saveErr := writer.Save(context.Background(), document, "")
	closeErr := writer.Close()
	if saveErr != nil || closeErr != nil || result.Saved == nil || !*result.Saved || result.Snapshot == nil {
		t.Fatalf("创建完整文档失败: result=%+v save=%v close=%v", result, saveErr, closeErr)
	}
	return store, path
}

func TestSettingsProductStartupUsesBundledToolsAndAdvancedOverrides(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			document := settings.NewDocument()
			document.Desktop.Language = "en"
			store, path := settingsProductDocument(t, document)
			root := filepath.Dir(path)
			resolver, expected := settingsProductTools(t, goos, filepath.Join(root, "bundle", "Contents", "MacOS"))
			opened, err := openUnifiedSettings(context.Background(), path, resolver, store)
			if err != nil {
				t.Fatal(err)
			}
			if err := opened.writer.Close(); err != nil {
				t.Fatal(err)
			}
			if opened.resolved != expected || !reflect.DeepEqual(opened.snapshot.Document, document) {
				t.Fatalf("默认工具未随程序定位或被写进文档: resolved=%+v document=%+v", opened.resolved, opened.snapshot.Document)
			}
			_, override := settingsProductTools(t, goos, filepath.Join(root, "advanced"))
			document.Runtime = override
			writer, err := store.OpenWriter(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, readErr := writer.Read()
			if readErr != nil {
				closeErr := writer.Close()
				t.Fatalf("读取高级覆盖目标失败: read=%v close=%v", readErr, closeErr)
			}
			result, saveErr := writer.Save(context.Background(), document, snapshot.Revision)
			closeErr := writer.Close()
			if saveErr != nil || closeErr != nil || result.Saved == nil || !*result.Saved {
				t.Fatalf("高级覆盖保存失败: save=%v close=%v", saveErr, closeErr)
			}
			opened, err = openUnifiedSettings(context.Background(), path, resolver, store)
			if err != nil {
				t.Fatal(err)
			}
			if err := opened.writer.Close(); err != nil {
				t.Fatal(err)
			}
			if opened.resolved != override || !reflect.DeepEqual(opened.snapshot.Document, document) {
				t.Fatalf("高级绝对覆盖未应用: resolved=%+v document=%+v", opened.resolved, opened.snapshot.Document)
			}
		})
	}
}

func TestSettingsProductMovedBundleDoesNotPersistInstallationPaths(t *testing.T) {
	document := settings.NewDocument()
	document.Desktop.Language = "zh-CN"
	store, path := settingsProductDocument(t, document)
	root := filepath.Dir(path)
	original, moved := filepath.Join(root, "Original.app"), filepath.Join(root, "Moved.app")
	resolver, beforePaths := settingsProductTools(t, "darwin", filepath.Join(original, "Contents", "MacOS"))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := openUnifiedSettings(context.Background(), path, resolver, store)
	if err != nil {
		t.Fatalf("原包定位失败: opened=%+v err=%v", opened, err)
	}
	if err := opened.writer.Close(); err != nil {
		t.Fatal(err)
	}
	if opened.resolved != beforePaths {
		t.Fatalf("原包工具路径错误: %+v", opened.resolved)
	}
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	resolver.Executable = filepath.Join(moved, "Contents", "MacOS", "displaykvmd")
	opened, err = openUnifiedSettings(context.Background(), path, resolver, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.writer.Close(); err != nil {
		t.Fatal(err)
	}
	if opened.resolved.ToolPath != filepath.Join(moved, "Contents", "MacOS", "m1ddc") || opened.resolved == beforePaths || !reflect.DeepEqual(opened.snapshot.Document, document) {
		t.Fatalf("移动包仍绑定旧安装路径: resolved=%+v document=%+v", opened.resolved, opened.snapshot.Document)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("包内工具定位或移动改写了用户文档")
	}
}

func TestSettingsProductMissingToolsBlockStartupButNotLanguageSave(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			document := settings.NewDocument()
			store, path := settingsProductDocument(t, document)
			resolver, paths := settingsProductTools(t, goos, filepath.Join(filepath.Dir(path), "bundle"))
			missing := paths.ToolPath
			if goos == "linux" {
				missing = paths.BusctlPath
			}
			if err := os.Remove(missing); err != nil {
				t.Fatal(err)
			}
			if opened, err := openUnifiedSettings(context.Background(), path, resolver, store); err == nil {
				closeErr := opened.writer.Close()
				t.Fatalf("缺少随包依赖仍允许启动: close=%v", closeErr)
			}
			// 启动就绪失败必须释放写锁；语言保存只校验文档结构，不能要求
			// 尚未用于启动的控制工具就绪，更不能把安装路径写回用户设置。
			writer, err := store.OpenWriter(context.Background(), path)
			if err != nil {
				t.Fatalf("启动失败遗留写锁: %v", err)
			}
			snapshot, readErr := writer.Read()
			if readErr != nil {
				closeErr := writer.Close()
				t.Fatalf("读取语言保存目标失败: read=%v close=%v", readErr, closeErr)
			}
			document = snapshot.Document
			document.Desktop.Language = "en"
			result, saveErr := writer.Save(context.Background(), document, snapshot.Revision)
			closeErr := writer.Close()
			if saveErr != nil || closeErr != nil || result.Snapshot == nil || result.Saved == nil || !*result.Saved {
				t.Fatalf("缺依赖阻断语言保存: save=%v close=%v", saveErr, closeErr)
			}
			if result.Snapshot.Document.Desktop.Language != "en" || result.Snapshot.Document.Runtime != (controltools.Paths{}) {
				t.Fatalf("语言或空工具设置被改写: %+v", result.Snapshot.Document)
			}
		})
	}
}

func TestSettingsProductWindowsNativePathsAreReady(t *testing.T) {
	legacyDirectory := t.TempDir()
	for _, tc := range []struct {
		name    string
		runtime controltools.Paths
	}{
		{"empty", controltools.Paths{}},
		{"legacy", controltools.Paths{
			ToolPath: filepath.Join(legacyDirectory, "ddcutil"), BusctlPath: filepath.Join(legacyDirectory, "busctl"), UdevadmPath: filepath.Join(legacyDirectory, "udevadm"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := settings.NewDocument()
			document.Desktop.Language = "en"
			document.Runtime = tc.runtime
			store, path := settingsProductDocument(t, document)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			resolver := controltools.Resolver{GOOS: "windows", Executable: filepath.Join(filepath.Dir(path), "missing-displaykvmd.exe")}
			// 这里只验证完整配置在Windows原生控制下无需外部执行路径，不能据此
			// 声称Windows IPC、私有目录ACL、单实例互斥或真实显示器已经可用。
			resolved, err := document.ValidateForService(resolver)
			if err != nil || resolved != (controltools.Paths{}) {
				t.Fatalf("Windows原生配置仍要求工具路径: resolved=%+v err=%v", resolved, err)
			}
			opened, err := openUnifiedSettings(context.Background(), path, resolver, store)
			if err != nil {
				t.Fatal(err)
			}
			if err := opened.writer.Close(); err != nil {
				t.Fatal(err)
			}
			if opened.resolved != (controltools.Paths{}) || !reflect.DeepEqual(opened.snapshot.Document, document) {
				t.Fatalf("Windows运行路径或文档分区被改写: resolved=%+v document=%+v", opened.resolved, opened.snapshot.Document)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("Windows原生路径解析改写了用户文档")
			}
		})
	}
	for _, invalid := range []string{"relative-tool", filepath.Join(legacyDirectory, "tool") + "\x00"} {
		for _, paths := range []controltools.Paths{{ToolPath: invalid}, {BusctlPath: invalid}, {UdevadmPath: invalid}} {
			document := settings.NewDocument()
			document.Runtime = paths
			if resolved, err := document.ValidateForService(controltools.Resolver{GOOS: "windows"}); err == nil || resolved != (controltools.Paths{}) {
				t.Fatalf("Windows原生解析绕过文档路径结构校验: paths=%+v resolved=%+v err=%v", paths, resolved, err)
			}
		}
	}
}

func TestSettingsProductReloadComparesResolvedTools(t *testing.T) {
	m, _, _, root, _ := managementFixture(t)
	resolver, active := settingsProductTools(t, "linux", filepath.Join(root, "bundle"))
	m.opts.toolResolver = resolver
	m.opts.toolPath, m.opts.busctlPath, m.opts.udevadmPath = active.ToolPath, active.BusctlPath, active.UdevadmPath
	document := m.settingsSnapshot.Document
	document.Runtime = controltools.Paths{}
	document.Desktop.Language = "en"
	managementSetDocument(t, m, document)
	if _, _, _, err := m.reload(); err != nil || m.settingsSnapshot.Document.Runtime != (controltools.Paths{}) || m.settingsSnapshot.Document.Desktop.Language != "en" {
		t.Fatalf("空默认工具被误判变化或写回路径: %+v %v", m.settingsSnapshot.Document, err)
	}
	document.Runtime = active
	managementSetDocument(t, m, document)
	if _, _, _, err := m.reload(); err != nil {
		t.Fatalf("与默认位置相同的显式覆盖被误判变化: %v", err)
	}
	_, different := settingsProductTools(t, "linux", filepath.Join(root, "different"))
	for _, section := range []string{"tool", "busctl", "udevadm"} {
		changed := document
		switch section {
		case "tool":
			changed.Runtime.ToolPath = different.ToolPath
		case "busctl":
			changed.Runtime.BusctlPath = different.BusctlPath
		case "udevadm":
			changed.Runtime.UdevadmPath = different.UdevadmPath
		}
		managementSetDocument(t, m, changed)
		if _, _, _, err := m.reload(); err == nil {
			t.Fatalf("实际%s工具变化仍复用已构造后端/事件来源", section)
		}
	}
	document.Runtime = controltools.Paths{}
	managementSetDocument(t, m, document)
	if err := os.Remove(active.ToolPath); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.reload(); err == nil {
		t.Fatal("实际工具已缺失仍恢复服务")
	}
}
