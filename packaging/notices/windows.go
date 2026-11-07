package main

import (
	"crypto/sha256"
	"debug/pe"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

// 归档标签不注入产品版本；额外记录实际检出的提交与已跟踪文件修改状态，
// 防止把不同来源的GUI和核心仅凭相同文件名、版本标签配成一包。
func buildSource(repo string) (string, bool, string, error) {
	data, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "HEAD").Output()
	if err != nil {
		return "", false, "", fmt.Errorf("无法记录实际构建提交：%w", err)
	}
	revision := strings.TrimSpace(string(data))
	status, err := exec.Command("git", "-C", repo, "status", "--porcelain", "--untracked-files=no").Output()
	if err != nil {
		return "", false, "", err
	}
	// 仅比较提交和dirty布尔值无法区分同提交的两份不同工作树。
	// 将产品源码、桌面内嵌资源及模块锁定文件一起哈希，包含未提交的新源码；
	// 排除可再生缓存和成品，避免先构建核心这一动作本身改变桌面的来源标记。
	source := sha256.New()
	for _, name := range []string{"go.mod", "go.sum", "cmd", "internal", "desktop"} {
		path := filepath.Join(repo, name)
		if name == "go.sum" {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
		err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == ".cache" || entry.Name() == "bin" || entry.Name() == "node_modules" || entry.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			hash, err := digest(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(repo, path)
			if err != nil {
				return err
			}
			fmt.Fprintf(source, "%s\x00%s\n", filepath.ToSlash(rel), hash)
			return nil
		})
		if err != nil {
			return "", false, "", err
		}
	}
	return revision, len(status) != 0, fmt.Sprintf("%x", source.Sum(nil)), nil
}

func readBuild(path string) (buildRecord, error) {
	var record buildRecord
	if _, err := regular(path); err != nil {
		return record, err
	}
	f, err := os.Open(path)
	if err != nil {
		return record, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return record, errors.New("构建记录含额外内容")
	}
	if record.Schema != 1 || record.Toolchain != toolchain || !validLabel(record.VersionLabel) {
		return record, errors.New("构建记录版本或工具链无效")
	}
	return record, nil
}

func windowsPE(path, arch string, gui bool) error {
	if _, err := regular(path); err != nil {
		return err
	}
	image, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("Windows程序不是有效PE：%w", err)
	}
	defer image.Close()
	want := uint16(pe.IMAGE_FILE_MACHINE_AMD64)
	if arch == "arm64" {
		want = pe.IMAGE_FILE_MACHINE_ARM64
	} else if arch != "amd64" {
		return errors.New("未知Windows架构")
	}
	header, ok := image.OptionalHeader.(*pe.OptionalHeader64)
	if image.Machine != want || image.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 || image.Characteristics&pe.IMAGE_FILE_DLL != 0 || !ok {
		return errors.New("Windows程序架构、PE64或可执行类型不匹配")
	}
	if gui && header.Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI {
		return errors.New("Windows桌面必须使用windowsgui子系统")
	}
	return nil
}

func recordWindows(repo, runDir, version, target string) error {
	if !validLabel(version) || target != "windows-amd64" && target != "windows-arm64" {
		return errors.New("Windows版本或目标无效")
	}
	arch := strings.TrimPrefix(target, "windows-")
	path := filepath.Join(runDir, "displaykvm.exe")
	if err := windowsPE(path, arch, true); err != nil {
		return err
	}
	record, _, err := inspect(path, "displaykvm.exe", projectModule+"/desktop", "windows", arch, "0", "production")
	if err != nil {
		return err
	}
	if record.Main.Path != projectModule+"/desktop" {
		return errors.New("Windows GUI不属于桌面模块")
	}
	revision, modified, sourceHash, err := buildSource(repo)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(runDir, "build.json"), buildRecord{Schema: 1, Toolchain: toolchain, VersionLabel: version, SourceRevision: revision, SourceModified: modified, SourceContentSHA256: sourceHash, Binaries: []binaryRecord{record}})
}

func windowsInput(repo, app string) (string, error) {
	if !filepath.IsAbs(app) {
		app = filepath.Join(repo, app)
	}
	app = filepath.Clean(app)
	parent := filepath.Dir(app)
	if filepath.Base(app) != "displaykvm.exe" || filepath.Dir(parent) != filepath.Join(repo, "build", "out") || !validLabel(filepath.Base(parent)) {
		return "", errors.New("Windows输入必须为build/out单个构建编号内的displaykvm.exe")
	}
	if err := noLinks(parent); err != nil {
		return "", err
	}
	marker := filepath.Join(parent, ".displaykvm-run")
	if _, err := regular(marker); err != nil {
		return "", err
	}
	owner, err := os.ReadFile(marker)
	if err != nil {
		return "", err
	}
	if string(owner) != "displaykvm-windows-build-v1\n" {
		return "", errors.New("Windows桌面构建目录身份不匹配")
	}
	if _, err := regular(app); err != nil {
		return "", err
	}
	return app, nil
}

// Windows按大小写不敏感解析文件名，原始核心的displaykvm.exe是CLI。
// 必须先按Go入口和架构配对，复制时仅将CLI改名，不能用改名CLI伪装GUI。
func windowsCompanions(gui, core buildRecord) ([]binaryRecord, error) {
	if len(gui.Binaries) != 1 || gui.VersionLabel != core.VersionLabel || gui.SourceRevision == "" || gui.SourceRevision != core.SourceRevision || gui.SourceModified != core.SourceModified || len(gui.SourceContentSHA256) != 64 || gui.SourceContentSHA256 != core.SourceContentSHA256 {
		return nil, errors.New("Windows桌面与核心必须来自相同版本标签及构建来源")
	}
	g := gui.Binaries[0]
	if g.Entry != projectModule+"/desktop" || g.Main.Path != projectModule+"/desktop" || g.File != "displaykvm.exe" || g.GOOS != "windows" || g.GOARCH != "amd64" && g.GOARCH != "arm64" || g.CGO != "0" || g.Tags != "production" {
		return nil, errors.New("Windows GUI角色或构建契约不符")
	}
	records := []binaryRecord{g}
	for _, name := range []string{"displaykvmd", "displaykvm"} {
		matches := 0
		for _, r := range core.Binaries {
			if r.Entry == projectModule+"/cmd/"+name && r.GOOS == "windows" && r.GOARCH == g.GOARCH && r.CGO == "0" && r.Tags == "" && r.Main.Path == projectModule && r.File == "bin/windows-"+g.GOARCH+"/"+name+".exe" {
				records = append(records, r)
				matches++
			}
		}
		if matches != 1 {
			return nil, fmt.Errorf("Windows候选缺少唯一同架构%s", name)
		}
	}
	return records, nil
}

func packageWindows(repo, runDir, app, coreRun string) error {
	app, err := windowsInput(repo, app)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(coreRun) {
		coreRun = filepath.Join(repo, coreRun)
	}
	coreRun = filepath.Clean(coreRun)
	if filepath.Dir(coreRun) != filepath.Join(repo, "build", "out") || !validLabel(filepath.Base(coreRun)) {
		return errors.New("核心目录必须为build/out下单个编号")
	}
	marker := filepath.Join(coreRun, ".displaykvm-run")
	if _, err := regular(marker); err != nil {
		return err
	}
	owner, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	if string(owner) != "displaykvm-core-v1\n" {
		return errors.New("Windows配套核心目录身份不匹配")
	}
	gui, err := readBuild(filepath.Join(filepath.Dir(app), "build.json"))
	if err != nil {
		return err
	}
	core, err := readBuild(filepath.Join(coreRun, "build.json"))
	if err != nil {
		return err
	}
	actual, err := coreBinaries(coreRun)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(core.Binaries, actual) {
		return errors.New("Windows配套核心已偏离构建记录")
	}
	records, err := windowsCompanions(gui, core)
	if err != nil {
		return err
	}
	actualGUI, _, err := inspect(app, "displaykvm.exe", projectModule+"/desktop", "windows", records[0].GOARCH, "0", "production")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actualGUI, records[0]) {
		return errors.New("Windows桌面已偏离构建记录")
	}
	name := "displaykvm-desktop-" + gui.VersionLabel + "-windows-" + records[0].GOARCH
	payload, err := preparePayload(repo, runDir, name)
	if err != nil {
		return err
	}
	// 桌面锁定缓存仅作输入；复制后所有离线许可查询均使用本次独占缓存。
	if err := copyTree(filepath.Join(repo, "desktop", ".cache", "gomod"), filepath.Join(runDir, "cache", "gomod")); err != nil {
		return err
	}
	for i := range records {
		source, destName := app, "displaykvm.exe"
		if i > 0 {
			source = filepath.Join(coreRun, filepath.FromSlash(records[i].File))
			destName = "displaykvmd.exe"
			if i == 2 {
				destName = "displaykvm-cli.exe"
			}
		}
		if err := windowsPE(source, records[i].GOARCH, i == 0); err != nil {
			return err
		}
		dest := filepath.Join(payload, destName)
		if err := copyFile(source, dest); err != nil {
			return err
		}
		hash, err := digest(dest)
		if err != nil {
			return err
		}
		if hash != records[i].SHA256 {
			return errors.New("Windows程序在复制期间变化")
		}
		records[i].File = destName
		if err := collectLicenses(repo, runDir, payload, "licenses/"+strings.TrimSuffix(destName, ".exe"), &records[i], dest); err != nil {
			return err
		}
	}
	readme := "Windows候选版（未签名；硬件、登录与运行兼容仍待实机验收）\n" +
		"版本标签：" + gui.VersionLabel + "；目标：windows-" + records[0].GOARCH + "；源码提交：" + gui.SourceRevision + "\n" +
		"请完整解压到当前用户拥有且可写的本地NTFS目录，不要从ZIP内部或网络共享运行。\n" +
		"系统前提：Windows 10/11及相应架构的Microsoft Edge WebView2 Runtime；本包不包含运行时安装器、显卡驱动或通用安装器。\n" +
		"首次启动：先在终端运行 .\\displaykvmd.exe（按需提供其已有-config参数），保持后台运行，再双击displaykvm.exe打开桌面。\n" +
		"displaykvm-cli.exe是命令行入口；三个程序须保持同目录，不得互相改名替换。\n" +
		"登录启动在桌面设置中分别管理后台与桌面；手动启动不自动登记登录项。\n" +
		"archive_label为包装标签，未注入产品运行时版本；完整构建信息/来源及许可见manifest.json、licenses/，文件校验见SHA256SUMS。\n"
	if err := writeFile(filepath.Join(payload, "README.txt"), []byte(readme)); err != nil {
		return err
	}
	manifest := packageRecord{Schema: 1, Label: gui.VersionLabel, Status: "candidate; unsigned; Windows runtime/login/hardware not verified", App: map[string]string{"target": "windows-" + records[0].GOARCH, "source_revision": gui.SourceRevision, "source_modified": fmt.Sprint(gui.SourceModified), "source_content_sha256": gui.SourceContentSHA256, "gui_build_run": filepath.Base(filepath.Dir(app)), "core_build_run": filepath.Base(coreRun), "minimum_windows_declaration": "Windows 10/11; matching WebView2 Runtime required; runtime not verified"}, Binaries: records}
	if err := finishPayload(payload, manifest); err != nil {
		return err
	}
	archive, err := makeArchive(payload, filepath.Join(runDir, "dist"), true)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(runDir, "dist", "archives.json"), []archiveRecord{archive})
}
