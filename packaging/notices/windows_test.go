package main

import (
	"debug/pe"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func windowsBuildFixture() (buildRecord, buildRecord) {
	gui := buildRecord{Schema: 1, Toolchain: toolchain, VersionLabel: "v1.0.0", SourceRevision: "0123456789abcdef", SourceContentSHA256: strings.Repeat("a", 64), Binaries: []binaryRecord{{File: "displaykvm.exe", Entry: projectModule + "/desktop", Main: moduleRecord{Path: projectModule + "/desktop"}, GOOS: "windows", GOARCH: "arm64", CGO: "0", Tags: "production"}}}
	core := buildRecord{Schema: 1, Toolchain: toolchain, VersionLabel: gui.VersionLabel, SourceRevision: gui.SourceRevision, SourceContentSHA256: gui.SourceContentSHA256, Binaries: []binaryRecord{
		{File: "bin/windows-arm64/displaykvm.exe", Entry: projectModule + "/cmd/displaykvm", Main: moduleRecord{Path: projectModule}, GOOS: "windows", GOARCH: "arm64", CGO: "0"},
		{File: "bin/windows-arm64/displaykvmd.exe", Entry: projectModule + "/cmd/displaykvmd", Main: moduleRecord{Path: projectModule}, GOOS: "windows", GOARCH: "arm64", CGO: "0"},
	}}
	return gui, core
}

func TestWindowsCompanionsKeepRolesAndArchitecture(t *testing.T) {
	gui, core := windowsBuildFixture()
	records, err := windowsCompanions(gui, core)
	if err != nil || len(records) != 3 || records[0].Entry != projectModule+"/desktop" || records[1].Entry != projectModule+"/cmd/displaykvmd" || records[2].Entry != projectModule+"/cmd/displaykvm" {
		t.Fatalf("未保留GUI、后台和CLI真实角色：%+v %v", records, err)
	}
	for _, change := range []struct {
		name string
		edit func(*buildRecord, *buildRecord)
	}{
		{"renamed-cli", func(g, c *buildRecord) { g.Binaries[0].Entry = projectModule + "/cmd/displaykvm" }},
		{"mixed-architecture", func(g, c *buildRecord) { c.Binaries[1].GOARCH = "amd64" }},
		{"missing-daemon", func(g, c *buildRecord) { c.Binaries = c.Binaries[:1] }},
		{"different-version", func(g, c *buildRecord) { c.VersionLabel = "v0.9.0" }},
		{"different-source", func(g, c *buildRecord) { c.SourceRevision = "fedcba9876543210" }},
		{"different-working-tree", func(g, c *buildRecord) { c.SourceContentSHA256 = strings.Repeat("b", 64) }},
		{"wrong-module", func(g, c *buildRecord) { g.Binaries[0].Main.Path = projectModule }},
		{"missing-provenance", func(g, c *buildRecord) { g.SourceRevision = ""; c.SourceRevision = "" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			gui, core := windowsBuildFixture()
			change.edit(&gui, &core)
			if _, err := windowsCompanions(gui, core); err == nil {
				t.Fatal("不完整或来源/角色不一致的候选被接受")
			}
		})
	}
}

func TestWindowsPERejectsWrongMachineDLLAndConsoleGUI(t *testing.T) {
	for _, sample := range []struct {
		name                                string
		machine, characteristics, subsystem uint16
		wantError                           bool
	}{
		{"arm64-gui", pe.IMAGE_FILE_MACHINE_ARM64, pe.IMAGE_FILE_EXECUTABLE_IMAGE, pe.IMAGE_SUBSYSTEM_WINDOWS_GUI, false},
		{"wrong-machine", pe.IMAGE_FILE_MACHINE_AMD64, pe.IMAGE_FILE_EXECUTABLE_IMAGE, pe.IMAGE_SUBSYSTEM_WINDOWS_GUI, true},
		{"dll", pe.IMAGE_FILE_MACHINE_ARM64, pe.IMAGE_FILE_EXECUTABLE_IMAGE | pe.IMAGE_FILE_DLL, pe.IMAGE_SUBSYSTEM_WINDOWS_GUI, true},
		{"console-gui", pe.IMAGE_FILE_MACHINE_ARM64, pe.IMAGE_FILE_EXECUTABLE_IMAGE, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			// 最小PE64头用于校验文件角色，不编译、不执行Windows程序。
			data := make([]byte, 64+4+20+240)
			copy(data, "MZ")
			binary.LittleEndian.PutUint32(data[0x3c:], 64)
			copy(data[64:], "PE\x00\x00")
			binary.LittleEndian.PutUint16(data[68:], sample.machine)
			binary.LittleEndian.PutUint16(data[84:], 240)
			binary.LittleEndian.PutUint16(data[86:], sample.characteristics)
			binary.LittleEndian.PutUint16(data[88:], 0x20b)
			binary.LittleEndian.PutUint16(data[88+68:], sample.subsystem)
			// 240字节PE32+可选头包含112字节固定字段和16个8字节数据目录；
			// 数量必须与预留目录空间一致，否则解析先失败，无法检验机器类型及GUI角色。
			binary.LittleEndian.PutUint32(data[88+108:], 16)
			path := filepath.Join(noticeTempDir(t), "displaykvm.exe")
			if err := os.WriteFile(path, data, 0644); err != nil {
				t.Fatal(err)
			}
			err := windowsPE(path, "arm64", true)
			if (err != nil) != sample.wantError {
				t.Fatalf("PE角色检查错误：%v", err)
			}
		})
	}
}

func TestWindowsInputRequiresOwnedBoundedBuild(t *testing.T) {
	repo := noticeTempDir(t)
	run := filepath.Join(repo, "build", "out", "gui")
	if err := os.MkdirAll(run, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(run, ".displaykvm-run")
	app := filepath.Join(run, "displaykvm.exe")
	if err := os.WriteFile(app, []byte("input-boundary-fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"displaykvm-core-v1\n", "displaykvm-windows-v1\n"} {
		if err := os.WriteFile(marker, []byte(owner), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := windowsInput(repo, app); err == nil {
			t.Fatal("错误目录身份被接受")
		}
	}
	if err := os.WriteFile(marker, []byte("displaykvm-windows-build-v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if actual, err := windowsInput(repo, app); err != nil || actual != app {
		t.Fatalf("自有单层编号输入应可用：%q %v", actual, err)
	}
	nested := filepath.Join(run, "nested")
	if err := os.Mkdir(nested, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".displaykvm-run", "displaykvm.exe"} {
		if err := os.WriteFile(filepath.Join(nested, name), []byte("displaykvm-windows-build-v1\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := windowsInput(repo, filepath.Join(nested, "displaykvm.exe")); err == nil {
		t.Fatal("嵌套输入被接受")
	}
}
