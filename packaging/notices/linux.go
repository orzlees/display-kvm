package main

import (
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 只接受已审核提交的归档摘要；构建机和外部manifest均无权自行升级工具版本。
var linuxSourcePins = map[string]struct{ version, commit, sha256, repository string }{
	"ddcutil": {"3.0.2", "5085ae9bb1a8e5bd8db717d80dd80a279a0f05fb", "c5c271131ba40e009f0a71a04e8856f4ca4647e9e22d38e23344fdc7714fcc28", "rockowitz/ddcutil"},
	"systemd": {"258", "781d9d0789379d1ea1f2ecefb804d41e9c8b6c38", "9421b006c0e4c816fe06c004204091b09fd628891403dcdf6511ceb8ce384f13", "systemd/systemd"},
}

type linuxArchive struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

type linuxSource struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Version        string         `json:"version"`
	Archives       []linuxArchive `json:"archives"`
	BinaryArchives []linuxArchive `json:"binary_archives"`
	Licenses       []licenseFile  `json:"licenses"`
}

type linuxTool struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
	Source  string `json:"source"`
}

type linuxLibrary struct {
	File           string `json:"file"`
	SHA256         string `json:"sha256"`
	OriginalSHA256 string `json:"original_sha256"`
	Package        string `json:"package"`
	Version        string `json:"version"`
	Architecture   string `json:"architecture"`
	Source         string `json:"source"`
	PackageArchive string `json:"package_archive"`
	BuildPath      string `json:"build_path,omitempty"`
}

type linuxPayloadManifest struct {
	Schema    int            `json:"schema_version"`
	GOOS      string         `json:"goos"`
	GOARCH    string         `json:"goarch"`
	Baseline  string         `json:"baseline"`
	Tools     []linuxTool    `json:"tools"`
	Libraries []linuxLibrary `json:"libraries"`
	Sources   []linuxSource  `json:"sources"`
	Files     []licenseFile  `json:"files"`
}

var linuxSystemLibraries = map[string]bool{
	"libc.so.6": true, "libm.so.6": true, "libdl.so.2": true, "libpthread.so.0": true,
	"librt.so.1": true, "libresolv.so.2": true, "libutil.so.1": true,
	"ld-linux-x86-64.so.2": true, "ld-linux-aarch64.so.1": true,
}

func linuxPath(value string) bool {
	return value != "" && value != "." && path.Clean(value) == value && !path.IsAbs(value) &&
		value != ".." && !strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\\\x00\r\n")
}

func linuxHash(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 32 && strings.ToLower(value) == value
}

// 清单列出所有普通文件，复制件也重新核验；未知文件可能是构建缓存或个人配置，不能顺带分发。
func linuxInventory(directory string, files []licenseFile) error {
	allowed := map[string]bool{"bundled-tools.json": true}
	for _, file := range files {
		if !linuxPath(file.File) || !linuxHash(file.SHA256) || allowed[file.File] {
			return errors.New("Linux载荷清单含重复、越界路径或无效摘要")
		}
		allowed[file.File] = true
		name := filepath.Join(directory, filepath.FromSlash(file.File))
		info, err := regular(name)
		if err != nil || info.Size() == 0 {
			return fmt.Errorf("Linux载荷文件缺失、为空或含链接：%s", file.File)
		}
		hash, err := digest(name)
		if err != nil || hash != file.SHA256 {
			return fmt.Errorf("Linux载荷文件摘要不匹配：%s", file.File)
		}
	}
	return filepath.WalkDir(directory, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := noLinks(name); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(directory, name)
		if err != nil || !entry.Type().IsRegular() || !allowed[filepath.ToSlash(rel)] {
			return errors.New("Linux载荷含未登记文件或特殊文件")
		}
		return nil
	})
}

var linuxGLIBCVersion = regexp.MustCompile(`GLIBC_([0-9]+(?:\.[0-9]+)+)|GLIBC_PRIVATE`)

// 包装在macOS也可静态检查ELF，不执行跨架构程序或ldd。只有原生生成器对本次可信编译结果执行ldd。
func inspectLinuxELF(name, arch string, program bool) ([]string, error) {
	info, err := regular(name)
	if err != nil {
		return nil, err
	}
	image, err := elf.Open(name)
	if err != nil {
		return nil, err
	}
	defer image.Close()
	machine, loader := elf.EM_X86_64, "/lib64/ld-linux-x86-64.so.2"
	if arch == "arm64" {
		machine, loader = elf.EM_AARCH64, "/lib/ld-linux-aarch64.so.1"
	} else if arch != "amd64" {
		return nil, errors.New("未知Linux架构")
	}
	if image.Class != elf.ELFCLASS64 || image.Data != elf.ELFDATA2LSB || image.Machine != machine ||
		(image.Type != elf.ET_DYN && (!program || image.Type != elf.ET_EXEC)) || image.Section(".go.buildinfo") != nil || program && info.Mode()&0111 == 0 {
		return nil, errors.New("Linux工具或共享库的架构、类型、权限无效")
	}
	runpath, err := image.DynString(elf.DT_RUNPATH)
	want := "$ORIGIN"
	if program {
		want = "$ORIGIN/lib"
	}
	oldRpath, oldErr := image.DynString(elf.DT_RPATH)
	if err != nil || oldErr != nil || len(oldRpath) != 0 || len(runpath) != 1 || runpath[0] != want {
		return nil, errors.New("Linux载荷必须只使用受控相对RUNPATH")
	}
	if !program {
		sonames, err := image.DynString(elf.DT_SONAME)
		if err != nil || len(sonames) != 1 || sonames[0] != filepath.Base(name) {
			return nil, errors.New("Linux共享库SONAME必须与随包文件名唯一匹配")
		}
	}
	interp := ""
	for _, segment := range image.Progs {
		if segment.Type == elf.PT_INTERP {
			data, err := io.ReadAll(io.LimitReader(segment.Open(), 4096))
			if err != nil || interp != "" {
				return nil, errors.New("ELF加载器字段无效")
			}
			interp = strings.TrimSuffix(string(data), "\x00")
		}
	}
	if program && interp != loader || !program && interp != "" {
		return nil, errors.New("ELF加载器不符合目标基线")
	}
	// Debian12/glibc2.36是本配方唯一构建边界；较新runner不能通过改清单隐藏ABI抬升。
	for _, section := range image.Sections {
		if section.Type != elf.SHT_STRTAB {
			continue
		}
		if section.Size > 32<<20 {
			return nil, errors.New("ELF字符串表过大")
		}
		data, err := section.Data()
		if err != nil {
			return nil, err
		}
		for _, match := range linuxGLIBCVersion.FindAllStringSubmatch(string(data), -1) {
			parts := strings.Split(match[1], ".")
			if len(parts) < 2 {
				return nil, errors.New("不接受GLIBC_PRIVATE依赖")
			}
			major, _ := strconv.Atoi(parts[0])
			minor, _ := strconv.Atoi(parts[1])
			if major > 2 || major == 2 && (minor > 36 || minor == 36 && len(parts) > 2 && strings.Trim(strings.Join(parts[2:], ""), "0") != "") {
				return nil, errors.New("ELF符号要求超过glibc2.36基线")
			}
		}
	}
	dependencies, err := image.ImportedLibraries()
	if err != nil || len(dependencies) == 0 {
		return nil, errors.New("Linux载荷必须有可检查的动态依赖")
	}
	for _, dependency := range dependencies {
		if !linuxPath(dependency) || strings.Contains(dependency, "/") {
			return nil, errors.New("ELF依赖含绝对路径或目录")
		}
		if strings.HasPrefix(dependency, "ld-linux-") && dependency != path.Base(loader) {
			return nil, errors.New("ELF依赖含错误架构的系统加载器")
		}
	}
	return dependencies, nil
}

func linuxClosure(objects map[string][]string, libraries map[string]bool) error {
	reached := map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		if reached[name] {
			return nil
		}
		reached[name] = true
		for _, dependency := range objects[name] {
			if linuxSystemLibraries[dependency] {
				continue
			}
			if !libraries[dependency] {
				return fmt.Errorf("Linux载荷缺少非基线依赖：%s", dependency)
			}
			if err := visit(dependency); err != nil {
				return err
			}
		}
		return nil
	}
	for name := range objects {
		if !libraries[name] {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	for name := range libraries {
		if !reached[name] {
			return fmt.Errorf("Linux载荷包含未被工具使用的共享库：%s", name)
		}
	}
	return nil
}

// Debian源码的.dsc列出原始源码及发行版补丁，不能只保留版权文件或单个上游tar包；
// 逐项核对原有Checksums-Sha256和长度，确保再分发的源码与所记录发行版版本完整对应。
func linuxDebianSource(directory string, source linuxSource) error {
	archives := map[string]linuxArchive{}
	dsc := ""
	for _, archive := range source.Archives {
		name := path.Base(archive.File)
		if archives[name].File != "" {
			return errors.New("Debian源码包含同名归档")
		}
		archives[name] = archive
		if strings.HasSuffix(name, ".dsc") {
			if dsc != "" {
				return errors.New("Debian源码描述文件不唯一")
			}
			dsc = archive.File
		}
	}
	if dsc == "" {
		return errors.New("Debian源码缺少原始.dsc描述文件")
	}
	info, err := regular(filepath.Join(directory, filepath.FromSlash(dsc)))
	if err != nil || info.Size() > 4<<20 {
		return errors.New("Debian源码描述文件无效")
	}
	data, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(dsc)))
	if err != nil {
		return err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.Contains("\n"+text, "\nSource: "+source.Name+"\n") || !strings.Contains("\n"+text, "\nVersion: "+source.Version+"\n") {
		return errors.New("Debian源码描述版本与运行库来源不符")
	}
	checking, count := false, 0
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if line == "Checksums-Sha256:" {
			checking = true
			continue
		}
		if !checking {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			break
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return errors.New("Debian源码摘要字段格式无效")
		}
		archive := archives[fields[2]]
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size <= 0 || seen[fields[2]] || archive.File == "" || archive.File == dsc || archive.SHA256 != fields[0] {
			return errors.New("Debian源码原始归档或补丁不完整")
		}
		info, err := regular(filepath.Join(directory, filepath.FromSlash(archive.File)))
		if err != nil || info.Size() != size {
			return errors.New("Debian源码归档长度不符")
		}
		seen[fields[2]] = true
		count++
	}
	if count == 0 || count != len(archives)-1 || len(source.BinaryArchives) == 0 {
		return errors.New("Debian源码未覆盖完整归档和对应运行库原包")
	}
	return nil
}

var linuxBuildLibraryPath = regexp.MustCompile(`^[A-Za-z0-9_+./@-]+$`)

// 来源归档、许可文件及固定上游摘要已由linuxBundle统一核验；此处区分两种库的追溯关系。
// systemd私库来自同一次固定源码构建，没有Debian原包，不能伪造PackageArchive来绕过来源约束；
// build_path只记录原构建树内的相对位置，包装时不访问构建机目录，载荷仍必须通过ELF和完整闭包检查。
func linuxLibrarySource(library linuxLibrary, source linuxSource) error {
	if source.ID == "" || source.ID != library.Source || source.Name == "" || source.Version == "" ||
		len(source.Archives) == 0 || len(source.Licenses) == 0 || !linuxHash(library.OriginalSHA256) || library.Version == "" {
		return errors.New("Linux共享库缺少原始摘要、对应源码或许可来源")
	}
	if source.ID == "systemd" {
		if source.Name != "systemd" || source.Version != linuxSourcePins["systemd"].version || library.Version != source.Version ||
			len(source.BinaryArchives) != 0 || library.Package != "" || library.PackageArchive != "" ||
			!linuxPath(library.BuildPath) || !linuxBuildLibraryPath.MatchString(library.BuildPath) {
			return errors.New("systemd构建库必须来自固定源码和受限相对构建路径，不能伪造Debian原包")
		}
		return nil
	}
	packageSeen := false
	for _, archive := range source.BinaryArchives {
		packageSeen = packageSeen || archive.File == library.PackageArchive
	}
	if !strings.HasPrefix(source.ID, "debian-") || library.BuildPath != "" || library.Package == "" || library.PackageArchive == "" || !packageSeen {
		return errors.New("Debian共享库必须匹配精确原包，不能借构建路径替代来源")
	}
	return nil
}

func linuxBundle(directory, arch string) (*linuxPayloadManifest, error) {
	manifestPath := filepath.Join(directory, "bundled-tools.json")
	if _, err := regular(manifestPath); err != nil {
		return nil, err
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 4<<20))
	decoder.DisallowUnknownFields()
	var manifest linuxPayloadManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || manifest.Schema != 1 || manifest.GOOS != "linux" || manifest.GOARCH != arch || manifest.Baseline != "debian-12-glibc-2.36" || len(manifest.Tools) != 3 || len(manifest.Libraries) == 0 || len(manifest.Sources) < 3 {
		return nil, errors.New("Linux随包清单缺少真实工具、依赖、源码或目标基线")
	}
	if err := linuxInventory(directory, manifest.Files); err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, file := range manifest.Files {
		files[file.File] = file.SHA256
	}
	for _, required := range []string{"recipe/bundle.py", "recipe/source-lock.json", "recipe/environment.json"} {
		if files[required] == "" {
			return nil, errors.New("Linux载荷缺少完整生成配方或环境记录")
		}
	}
	sources := map[string]linuxSource{}
	for _, source := range manifest.Sources {
		if source.ID == "" || source.Name == "" || source.Version == "" || sources[source.ID].ID != "" || len(source.Archives) == 0 || len(source.Licenses) == 0 {
			return nil, errors.New("Linux源码来源记录不完整或重复")
		}
		for _, archive := range append(append([]linuxArchive{}, source.Archives...), source.BinaryArchives...) {
			u, err := url.Parse(archive.URL)
			if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(archive.File, "sources/") || !linuxHash(archive.SHA256) || files[archive.File] != archive.SHA256 {
				return nil, errors.New("Linux源码归档缺失、来源URL或摘要无效")
			}
			if source.ID != "ddcutil" && source.ID != "systemd" && u.Host != "deb.debian.org" && u.Host != "security.debian.org" {
				return nil, errors.New("运行库只接受Debian官方来源")
			}
		}
		for _, license := range source.Licenses {
			if !strings.HasPrefix(license.File, "licenses/") || !linuxHash(license.SHA256) || files[license.File] != license.SHA256 {
				return nil, errors.New("Linux来源缺少完整许可文件")
			}
		}
		if source.ID != "ddcutil" && source.ID != "systemd" {
			if err := linuxDebianSource(directory, source); err != nil {
				return nil, err
			}
		}
		sources[source.ID] = source
	}
	for id, pin := range linuxSourcePins {
		source := sources[id]
		if source.Name != id || source.Version != pin.version || len(source.Archives) != 1 || source.Archives[0].SHA256 != pin.sha256 || source.Archives[0].URL != "https://codeload.github.com/"+pin.repository+"/tar.gz/"+pin.commit || len(source.BinaryArchives) != 0 {
			return nil, errors.New("Linux工具的固定源码来源与审核版本不符")
		}
	}
	objects, libraries := map[string][]string{}, map[string]bool{}
	for _, tool := range manifest.Tools {
		id := "systemd"
		if tool.Name == "ddcutil" {
			id = "ddcutil"
		} else if tool.Name != "busctl" && tool.Name != "udevadm" {
			return nil, errors.New("Linux载荷包含未知工具")
		}
		if tool.File != tool.Name || tool.Source != id || tool.Version != linuxSourcePins[id].version || !linuxHash(tool.SHA256) || files[tool.File] != tool.SHA256 || objects[tool.Name] != nil {
			return nil, errors.New("Linux工具来源、摘要或路径不符")
		}
		objects[tool.Name], err = inspectLinuxELF(filepath.Join(directory, tool.File), arch, true)
		if err != nil {
			return nil, fmt.Errorf("Linux工具%s无效：%w", tool.Name, err)
		}
	}
	for _, library := range manifest.Libraries {
		name := path.Base(library.File)
		source := sources[library.Source]
		if library.File != "lib/"+name || linuxSystemLibraries[name] || libraries[name] || files[library.File] != library.SHA256 || !linuxHash(library.SHA256) || library.Architecture != arch {
			return nil, errors.New("Linux共享库缺少架构、对应源码、原包或许可来源")
		}
		if err := linuxLibrarySource(library, source); err != nil {
			return nil, fmt.Errorf("Linux共享库%s来源无效：%w", name, err)
		}
		libraries[name] = true
		objects[name], err = inspectLinuxELF(filepath.Join(directory, filepath.FromSlash(library.File)), arch, false)
		if err != nil {
			return nil, fmt.Errorf("Linux共享库%s无效：%w", name, err)
		}
	}
	if err := linuxClosure(objects, libraries); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func stageLinux(runDir, sourceRun string, binaries []binaryRecord) error {
	needed := map[string]bool{}
	for _, binary := range binaries {
		if binary.GOOS == "linux" {
			needed[binary.GOARCH] = true
		}
	}
	if len(needed) == 0 {
		if sourceRun != "" {
			return errors.New("非Linux核心构建不接受Linux载荷")
		}
		return nil
	}
	if sourceRun == "" {
		return errors.New("Linux核心构建必须提供已完成的原生工具载荷，不能仅用Go交叉编译冒充完整Linux包")
	}
	sourceRun = filepath.Clean(sourceRun)
	if filepath.Dir(sourceRun) != filepath.Dir(runDir) || sourceRun == runDir || !validLabel(filepath.Base(sourceRun)) {
		return errors.New("Linux载荷必须来自build/out下另一个独占原生生成编号")
	}
	marker := filepath.Join(sourceRun, ".displaykvm-run")
	if _, err := regular(marker); err != nil {
		return err
	}
	owner, err := os.ReadFile(marker)
	if err != nil || string(owner) != "displaykvm-linux-tools-v1\n" {
		return errors.New("Linux原生载荷目录身份不匹配")
	}
	for arch := range needed {
		source := filepath.Join(sourceRun, "linux", arch)
		if _, err := linuxBundle(source, arch); err != nil {
			return err
		}
		dest := filepath.Join(runDir, "linux", arch)
		if err := copyTree(source, dest); err != nil {
			return err
		}
		if _, err := linuxBundle(dest, arch); err != nil {
			return err
		}
	}
	return nil
}

// 构建记录锁定每个原生清单，防止包装阶段换入另一组虽合法但与本次构建不同的库版本。
func linuxBuildRecords(runDir string, binaries []binaryRecord) ([]licenseFile, error) {
	var records []licenseFile
	seen := map[string]bool{}
	for _, binary := range binaries {
		if binary.GOOS != "linux" || seen[binary.GOARCH] {
			continue
		}
		seen[binary.GOARCH] = true
		directory := filepath.Join(runDir, "linux", binary.GOARCH)
		if _, err := linuxBundle(directory, binary.GOARCH); err != nil {
			return nil, err
		}
		file := "linux/" + binary.GOARCH + "/bundled-tools.json"
		hash, err := digest(filepath.Join(runDir, filepath.FromSlash(file)))
		if err != nil {
			return nil, err
		}
		records = append(records, licenseFile{File: file, SHA256: hash})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].File < records[j].File })
	return records, nil
}

// Linux原生载荷与Go程序共用归档根目录。只复制已登记文件，不能用整树复制带入并发新增的文件；
// 复制后逐个核对摘要，以便工具仍与实际可执行文件相邻，并沿用现有拒绝链接和独占写入边界。
func packageLinux(source, dest, arch string) (*linuxPayloadManifest, error) {
	manifest, err := linuxBundle(source, arch)
	if err != nil {
		return nil, err
	}
	manifestHash, err := digest(filepath.Join(source, "bundled-tools.json"))
	if err != nil {
		return nil, err
	}
	files := append(append([]licenseFile{}, manifest.Files...), licenseFile{File: "bundled-tools.json", SHA256: manifestHash})
	for _, file := range files {
		target := filepath.Join(dest, filepath.FromSlash(file.File))
		if err := noLinks(dest); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return nil, err
		}
		if err := copyFile(filepath.Join(source, filepath.FromSlash(file.File)), target); err != nil {
			return nil, err
		}
		hash, err := digest(target)
		if err != nil || hash != file.SHA256 {
			return nil, errors.New("Linux载荷在包装复制期间变化")
		}
	}
	return manifest, nil
}
