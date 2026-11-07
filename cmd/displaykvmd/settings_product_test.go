package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/model"
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
	switch goos {
	case "darwin":
		paths.ToolPath = filepath.Join(directory, "m1ddc")
	case "linux":
		paths = controltools.Paths{ToolPath: filepath.Join(directory, "ddcutil"), BusctlPath: filepath.Join(directory, "busctl"), UdevadmPath: filepath.Join(directory, "udevadm")}
	case "windows":
		// Windows使用原生接口，不创建外部工具；假程序仅保留包位置夹具，绝不执行。
	default:
		t.Fatalf("不支持的测试工具平台: %s", goos)
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
	platforms := []string{"darwin", "linux"}
	if runtime.GOOS == "windows" {
		// Windows文件元信息不提供POSIX执行位；原生控制不检查外部工具，
		// 但仍须真实打开完整文档，保留切回其他平台时需要的历史覆盖。
		platforms = []string{"windows"}
	}
	for _, goos := range platforms {
		t.Run(goos, func(t *testing.T) {
			document := settings.NewDocument()
			document.Desktop.Language = "en"
			store, path := settingsProductDocument(t, document)
			root := filepath.Dir(path)
			resolver, expected := settingsProductTools(t, goos, filepath.Join(root, "bundle", "Contents", "MacOS"))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
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
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("默认工具解析改写了用户文档")
			}
			_, override := settingsProductTools(t, goos, filepath.Join(root, "advanced"))
			if goos == "windows" {
				override = controltools.Paths{
					ToolPath: filepath.Join(root, "advanced", "ddcutil"), BusctlPath: filepath.Join(root, "advanced", "busctl"),
					UdevadmPath: filepath.Join(root, "advanced", "udevadm"),
				}
			}
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
			before, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			opened, err = openUnifiedSettings(context.Background(), path, resolver, store)
			if err != nil {
				t.Fatal(err)
			}
			if err := opened.writer.Close(); err != nil {
				t.Fatal(err)
			}
			expected = override
			if goos == "windows" {
				expected = controltools.Paths{}
			}
			if opened.resolved != expected || !reflect.DeepEqual(opened.snapshot.Document, document) {
				t.Fatalf("高级绝对覆盖未应用: resolved=%+v document=%+v", opened.resolved, opened.snapshot.Document)
			}
			after, err = os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("高级覆盖解析改写了用户文档")
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
	goos := "darwin"
	if runtime.GOOS == "windows" {
		goos = "windows"
	}
	resolver, beforePaths := settingsProductTools(t, goos, filepath.Join(original, "Contents", "MacOS"))
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
	if goos == "windows" {
		if opened.resolved != (controltools.Paths{}) {
			t.Fatalf("Windows移动包后仍依赖安装位置: %+v", opened.resolved)
		}
	} else if opened.resolved.ToolPath != filepath.Join(moved, "Contents", "MacOS", "m1ddc") || opened.resolved == beforePaths {
		t.Fatalf("移动包仍绑定旧安装路径: %+v", opened.resolved)
	}
	if !reflect.DeepEqual(opened.snapshot.Document, document) {
		t.Fatalf("移动包仍绑定旧安装路径: resolved=%+v document=%+v", opened.resolved, opened.snapshot.Document)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("包内工具定位或移动改写了用户文档")
	}
}

func TestSettingsProductMissingToolsBlockStartupButNotLanguageSave(t *testing.T) {
	platforms := []string{"darwin", "linux"}
	if runtime.GOOS == "windows" {
		platforms = []string{"windows"}
	}
	for _, goos := range platforms {
		t.Run(goos, func(t *testing.T) {
			document := settings.NewDocument()
			if goos == "windows" {
				legacy := filepath.Join(t.TempDir(), "missing-legacy")
				document.Runtime = controltools.Paths{
					ToolPath: filepath.Join(legacy, "ddcutil"), BusctlPath: filepath.Join(legacy, "busctl"), UdevadmPath: filepath.Join(legacy, "udevadm"),
				}
			}
			expectedRuntime := document.Runtime
			store, path := settingsProductDocument(t, document)
			resolver, paths := settingsProductTools(t, goos, filepath.Join(filepath.Dir(path), "bundle"))
			if goos == "windows" {
				// 历史工具缺失不影响Windows原生启动；语言保存仍须保留原覆盖，
				// 不能把本次运行不使用工具误解为应清空跨平台设置。
				opened, err := openUnifiedSettings(context.Background(), path, resolver, store)
				if err != nil {
					t.Fatal(err)
				}
				if err := opened.writer.Close(); err != nil {
					t.Fatal(err)
				}
				if opened.resolved != (controltools.Paths{}) || !reflect.DeepEqual(opened.snapshot.Document, document) {
					t.Fatalf("Windows缺失历史工具阻断启动或丢失字段: resolved=%+v document=%+v", opened.resolved, opened.snapshot.Document)
				}
			} else {
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
			}
			// POSIX启动就绪失败必须释放写锁；语言保存只校验文档结构，不能要求
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
			if result.Snapshot.Document.Desktop.Language != "en" || result.Snapshot.Document.Runtime != expectedRuntime {
				t.Fatalf("语言或原始工具设置被改写: %+v", result.Snapshot.Document)
			}
		})
	}
}

func TestSettingsProductWindowsNativePathsAreReady(t *testing.T) {
	legacyDirectory := t.TempDir()
	missing := controltools.Paths{
		ToolPath: filepath.Join(legacyDirectory, "missing-ddcutil"), BusctlPath: filepath.Join(legacyDirectory, "missing-busctl"), UdevadmPath: filepath.Join(legacyDirectory, "missing-udevadm"),
	}
	present := controltools.Paths{
		ToolPath: filepath.Join(legacyDirectory, "ddcutil"), BusctlPath: filepath.Join(legacyDirectory, "busctl"), UdevadmPath: filepath.Join(legacyDirectory, "udevadm"),
	}
	// 存在的历史工具也只是普通文件；Windows不得要求其具有POSIX执行位，
	// 与缺失用例共同证明原生解析不依赖这些文件，同时保留文档结构校验。
	for _, path := range []string{present.ToolPath, present.BusctlPath, present.UdevadmPath} {
		if err := os.WriteFile(path, []byte("legacy fixture, never run"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		runtime controltools.Paths
	}{
		{"empty", controltools.Paths{}},
		{"missing-legacy", missing},
		{"present-legacy", present},
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
	document := settings.NewDocument()
	document.Desktop.Language = "invalid"
	if resolved, err := document.ValidateForService(controltools.Resolver{GOOS: "windows"}); model.CodeOf(err) != model.CodeInvalidConfig || resolved != (controltools.Paths{}) {
		t.Fatalf("Windows不依赖工具仍须拒绝非法业务文档: resolved=%+v err=%v", resolved, err)
	}
}

func TestSettingsProductReloadComparesResolvedTools(t *testing.T) {
	platforms := []string{runtime.GOOS}
	if runtime.GOOS == "darwin" {
		// Darwin只使用显示工具；保留Linux模拟用例，继续逐项检查两个事件工具，
		// 同时避免Windows宿主因没有POSIX执行位而误入Linux权限检查。
		platforms = append(platforms, "linux")
	}
	for _, goos := range platforms {
		t.Run(goos, func(t *testing.T) {
			m, _, _, root, _ := managementFixture(t)
			resolver, active := settingsProductTools(t, goos, filepath.Join(root, "bundle"))
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
			if goos == "windows" {
				document.Runtime = controltools.Paths{
					ToolPath: filepath.Join(root, "legacy-ddcutil"), BusctlPath: filepath.Join(root, "legacy-busctl"), UdevadmPath: filepath.Join(root, "legacy-udevadm"),
				}
			}
			managementSetDocument(t, m, document)
			beforeOverride, err := os.ReadFile(m.opts.settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := m.reload(); err != nil || !reflect.DeepEqual(m.settingsSnapshot.Document, document) {
				t.Fatalf("默认位置的显式覆盖或Windows历史覆盖被误判变化: document=%+v err=%v", m.settingsSnapshot.Document, err)
			}
			afterOverride, err := os.ReadFile(m.opts.settingsPath)
			if err != nil || !bytes.Equal(beforeOverride, afterOverride) {
				t.Fatal("重读默认或历史覆盖改写了磁盘文档")
			}
			differentDirectory := filepath.Join(root, "different")
			_, different := settingsProductTools(t, goos, differentDirectory)
			if goos == "windows" {
				// Windows运行路径始终为空；历史覆盖增加、变化或失效均不影响原生控制，
				// 但重读不能清除这些字段，否则跨平台继续使用文档时会丢失高级设置。
				different = controltools.Paths{
					ToolPath: filepath.Join(differentDirectory, "ddcutil"), BusctlPath: filepath.Join(differentDirectory, "busctl"), UdevadmPath: filepath.Join(differentDirectory, "udevadm"),
				}
				for _, path := range []string{different.ToolPath, different.BusctlPath, different.UdevadmPath} {
					if err := os.WriteFile(path, []byte("legacy fixture, never run"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			sections := []string{"tool"}
			if goos == "linux" || goos == "windows" {
				sections = []string{"tool", "busctl", "udevadm"}
			}
			for _, section := range sections {
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
				before, err := os.ReadFile(m.opts.settingsPath)
				if err != nil {
					t.Fatal(err)
				}
				_, _, _, reloadErr := m.reload()
				if goos == "windows" {
					if reloadErr != nil || !reflect.DeepEqual(m.settingsSnapshot.Document, changed) {
						t.Fatalf("Windows历史%s覆盖阻断重读或字段丢失: document=%+v err=%v", section, m.settingsSnapshot.Document, reloadErr)
					}
				} else if reloadErr == nil {
					t.Fatalf("实际%s工具变化仍复用已构造后端/事件来源", section)
				}
				actual := controltools.Paths{ToolPath: m.opts.toolPath, BusctlPath: m.opts.busctlPath, UdevadmPath: m.opts.udevadmPath}
				if actual != active {
					t.Fatalf("重读改写了已构造后端的实际工具: got=%+v want=%+v", actual, active)
				}
				after, err := os.ReadFile(m.opts.settingsPath)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("重读工具覆盖改写了磁盘文档")
				}
			}
			document.Runtime = controltools.Paths{}
			missingPaths := []string{active.ToolPath}
			if goos == "windows" {
				document.Runtime = different
				missingPaths = []string{different.ToolPath, different.BusctlPath, different.UdevadmPath}
			}
			managementSetDocument(t, m, document)
			for _, path := range missingPaths {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(m.opts.settingsPath)
				if err != nil {
					t.Fatal(err)
				}
				_, _, _, reloadErr := m.reload()
				if goos == "windows" {
					actual := controltools.Paths{ToolPath: m.opts.toolPath, BusctlPath: m.opts.busctlPath, UdevadmPath: m.opts.udevadmPath}
					if reloadErr != nil || actual != (controltools.Paths{}) || !reflect.DeepEqual(m.settingsSnapshot.Document, document) {
						t.Fatalf("Windows缺失历史工具阻断重读或字段丢失: active=%+v document=%+v err=%v", actual, m.settingsSnapshot.Document, reloadErr)
					}
				} else if reloadErr == nil {
					t.Fatal("实际工具已缺失仍恢复服务")
				}
				after, err := os.ReadFile(m.opts.settingsPath)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("重读缺失工具改写了磁盘文档")
				}
			}
		})
	}
}
