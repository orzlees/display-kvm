package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func noticeTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDesktopAppPathAcceptsBoundedCandidate(t *testing.T) {
	repo := noticeTempDir(t)
	for _, rel := range []string{"desktop/bin/DisplayKvm.app", "build/out/loginstart-20261106/DisplayKvm.app"} {
		app := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(app, 0755); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(rel, "build/out/") {
			if err := os.WriteFile(filepath.Join(filepath.Dir(app), ".displaykvm-run"), []byte("displaykvm-desktop-build-v1\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := desktopAppPath(repo, app)
		if err != nil || got != app {
			t.Fatalf("候选路径未被接受：got=%q err=%v", got, err)
		}
	}
}

func TestDesktopAppPathRequiresBuildOwner(t *testing.T) {
	for _, owner := range []string{"", "displaykvm-core-v1\n", "displaykvm-desktop-v1\n"} {
		repo := noticeTempDir(t)
		app := filepath.Join(repo, "build", "out", "candidate", "DisplayKvm.app")
		if err := os.MkdirAll(app, 0755); err != nil {
			t.Fatal(err)
		}
		if owner != "" {
			if err := os.WriteFile(filepath.Join(filepath.Dir(app), ".displaykvm-run"), []byte(owner), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := desktopAppPath(repo, app); err == nil {
			t.Fatalf("缺失或错误的构建目录身份被接受：%q", owner)
		}
	}
}

func TestDesktopAppPathRejectsEscapeAndLinks(t *testing.T) {
	repo := noticeTempDir(t)
	for _, rel := range []string{"DisplayKvm.app", "build/out/.hidden/DisplayKvm.app", "build/out/run/nested/DisplayKvm.app", "build/out/run/Other.app"} {
		app := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(app, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := desktopAppPath(repo, app); err == nil {
			t.Fatalf("越界路径被接受：%s", app)
		}
	}
	outside := noticeTempDir(t)
	if err := os.Mkdir(filepath.Join(outside, "DisplayKvm.app"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".displaykvm-run"), []byte("displaykvm-desktop-build-v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repo, "build", "out", "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := desktopAppPath(repo, filepath.Join(link, "DisplayKvm.app")); err == nil {
		t.Fatal("候选路径跟随了符号链接")
	}
}

func TestDesktopBinariesRequireCompanionPrograms(t *testing.T) {
	for _, missing := range []string{"displaykvmd", "displaykvm-cli"} {
		t.Run(missing, func(t *testing.T) {
			app := noticeTempDir(t)
			macOS := filepath.Join(app, "Contents", "MacOS")
			if err := os.MkdirAll(macOS, 0755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"displaykvm", "displaykvmd", "displaykvm-cli"} {
				if name != missing {
					if err := os.WriteFile(filepath.Join(macOS, name), []byte("fixture"), 0755); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := desktopBinaries(app); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("缺少辅助程序应明确失败：%v", err)
			}
		})
	}
}

func TestDesktopBinariesRejectInvalidCompanion(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "not-executable"} {
		t.Run(kind, func(t *testing.T) {
			app := noticeTempDir(t)
			macOS := filepath.Join(app, "Contents", "MacOS")
			if err := os.MkdirAll(macOS, 0755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"displaykvm", "displaykvmd", "displaykvm-cli"} {
				path := filepath.Join(macOS, name)
				if name == "displaykvmd" {
					switch kind {
					case "directory":
						if err := os.Mkdir(path, 0755); err != nil {
							t.Fatal(err)
						}
						continue
					case "symlink":
						if err := os.Symlink(filepath.Join(macOS, "displaykvm"), path); err != nil {
							t.Fatal(err)
						}
						continue
					case "not-executable":
						if err := os.WriteFile(path, []byte("fixture"), 0644); err != nil {
							t.Fatal(err)
						}
						continue
					}
				}
				if err := os.WriteFile(path, []byte("fixture"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			_, err := desktopBinaries(app)
			if err == nil {
				t.Fatal("无效程序被接受")
			}
			if !strings.Contains(err.Error(), "displaykvmd") {
				t.Fatalf("未明确标识无效辅助程序：%v", err)
			}
		})
	}
}

func TestDesktopBinariesRejectNonMachOPrograms(t *testing.T) {
	app := noticeTempDir(t)
	macOS := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macOS, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"displaykvm", "displaykvmd", "displaykvm-cli"} {
		if err := os.WriteFile(filepath.Join(macOS, name), []byte("fixture"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := desktopBinaries(app); err == nil || !strings.Contains(err.Error(), "Mach-O") {
		t.Fatalf("普通占位文件应被Mach-O检查拒绝：%v", err)
	}
}

func TestLinkedModulesKeepsCGOCoreEntryInRoot(t *testing.T) {
	repo, run := noticeTempDir(t), noticeTempDir(t)
	trace := filepath.Join(run, "query.txt")
	goPath := filepath.Join(run, "go")
	// 假Go只记录查询参数并返回已知模块；不编译程序，也不访问本机模块缓存。
	script := "#!/bin/sh\nprintf '%s\\n' \"$PWD\" \"$@\" \"CGO_ENABLED=$CGO_ENABLED\" > \"$DISPLAYKVM_NOTICE_TEST_TRACE\"\nprintf '%s\\n' \"$DISPLAYKVM_NOTICE_TEST_MODULES\"\n"
	if err := os.WriteFile(goPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", run+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DISPLAYKVM_NOTICE_TEST_TRACE", trace)
	for _, entry := range []string{projectModule + "/cmd/displaykvmd", projectModule + "/cmd/displaykvm", projectModule + "/desktop"} {
		modulePath, queryDir, pkg, tags := projectModule, repo, "./cmd/"+filepath.Base(entry), ""
		if entry == projectModule+"/desktop" {
			modulePath, queryDir, pkg, tags = entry, filepath.Join(repo, "desktop"), ".", "production"
			if err := os.Mkdir(queryDir, 0755); err != nil {
				t.Fatal(err)
			}
		}
		data, err := json.Marshal(listPackage{ImportPath: entry, Module: &listModule{Path: modulePath, Dir: queryDir}})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("DISPLAYKVM_NOTICE_TEST_MODULES", string(data))
		record := binaryRecord{Entry: entry, GOOS: "darwin", GOARCH: "arm64", CGO: "1", Tags: tags}
		info := &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}
		if _, err := linkedModules(repo, run, record, info); err != nil {
			t.Fatalf("%s：%v", entry, err)
		}
		data, err = os.ReadFile(trace)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if lines[0] != queryDir || !strings.Contains(string(data), "\n"+pkg+"\n") || !strings.Contains(string(data), "CGO_ENABLED=1") {
			t.Fatalf("%s的来源或CGO查询参数不符：%s", entry, data)
		}
	}
}
