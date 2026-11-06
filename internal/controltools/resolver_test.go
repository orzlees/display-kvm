package controltools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

func TestControlToolsBundledPaths(t *testing.T) {
	for _, tc := range []struct {
		name       string
		goos       string
		executable string
		names      []string
	}{
		{"mac-gui", "darwin", "DisplayKVM", []string{"m1ddc"}},
		{"mac-cli", "darwin", "displaykvmctl", []string{"m1ddc"}},
		{"mac-daemon", "darwin", "displaykvmd", []string{"m1ddc"}},
		{"linux", "linux", "displaykvmd", []string{"ddcutil", "busctl", "udevadm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := controlToolsDirectory(t)
			if tc.goos == "darwin" {
				dir = filepath.Join(dir, "Display KVM.app", "Contents", "MacOS")
			}
			executable := controlToolsFile(t, filepath.Join(dir, tc.executable), 0o755)
			want := controlToolsFiles(t, dir, tc.names)
			input := Paths{}
			got, err := (Resolver{GOOS: tc.goos, Executable: executable}).Resolve(input)
			if err != nil {
				t.Fatal(err)
			}
			if got != want || input != (Paths{}) {
				t.Fatalf("解析=%+v，期望=%+v，入参=%+v", got, want, input)
			}
		})
	}
}

func TestControlToolsExplicitOverrides(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			dir := controlToolsDirectory(t)
			input := controlToolsFiles(t, dir, []string{"custom-display", "busctl", "udevadm"})
			// 完整的高级配置覆盖不依赖安装目录，不能偷偷回落到工作区或 PATH。
			got, err := (Resolver{GOOS: goos}).Resolve(input)
			if err != nil || got != input {
				t.Fatalf("解析=%+v，入参=%+v，错误=%v", got, input, err)
			}
		})
	}
}

func TestControlToolsMixedLinuxOverrides(t *testing.T) {
	dir := controlToolsDirectory(t)
	executable := controlToolsFile(t, filepath.Join(dir, "displaykvmd"), 0o755)
	want := controlToolsFiles(t, dir, []string{"ddcutil", "busctl", "udevadm"})
	override := controlToolsFile(t, filepath.Join(dir, "advanced", "custom-busctl"), 0o755)
	input := Paths{BusctlPath: override}
	want.BusctlPath = override
	got, err := (Resolver{GOOS: "linux", Executable: executable}).Resolve(input)
	if err != nil || got != want || input != (Paths{BusctlPath: override}) {
		t.Fatalf("解析=%+v，期望=%+v，入参=%+v，错误=%v", got, want, input, err)
	}
}

func TestControlToolsMissingBundledDependency(t *testing.T) {
	for _, tc := range []struct {
		goos    string
		missing string
		field   string
		names   []string
	}{
		{"darwin", "m1ddc", "tool_path", []string{"m1ddc"}},
		{"linux", "ddcutil", "tool_path", []string{"ddcutil", "busctl", "udevadm"}},
		{"linux", "busctl", "busctl_path", []string{"ddcutil", "busctl", "udevadm"}},
		{"linux", "udevadm", "udevadm_path", []string{"ddcutil", "busctl", "udevadm"}},
	} {
		t.Run(tc.goos+"-"+tc.missing, func(t *testing.T) {
			dir := controlToolsDirectory(t)
			executable := controlToolsFile(t, filepath.Join(dir, "displaykvmd"), 0o755)
			for _, name := range tc.names {
				if name != tc.missing {
					controlToolsFile(t, filepath.Join(dir, name), 0o755)
				}
			}
			got, err := (Resolver{GOOS: tc.goos, Executable: executable}).Resolve(Paths{})
			controlToolsError(t, err, model.CodeBackendUnavailable, tc.field)
			if got != (Paths{}) || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), tc.missing) || !strings.Contains(err.Error(), "随包") {
				t.Fatalf("解析=%+v，缺项错误=%v", got, err)
			}
			if strings.Contains(err.Error(), dir) {
				t.Fatalf("错误文本泄漏内部安装路径：%v", err)
			}
		})
	}
}

func TestControlToolsInvalidPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		code model.ErrorCode
	}{
		{"relative", "relative", model.CodeInvalidConfig},
		{"nul", "nul", model.CodeInvalidConfig},
		{"symlink", "symlink", model.CodeInvalidConfig},
		{"directory", "directory", model.CodeInvalidConfig},
		{"not-executable", "not-executable", model.CodePermissionDenied},
		{"missing", "missing", model.CodeBackendUnavailable},
	} {
		for _, field := range []string{"tool_path", "busctl_path", "udevadm_path"} {
			t.Run(tc.name+"-"+field, func(t *testing.T) {
				dir := controlToolsDirectory(t)
				input := controlToolsFiles(t, dir, []string{"ddcutil", "busctl", "udevadm"})
				path := filepath.Join(dir, "invalid")
				switch tc.kind {
				case "relative":
					path = "relative-tool"
				case "nul":
					path += "\x00tool"
				case "symlink":
					if err := os.Symlink(input.ToolPath, path); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				case "not-executable":
					controlToolsFile(t, path, 0o644)
				}
				switch field {
				case "tool_path":
					input.ToolPath = path
				case "busctl_path":
					input.BusctlPath = path
				case "udevadm_path":
					input.UdevadmPath = path
				}
				before := input
				got, err := (Resolver{GOOS: "linux"}).Resolve(input)
				controlToolsError(t, err, tc.code, field)
				if got != (Paths{}) || input != before {
					t.Fatalf("失败时返回了部分路径或修改入参：返回=%+v，入参=%+v，原值=%+v", got, input, before)
				}
				if tc.kind == "missing" && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("丢失底层文件不存在错误：%v", err)
				}
			})
		}
	}
}

func TestControlToolsBundledFileValidation(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "not-executable"} {
		t.Run(kind, func(t *testing.T) {
			dir := controlToolsDirectory(t)
			executable := controlToolsFile(t, filepath.Join(dir, "DisplayKVM"), 0o755)
			path := filepath.Join(dir, "m1ddc")
			code := model.CodeInvalidConfig
			switch kind {
			case "symlink":
				if err := os.Symlink(executable, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			case "not-executable":
				controlToolsFile(t, path, 0o644)
				code = model.CodePermissionDenied
			}
			_, err := (Resolver{GOOS: "darwin", Executable: executable}).Resolve(Paths{})
			controlToolsError(t, err, code, "tool_path")
		})
	}
}

func TestControlToolsRealExecutableDirectory(t *testing.T) {
	dir := controlToolsDirectory(t)
	realDir := filepath.Join(dir, "Real.app", "Contents", "MacOS")
	executable := controlToolsFile(t, filepath.Join(realDir, "DisplayKVM"), 0o755)
	want := controlToolsFiles(t, realDir, []string{"m1ddc"})
	aliasDir := filepath.Join(dir, "launcher")
	controlToolsFile(t, filepath.Join(aliasDir, "m1ddc"), 0o755)
	alias := filepath.Join(aliasDir, "DisplayKVM")
	if err := os.Symlink(executable, alias); err != nil {
		t.Fatal(err)
	}
	parentAlias := filepath.Join(dir, "linked-MacOS")
	if err := os.Symlink(realDir, parentAlias); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{alias, filepath.Join(parentAlias, "DisplayKVM")} {
		got, err := (Resolver{GOOS: "darwin", Executable: entry}).Resolve(Paths{})
		if err != nil || got != want {
			t.Fatalf("未使用真实程序目录：入口=%q，解析=%+v，期望=%+v，错误=%v", entry, got, want, err)
		}
	}
}

func TestControlToolsApplicationMove(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			dir := controlToolsDirectory(t)
			original := filepath.Join(dir, "Original.app")
			moved := filepath.Join(dir, "Moved.app")
			relativeDir := filepath.Join("Contents", "MacOS")
			names := []string{"m1ddc"}
			if goos == "linux" {
				relativeDir = "bin"
				names = []string{"ddcutil", "busctl", "udevadm"}
			}
			originalDir := filepath.Join(original, relativeDir)
			executable := controlToolsFile(t, filepath.Join(originalDir, "displaykvmd"), 0o755)
			controlToolsFiles(t, originalDir, names)
			input := Paths{}
			before, err := (Resolver{GOOS: goos, Executable: executable}).Resolve(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(original, moved); err != nil {
				t.Fatal(err)
			}
			movedDir := filepath.Join(moved, relativeDir)
			want := Paths{ToolPath: filepath.Join(movedDir, names[0])}
			if goos == "linux" {
				want.BusctlPath = filepath.Join(movedDir, "busctl")
				want.UdevadmPath = filepath.Join(movedDir, "udevadm")
			}
			got, err := (Resolver{GOOS: goos, Executable: filepath.Join(movedDir, "displaykvmd")}).Resolve(input)
			if err != nil || got != want || got.ToolPath == before.ToolPath || input != (Paths{}) {
				t.Fatalf("移动后解析=%+v，期望=%+v，原解析=%+v，入参=%+v，错误=%v", got, want, before, input, err)
			}
		})
	}
}

func TestControlToolsExecutableValidation(t *testing.T) {
	for _, kind := range []string{"empty", "relative", "nul", "missing", "directory", "not-executable"} {
		t.Run(kind, func(t *testing.T) {
			dir := controlToolsDirectory(t)
			executable := filepath.Join(dir, "displaykvmd")
			code := model.CodeInvalidConfig
			switch kind {
			case "empty":
				executable = ""
			case "relative":
				executable = "displaykvmd"
			case "nul":
				executable += "\x00"
			case "missing":
				code = model.CodeBackendUnavailable
			case "directory":
				if err := os.Mkdir(executable, 0o755); err != nil {
					t.Fatal(err)
				}
			case "not-executable":
				controlToolsFile(t, executable, 0o644)
				code = model.CodePermissionDenied
			}
			_, err := (Resolver{GOOS: "darwin", Executable: executable}).Resolve(Paths{})
			controlToolsError(t, err, code, "executable")
		})
	}
}

func TestControlToolsWindowsNeedsNoExternalTools(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "unexpected-execution")
	t.Setenv("DISPLAYKVM_CONTROLTOOLS_SENTINEL", sentinel)
	legacy := controlToolsFiles(t, filepath.Join(dir, "legacy"), []string{"ddcutil", "busctl", "udevadm"})
	t.Setenv("PATH", filepath.Dir(legacy.ToolPath))
	for _, tc := range []struct {
		name       string
		executable string
		paths      Paths
	}{
		{"empty", "", Paths{}},
		{"missing-legacy", filepath.Join(dir, "missing-displaykvmd.exe"), Paths{
			ToolPath: filepath.Join(dir, "missing-ddcutil"), BusctlPath: filepath.Join(dir, "missing-busctl"), UdevadmPath: filepath.Join(dir, "missing-udevadm"),
		}},
		{"present-legacy", "relative-displaykvmd.exe", legacy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.paths
			got, err := (Resolver{GOOS: "windows", Executable: tc.executable}).Resolve(input)
			if err != nil || got != (Paths{}) || input != tc.paths {
				t.Fatalf("Windows原生控制仍依赖外部工具或修改覆盖：解析=%+v，入参=%+v，原值=%+v，错误=%v", got, input, tc.paths, err)
			}
		})
	}
	if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Windows路径解析执行了工具或无法确认零执行：%v", err)
	}
}

func TestControlToolsUnsupportedPlatforms(t *testing.T) {
	for _, goos := range []string{"plan9", ""} {
		t.Run(goos, func(t *testing.T) {
			// 不支持的平台必须先返回结构化能力错误，不能误报工具路径缺失。
			got, err := (Resolver{GOOS: goos}).Resolve(Paths{ToolPath: "invalid-relative-tool"})
			controlToolsError(t, err, model.CodeUnsupportedOperation, "")
			if got != (Paths{}) {
				t.Fatalf("解析=%+v，错误=%v", got, err)
			}
		})
	}
}

func TestControlToolsNoExecutionOrPATHSearch(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			dir := controlToolsDirectory(t)
			sentinel := filepath.Join(dir, "unexpected-execution")
			t.Setenv("DISPLAYKVM_CONTROLTOOLS_SENTINEL", sentinel)
			packageDir := filepath.Join(dir, "package")
			executable := controlToolsFile(t, filepath.Join(packageDir, "displaykvmd"), 0o755)
			names := []string{"m1ddc"}
			if goos == "linux" {
				names = []string{"ddcutil", "busctl", "udevadm"}
			}
			pathDir := filepath.Join(dir, "path-decoy")
			controlToolsFiles(t, pathDir, names)
			t.Setenv("PATH", pathDir)
			resolver := Resolver{GOOS: goos, Executable: executable}
			if _, err := resolver.Resolve(Paths{}); model.CodeOf(err) != model.CodeBackendUnavailable {
				t.Fatalf("错误使用了 PATH 中的工具：%v", err)
			}
			overrides := controlToolsFiles(t, packageDir, names)
			if _, err := resolver.Resolve(Paths{}); err != nil {
				t.Fatal(err)
			}
			if _, err := (Resolver{GOOS: goos}).Resolve(overrides); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("解析过程中执行了工具或无法确认零执行：%v", err)
			}
		})
	}
}

func TestControlToolsCurrent(t *testing.T) {
	want, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err = filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Current()
	if err != nil || got.GOOS != runtime.GOOS || got.Executable != want || !filepath.IsAbs(got.Executable) {
		t.Fatalf("当前程序解析=%+v，真实路径=%q，错误=%v", got, want, err)
	}
}

func TestControlToolsPathsJSON(t *testing.T) {
	encoded, err := json.Marshal(Paths{ToolPath: "/display"})
	if err != nil || string(encoded) != `{"tool_path":"/display"}` {
		t.Fatalf("路径字段不符合公共契约：%s，错误=%v", encoded, err)
	}
	want := Paths{ToolPath: "/display", BusctlPath: "/busctl", UdevadmPath: "/udevadm"}
	encoded, err = json.Marshal(want)
	if err != nil || string(encoded) != `{"tool_path":"/display","busctl_path":"/busctl","udevadm_path":"/udevadm"}` {
		t.Fatalf("完整路径字段不符合公共契约：%s，错误=%v", encoded, err)
	}
	var got Paths
	if err := json.Unmarshal(encoded, &got); err != nil || got != want {
		t.Fatalf("路径字段往返=%+v，错误=%v", got, err)
	}
}

func controlToolsDirectory(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("临时 POSIX 可执行权限用例由 macOS/Linux 执行；Windows 原生路径解析另有独立用例")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func controlToolsFile(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// 夹具一旦被错误执行就留下标记；解析器只应读取元信息，不得探测版本或启动工具。
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf invoked > \"$DISPLAYKVM_CONTROLTOOLS_SENTINEL\"\nexit 97\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func controlToolsFiles(t *testing.T, dir string, names []string) Paths {
	t.Helper()
	var paths Paths
	for _, name := range names {
		path := controlToolsFile(t, filepath.Join(dir, name), 0o755)
		switch name {
		case "busctl":
			paths.BusctlPath = path
		case "udevadm":
			paths.UdevadmPath = path
		default:
			paths.ToolPath = path
		}
	}
	return paths
}

func controlToolsError(t *testing.T, err error, code model.ErrorCode, field string) {
	t.Helper()
	var classified *model.Error
	if !errors.As(err, &classified) || classified.Code != code || classified.Field != field {
		t.Fatalf("错误=%v，期望类别=%s、字段=%s", err, code, field)
	}
	if validationErr := classified.Validate(); validationErr != nil {
		t.Fatalf("错误无法作为结构化错误传递：%v", validationErr)
	}
	encoded, marshalErr := json.Marshal(classified)
	if marshalErr != nil || strings.Contains(string(encoded), "PathError") || strings.Contains(string(encoded), "cause") {
		t.Fatalf("结构化错误泄漏底层原因：%s，错误=%v", encoded, marshalErr)
	}
}
