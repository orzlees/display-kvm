// notices只服务离线构建与许可收集，不属于产品入口，也不执行目标二进制。
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

const toolchain = "go1.27.3"
const projectModule = "github.com/orzlee/displayKvm"

type licenseFile struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

// 与third_party/m1ddc/SOURCE.json的固定原字节对应；不能只信随包清单自报的哈希，
// 否则替换来源或许可后再重写清单仍会被接受。更新版本须同时审核源码、recipe与这些pin。
const m1ddcSourceSHA256 = "148224cba5b8e6b573db67d62570b24763fc78580a76152413e53d140a750407"
const m1ddcLicenseSHA256 = "b01d3d1de4baa142bb3746588e70814d04eae071d104f8aff580f88e30bb4547"

var m1ddcUpstreamFiles = [...]licenseFile{
	{File: "Makefile", SHA256: "336e0bd09cd6328fd9dfae08a018a9b349cfe25000c6a95f135da92df7a5c01d"},
	{File: "LICENSE", SHA256: m1ddcLicenseSHA256},
	{File: "README.md", SHA256: "d24debe644f97bfc92d309a2105e0248d2119359bb31380d7c5b1e7096f1a730"},
	{File: "headers/i2c.h", SHA256: "7e0ae57dccbe53d84ddbc4c10f3efc036a06269583b788a854a89f8de81d326d"},
	{File: "headers/ioregistry.h", SHA256: "10a4833144b6168afa978f8ac63da30fe6dc67df24d881f22cb58f8a6e2b5ad2"},
	{File: "headers/utils.h", SHA256: "fcd87a2495f5bb7ca1d86253e3bab7ee1758f939c079106bf81c8bcaf6599b64"},
	{File: "sources/i2c.m", SHA256: "0f21bfb03a9f851f4dec72d6df1eae1809fe8ec49b0b74d3f4b9be601c2902d6"},
	{File: "sources/ioregistry.m", SHA256: "39bbc789169aa1d7132bfc3da6005f8e5aeb41052195992181e7c61addcfe0a8"},
	{File: "sources/m1ddc.m", SHA256: "37794b3c9d0831796496a11500f33b12c3c1a05f781d94c90c9762727b50f678"},
}

type moduleRecord struct {
	Path        string        `json:"path"`
	Version     string        `json:"version"`
	Sum         string        `json:"sum,omitempty"`
	Replacement *moduleRecord `json:"replacement,omitempty"`
	Licenses    []licenseFile `json:"licenses,omitempty"`
}

type binaryRecord struct {
	File         string            `json:"file"`
	SHA256       string            `json:"sha256"`
	GoVersion    string            `json:"go_version"`
	Entry        string            `json:"entry"`
	GOOS         string            `json:"goos"`
	GOARCH       string            `json:"goarch"`
	CGO          string            `json:"cgo_enabled"`
	Tags         string            `json:"tags"`
	Settings     map[string]string `json:"settings"`
	Main         moduleRecord      `json:"main"`
	Dependencies []moduleRecord    `json:"dependencies"`
	GoLicense    []licenseFile     `json:"go_license,omitempty"`
}

// 原生工具不具备Go构建信息，单独记录固定来源和签名后的真实字节；不能将其混入Go入口清单。
// 来源清单与完整许可沿用file/sha256结构，复制到归档后仍可逐项复核，且不泄露本机源码路径。
type nativeToolRecord struct {
	Name           string        `json:"name"`
	File           string        `json:"file"`
	SHA256         string        `json:"sha256"`
	GOOS           string        `json:"goos"`
	GOARCH         string        `json:"goarch"`
	Version        string        `json:"version"`
	Repository     string        `json:"repository"`
	Tag            string        `json:"tag"`
	Commit         string        `json:"commit"`
	License        string        `json:"license"`
	SourceManifest licenseFile   `json:"source_manifest"`
	Licenses       []licenseFile `json:"licenses"`
	Sources        []licenseFile `json:"sources"`
}

type nativeToolManifest struct {
	Schema int                `json:"schema_version"`
	Tools  []nativeToolRecord `json:"tools"`
}

type buildRecord struct {
	Schema       int            `json:"schema_version"`
	Toolchain    string         `json:"toolchain"`
	VersionLabel string         `json:"archive_label"`
	Binaries     []binaryRecord `json:"binaries"`
}

type packageRecord struct {
	Schema      int                `json:"schema_version"`
	Label       string             `json:"archive_label"`
	Status      string             `json:"status"`
	App         map[string]string  `json:"app,omitempty"`
	Binaries    []binaryRecord     `json:"binaries"`
	NativeTools []nativeToolRecord `json:"native_tools,omitempty"`
}

type listModule struct {
	Path    string
	Version string
	Sum     string
	Dir     string
	Replace *listModule
	Error   *struct{ Err string }
}

type listPackage struct {
	ImportPath string
	Dir        string
	Module     *listModule
	Error      *struct{ Err string }
	DepsErrors []struct{ Err string }
}

type archiveRecord struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "包装准备失败：", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("缺少record-core、core或desktop模式")
	}
	flags := flag.NewFlagSet("notices", flag.ContinueOnError)
	repoFlag := flags.String("repo", "", "项目根")
	runFlag := flags.String("run", "", "本次独占目录")
	version := flags.String("version", "", "归档标签，不注入程序版本")
	app := flags.String("app", "", "既有macOS应用包")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("不接受位置参数")
	}
	repo, err := filepath.Abs(*repoFlag)
	if err != nil || *repoFlag == "" {
		return errors.New("缺少项目根")
	}
	runDir, err := filepath.Abs(*runFlag)
	if err != nil || *runFlag == "" {
		return errors.New("缺少运行目录")
	}
	if filepath.Dir(runDir) != filepath.Join(repo, "build", "out") || !validLabel(filepath.Base(runDir)) {
		return errors.New("运行目录必须为build/out下的单个合法编号")
	}
	if err := noLinks(runDir); err != nil {
		return err
	}
	marker := filepath.Join(runDir, ".displaykvm-run")
	if _, err := regular(marker); err != nil {
		return err
	}
	owner, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	expectedOwner := "displaykvm-core-v1\n"
	if os.Args[1] == "desktop" {
		expectedOwner = "displaykvm-desktop-v1\n"
	}
	if string(owner) != expectedOwner {
		return errors.New("运行目录身份不匹配")
	}
	if runtime.Version() != toolchain {
		return errors.New("辅助程序必须由Go1.27.3构建")
	}
	actual, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil || strings.TrimSpace(string(actual)) != toolchain {
		return errors.New("当前Go与实际二进制工具链必须一致为Go1.27.3")
	}
	switch os.Args[1] {
	case "record-core":
		if !validLabel(*version) {
			return errors.New("归档标签无效")
		}
		binaries, err := coreBinaries(runDir)
		if err != nil {
			return err
		}
		return writeJSON(filepath.Join(runDir, "build.json"), buildRecord{1, toolchain, *version, binaries})
	case "core":
		return packageCore(repo, runDir)
	case "desktop":
		return packageDesktop(repo, runDir, *app)
	default:
		return errors.New("未知模式")
	}
}

func validLabel(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	first := value[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9') {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// 每个路径分量都拒绝链接，既不跟随断链，也不接管用户放入的链接目录。
// 目录由shell以mkdir独占；这里不清理失败现场，不存在递归删除入口。
func noLinks(path string) error {
	path = filepath.Clean(path)
	for {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("路径包含符号链接")
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func regular(path string) (fs.FileInfo, error) {
	if err := noLinks(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("只接受普通文件")
	}
	return info, nil
}

func digest(path string) (string, error) {
	if _, err := regular(path); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFile(path string, data []byte) error {
	if err := noLinks(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Close())
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
}

func copyFile(source, dest string) error {
	info, err := regular(source)
	if err != nil {
		return err
	}
	if err := noLinks(filepath.Dir(dest)); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	mode := fs.FileMode(0644)
	if info.Mode()&0111 != 0 {
		mode = 0755
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close())
}

func copyTree(source, dest string) error {
	if err := noLinks(source); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("输入树含链接，拒绝包装或复制缓存")
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dest, rel)
		if entry.IsDir() {
			if err := os.MkdirAll(to, 0755); err != nil {
				return err
			}
			return noLinks(to)
		}
		return copyFile(path, to)
	})
}

func publicModule(module debug.Module) moduleRecord {
	r := moduleRecord{Path: module.Path, Version: module.Version, Sum: module.Sum}
	if module.Replace != nil {
		replacement := publicModule(*module.Replace)
		// 本地替换仅在依赖查询确认是项目核心后接受；清单不泄露绝对源目录。
		if replacement.Version == "" {
			replacement.Path = projectModule
			replacement.Version = "(local project)"
		}
		r.Replacement = &replacement
	}
	return r
}

func inspect(path, publicPath, entry, goos, goarch, cgo, tags string) (binaryRecord, *debug.BuildInfo, error) {
	hash, err := digest(path)
	if err != nil {
		return binaryRecord{}, nil, err
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return binaryRecord{}, nil, err
	}
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if info.GoVersion != toolchain || info.Path != entry || settings["GOOS"] != goos || settings["GOARCH"] != goarch || settings["CGO_ENABLED"] != cgo || settings["-tags"] != tags || settings["-trimpath"] != "true" {
		return binaryRecord{}, nil, errors.New("二进制工具链、入口、目标、CGO、标签或trimpath与构建契约不符")
	}
	if info.Main.Path != projectModule && info.Main.Path != projectModule+"/desktop" {
		return binaryRecord{}, nil, errors.New("二进制不属于项目模块")
	}
	r := binaryRecord{File: filepath.ToSlash(publicPath), SHA256: hash, GoVersion: info.GoVersion, Entry: info.Path, GOOS: goos, GOARCH: goarch, CGO: cgo, Tags: tags, Main: publicModule(info.Main), Settings: make(map[string]string), Dependencies: []moduleRecord{}}
	// 只导出固定白名单，编译器选项中的本机目录或个人环境不会进入可分发清单。
	for _, key := range []string{"-buildmode", "-compiler", "-trimpath", "-tags", "GOOS", "GOARCH", "CGO_ENABLED", "GOAMD64", "GOARM64", "vcs", "vcs.revision", "vcs.time", "vcs.modified"} {
		if value, ok := settings[key]; ok {
			r.Settings[key] = value
		}
	}
	for _, module := range info.Deps {
		r.Dependencies = append(r.Dependencies, publicModule(*module))
	}
	return r, info, nil
}

func coreBinaries(runDir string) ([]binaryRecord, error) {
	root := filepath.Join(runDir, "bin")
	if err := noLinks(root); err != nil {
		return nil, err
	}
	targets, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, errors.New("核心目标为空")
	}
	var records []binaryRecord
	for _, target := range targets {
		parts := strings.Split(target.Name(), "-")
		if !target.IsDir() || len(parts) != 2 || !(parts[0] == "darwin" || parts[0] == "linux" || parts[0] == "windows") || !(parts[1] == "amd64" || parts[1] == "arm64") {
			return nil, errors.New("核心bin中存在未知目标或内容")
		}
		files, err := os.ReadDir(filepath.Join(root, target.Name()))
		if err != nil {
			return nil, err
		}
		if len(files) != 2 {
			return nil, errors.New("每个核心目标必须且只能含CLI和后台")
		}
		for _, program := range []string{"displaykvm", "displaykvmd"} {
			suffix := ""
			if parts[0] == "windows" {
				suffix = ".exe"
			}
			rel := filepath.Join("bin", target.Name(), program+suffix)
			r, _, err := inspect(filepath.Join(runDir, rel), rel, projectModule+"/cmd/"+program, parts[0], parts[1], "0", "")
			if err != nil {
				return nil, err
			}
			records = append(records, r)
		}
	}
	return records, nil
}

func commandEnv(overrides map[string]string) []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[key]; !replaced {
			env = append(env, item)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func linkedModules(repo, runDir string, record binaryRecord, info *debug.BuildInfo) (map[string]*listModule, error) {
	dir, pkg := repo, "./cmd/"+filepath.Base(record.Entry)
	// 桌面包内后台也启用CGO以保留原生事件能力；CGO不能再用来判断所属模块。
	// 只按已核实的实际入口选择桌面模块，后台和CLI继续从根模块查询各自依赖。
	if record.Entry == projectModule+"/desktop" {
		dir, pkg = filepath.Join(repo, "desktop"), "."
	}
	args := []string{"list", "-deps", "-json", "-mod=readonly", "-trimpath", "-buildvcs=false"}
	if record.Tags != "" {
		args = append(args, "-tags="+record.Tags)
	}
	args = append(args, pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	env := map[string]string{"GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "", "GOEXPERIMENT": "", "GOOS": record.GOOS, "GOARCH": record.GOARCH, "CGO_ENABLED": record.CGO, "GOAMD64": "v1", "GOARM64": "v8.0", "GOMODCACHE": filepath.Join(runDir, "cache", "gomod"), "GOCACHE": filepath.Join(runDir, "cache", "build"), "GOTMPDIR": filepath.Join(runDir, "cache", "tmp"), "TMPDIR": filepath.Join(runDir, "cache", "tmp")}
	if record.CGO == "1" {
		env["MACOSX_DEPLOYMENT_TARGET"] = "13.0"
		env["CGO_CFLAGS"], env["CGO_LDFLAGS"] = "-mmacosx-version-min=13.0", "-mmacosx-version-min=13.0"
		for _, setting := range info.Settings {
			if (setting.Key == "CGO_CFLAGS" || setting.Key == "CGO_LDFLAGS") && setting.Value != "-mmacosx-version-min=13.0" {
				return nil, errors.New("桌面CGO参数与既有入口不一致")
			}
		}
	}
	cmd.Env = commandEnv(env)
	data, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("离线只读go list失败，不能推测许可载荷；stderr=%q：%w", exit.Stderr, err)
		}
		return nil, fmt.Errorf("离线只读go list失败，不能推测许可载荷：%w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	modules := make(map[string]*listModule)
	entrySeen := false
	for {
		var pkg listPackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if pkg.Error != nil || len(pkg.DepsErrors) != 0 {
			return nil, errors.New("依赖查询含包错误")
		}
		if pkg.ImportPath == record.Entry {
			entrySeen = true
		}
		if pkg.Module != nil {
			// Go查询固定版本失败时仍可能填充Dir、Version和Sum，不能据此认定来源有效。
			// 每条包记录须先拒绝模块及替换错误，避免后续同模块记录覆盖失败证据。
			if pkg.Module.Error != nil {
				return nil, fmt.Errorf("模块%s@%s依赖查询失败：%s", pkg.Module.Path, pkg.Module.Version, pkg.Module.Error.Err)
			}
			if pkg.Module.Replace != nil && pkg.Module.Replace.Error != nil {
				return nil, fmt.Errorf("模块%s@%s的替换%s@%s查询失败：%s", pkg.Module.Path, pkg.Module.Version, pkg.Module.Replace.Path, pkg.Module.Replace.Version, pkg.Module.Replace.Error.Err)
			}
			modules[pkg.Module.Path] = pkg.Module
		}
	}
	if !entrySeen {
		return nil, errors.New("依赖查询未找到二进制入口")
	}
	// BuildInfo给出实际链接模块，go list只负责提供相同参数下的来源与替换目录。
	// 不把桌面go.mod的所有require项冒充载荷；两方无法对应即停止包装。
	for _, module := range append([]*debug.Module{&info.Main}, info.Deps...) {
		found := modules[module.Path]
		if found == nil || found.Version != module.Version && module.Version != "(devel)" || module.Sum != "" && found.Sum != module.Sum {
			return nil, errors.New("实际链接模块与依赖来源不匹配")
		}
		if (module.Replace == nil) != (found.Replace == nil) {
			return nil, errors.New("实际模块替换信息不匹配")
		}
		if module.Replace != nil {
			// 本地核心回指在BuildInfo中记为(devel)，go list则省略替换版本。
			// 仅接受两侧都明确回指本项目根的这一组合，不能推广到第三方或其他替换来源。
			localCoreReplacement := module.Path == projectModule && module.Replace.Path == ".." && found.Replace.Path == ".." &&
				module.Replace.Version == "(devel)" && found.Replace.Version == "" &&
				module.Replace.Sum == "" && found.Replace.Sum == "" && filepath.Clean(found.Replace.Dir) == repo
			if module.Replace.Path != found.Replace.Path || module.Replace.Version != found.Replace.Version && !localCoreReplacement || module.Replace.Sum != "" && module.Replace.Sum != found.Replace.Sum {
				return nil, errors.New("实际替换模块与来源不匹配")
			}
		}
	}
	return modules, nil
}

func addLicense(source, payload, rel string) (licenseFile, error) {
	dest := filepath.Join(payload, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return licenseFile{}, err
	}
	if _, err := os.Lstat(dest); err == nil {
		// 桌面主模块与本地替换核心共享项目许可；仅复用字节相同的既有副本，不覆盖。
		existing, err := digest(dest)
		if err != nil {
			return licenseFile{}, err
		}
		original, err := digest(source)
		if err != nil {
			return licenseFile{}, err
		}
		if existing != original {
			return licenseFile{}, errors.New("重复许可目标内容不一致")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := copyFile(source, dest); err != nil {
			return licenseFile{}, err
		}
	} else {
		return licenseFile{}, err
	}
	hash, err := digest(dest)
	if err != nil {
		return licenseFile{}, err
	}
	info, err := os.Stat(dest)
	if err != nil || info.Size() == 0 {
		return licenseFile{}, errors.New("许可文件为空")
	}
	return licenseFile{filepath.ToSlash(rel), hash}, nil
}

func moduleLicenses(repo, runDir, payload, prefix string, source *listModule) ([]licenseFile, error) {
	actual := source
	if source.Replace != nil {
		if source.Replace.Version == "" && source.Path != projectModule {
			return nil, errors.New("除核心回指外不接受未锁定的本地模块替换")
		}
		actual = source.Replace
	}
	if source.Path == projectModule || source.Path == projectModule+"/desktop" {
		// 本地replace仅允许核心回指项目根，拒绝从任意私人目录借用许可。
		expected := repo
		if source.Path == projectModule+"/desktop" {
			expected = filepath.Join(repo, "desktop")
		}
		if filepath.Clean(actual.Dir) != expected {
			return nil, errors.New("项目模块目录与许可来源不一致")
		}
		file, err := addLicense(filepath.Join(repo, "LICENSE"), payload, prefix+"/project/LICENSE")
		return []licenseFile{file}, err
	}
	cache := filepath.Join(runDir, "cache", "gomod")
	rel, err := filepath.Rel(cache, actual.Dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, errors.New("第三方模块未来自本次隔离模块缓存")
	}
	if err := noLinks(actual.Dir); err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(actual.Path + "@" + actual.Version))
	base := prefix + "/modules/" + hex.EncodeToString(key[:8])
	var files []licenseFile
	licenseSeen := false
	err = filepath.WalkDir(actual.Dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("模块含链接，许可来源不确定")
		}
		if entry.IsDir() {
			return nil
		}
		name := strings.ToUpper(entry.Name())
		isLicense := strings.HasPrefix(name, "LICENSE") || strings.HasPrefix(name, "LICENCE") || strings.HasPrefix(name, "COPYING")
		if !isLicense && !strings.HasPrefix(name, "NOTICE") {
			return nil
		}
		rel, err := filepath.Rel(actual.Dir, path)
		if err != nil {
			return err
		}
		file, err := addLicense(path, payload, base+"/"+filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		files = append(files, file)
		licenseSeen = licenseSeen || isLicense
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !licenseSeen {
		return nil, fmt.Errorf("实际模块%s缺少LICENSE/COPYING，停止包装", source.Path)
	}
	return files, nil
}

func collectLicenses(repo, runDir, payload, prefix string, record *binaryRecord, binary string) error {
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return err
	}
	modules, err := linkedModules(repo, runDir, *record, info)
	if err != nil {
		return err
	}
	record.Main.Licenses, err = moduleLicenses(repo, runDir, payload, prefix, modules[info.Main.Path])
	if err != nil {
		return err
	}
	for i, module := range info.Deps {
		record.Dependencies[i].Licenses, err = moduleLicenses(repo, runDir, payload, prefix, modules[module.Path])
		if err != nil {
			return err
		}
	}
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return err
	}
	// Homebrew的受管工具链入口可能是opt链接，只在确认工具链版本后解析其实际根。
	// 解析后的路径只用于本地读取，不写入清单；工件和输出目录仍拒绝全部链接。
	actualRoot, err := filepath.EvalSymlinks(strings.TrimSpace(string(goroot)))
	if err != nil {
		return err
	}
	actualRoot, err = filepath.Abs(actualRoot)
	if err != nil {
		return err
	}
	goLicense := filepath.Join(actualRoot, "LICENSE")
	if _, err := os.Lstat(goLicense); errors.Is(err, os.ErrNotExist) {
		// Homebrew将GOROOT放在同一安装的libexec中，而Go许可保留在安装根。
		// 只有标准位置缺失，且当前Go真实文件、安装根bin/go与目标构建版本
		// 全部对应时才读取该根的LICENSE；不能向任意上级回溯而误收其他软件许可。
		if filepath.Base(actualRoot) != "libexec" {
			return fmt.Errorf("Go工具链缺少GOROOT/LICENSE：%w", err)
		}
		installRoot := filepath.Dir(actualRoot)
		goPath, err := exec.LookPath("go")
		if err != nil {
			return err
		}
		goPath, err = filepath.Abs(goPath)
		if err != nil {
			return err
		}
		activeGo, err := filepath.EvalSymlinks(goPath)
		if err != nil {
			return err
		}
		installedGo, err := filepath.EvalSymlinks(filepath.Join(installRoot, "bin", "go"))
		if err != nil {
			return err
		}
		// Homebrew的bin/go入口本身也可能是链接，身份判断须统一比较实际文件。
		// 同版本并不证明同源；还需与真实GOROOT内的Go对应且不得逃逸该目录。
		rootGo, err := filepath.EvalSymlinks(filepath.Join(actualRoot, "bin", "go"))
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(actualRoot, rootGo)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return errors.New("GOROOT内的Go实际文件逃逸工具链目录，停止收集许可")
		}
		if activeGo != installedGo || activeGo != rootGo {
			return errors.New("libexec与当前Go不属于同一工具链安装，不能借用上级许可")
		}
		activeInfo, err := buildinfo.ReadFile(activeGo)
		if err != nil {
			return err
		}
		if activeInfo.GoVersion != toolchain || activeInfo.GoVersion != info.GoVersion {
			return errors.New("Go安装与实际二进制构建版本不一致，停止收集许可")
		}
		goLicense = filepath.Join(installRoot, "LICENSE")
	} else if err != nil {
		return err
	}
	file, err := addLicense(goLicense, payload, prefix+"/go/LICENSE")
	if err != nil {
		return err
	}
	record.GoLicense = []licenseFile{file}
	return nil
}

func preparePayload(repo, runDir, name string) (string, error) {
	payload := filepath.Join(runDir, "dist", "payload", name)
	if err := os.MkdirAll(filepath.Dir(payload), 0755); err != nil {
		return "", err
	}
	if err := noLinks(filepath.Dir(payload)); err != nil {
		return "", err
	}
	if err := os.Mkdir(payload, 0755); err != nil {
		return "", err
	}
	for _, file := range []struct{ source, dest string }{{filepath.Join(repo, "LICENSE"), "LICENSE"}, {filepath.Join(repo, "packaging", "NOTICE"), "NOTICE"}} {
		if err := copyFile(file.source, filepath.Join(payload, file.dest)); err != nil {
			return "", err
		}
	}
	return payload, nil
}

func packageCore(repo, runDir string) error {
	path := filepath.Join(runDir, "build.json")
	if _, err := regular(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var build buildRecord
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&build); err != nil {
		return err
	}
	actual, err := coreBinaries(runDir)
	if err != nil {
		return err
	}
	if build.Schema != 1 || build.Toolchain != toolchain || !validLabel(build.VersionLabel) || !reflect.DeepEqual(build.Binaries, actual) {
		return errors.New("核心构建记录与实际二进制不一致")
	}
	var archives []archiveRecord
	for i := 0; i < len(actual); i += 2 {
		target := actual[i].GOOS + "-" + actual[i].GOARCH
		name := "displaykvm-" + build.VersionLabel + "-" + target
		payload, err := preparePayload(repo, runDir, name)
		if err != nil {
			return err
		}
		records := append([]binaryRecord(nil), actual[i:i+2]...)
		for j := range records {
			source := filepath.Join(runDir, filepath.FromSlash(records[j].File))
			records[j].File = filepath.Base(records[j].File)
			binary := filepath.Join(payload, records[j].File)
			if err := copyFile(source, binary); err != nil {
				return err
			}
			copiedHash, err := digest(binary)
			if err != nil {
				return err
			}
			if copiedHash != records[j].SHA256 {
				return errors.New("二进制在包装复制期间变化，停止生成归档")
			}
			if err := collectLicenses(repo, runDir, payload, "licenses/"+strings.TrimSuffix(records[j].File, ".exe"), &records[j], binary); err != nil {
				return err
			}
		}
		manifest := packageRecord{Schema: 1, Label: build.VersionLabel, Status: "candidate; source versions are actual build info; runtime/signing/hardware not verified", Binaries: records}
		if err := finishPayload(payload, manifest); err != nil {
			return err
		}
		archive, err := makeArchive(payload, filepath.Join(runDir, "dist"), actual[i].GOOS == "windows")
		if err != nil {
			return err
		}
		archives = append(archives, archive)
	}
	return writeJSON(filepath.Join(runDir, "dist", "archives.json"), archives)
}

func readPlist(path string) (map[string]string, error) {
	if _, err := regular(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	decoder := xml.NewDecoder(f)
	values := make(map[string]string)
	key := ""
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local == "key" {
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return nil, err
			}
		} else if start.Name.Local == "string" && key != "" {
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return nil, err
			}
			if _, duplicate := values[key]; duplicate {
				return nil, errors.New("Info.plist字段重复")
			}
			values[key], key = value, ""
		}
	}
	return values, nil
}

func desktopAppPath(repo, app string) (string, error) {
	if !filepath.IsAbs(app) {
		app = filepath.Join(repo, app)
	}
	app = filepath.Clean(app)
	legacy := filepath.Join(repo, "desktop", "bin", "DisplayKvm.app")
	if app != legacy {
		// 新构建独占build/out下的一个编号，且与包装run使用不同身份。
		// 不能将任意外部应用或旧核心/包装目录冒充本次桌面构建输入。
		parent := filepath.Dir(app)
		if filepath.Base(app) != "DisplayKvm.app" || filepath.Dir(parent) != filepath.Join(repo, "build", "out") || !validLabel(filepath.Base(parent)) {
			return "", errors.New("仅包装desktop/bin或build/out单个构建编号内的DisplayKvm.app")
		}
		marker := filepath.Join(parent, ".displaykvm-run")
		if _, err := regular(marker); err != nil {
			return "", err
		}
		owner, err := os.ReadFile(marker)
		if err != nil {
			return "", err
		}
		if string(owner) != "displaykvm-desktop-build-v1\n" {
			return "", errors.New("桌面候选构建目录身份不匹配")
		}
	}
	if err := noLinks(app); err != nil {
		return "", err
	}
	info, err := os.Lstat(app)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("桌面应用必须为真实目录")
	}
	return app, nil
}

func desktopBinaries(app string) ([]binaryRecord, error) {
	programs := []struct{ name, entry, module, tags string }{
		{"displaykvm", projectModule + "/desktop", projectModule + "/desktop", "production"},
		{"displaykvmd", projectModule + "/cmd/displaykvmd", projectModule, ""},
		{"displaykvm-cli", projectModule + "/cmd/displaykvm", projectModule, ""},
	}
	// 先检查完整布局，旧单GUI产物必须明确报出缺失程序，不能静默生成不完整清单。
	// 不接受链接、目录或无执行权限的占位文件，也不会执行待包装程序。
	for _, program := range programs {
		path := filepath.Join(app, "Contents", "MacOS", program.name)
		info, err := regular(path)
		if err != nil {
			return nil, fmt.Errorf("桌面程序%s缺失或不是普通文件：%w", program.name, err)
		}
		if info.Mode()&0111 == 0 {
			return nil, fmt.Errorf("桌面程序%s缺少执行权限", program.name)
		}
	}
	var records []binaryRecord
	for _, program := range programs {
		path := filepath.Join(app, "Contents", "MacOS", program.name)
		image, err := macho.Open(path)
		if err != nil {
			return nil, fmt.Errorf("桌面程序%s不是有效Mach-O：%w", program.name, err)
		}
		targetOK := image.Cpu == macho.CpuArm64 && image.Type == macho.TypeExec
		if err := image.Close(); err != nil {
			return nil, err
		}
		if !targetOK {
			return nil, fmt.Errorf("桌面程序%s必须为macOS arm64可执行程序", program.name)
		}
		rel := filepath.Join("DisplayKvm.app", "Contents", "MacOS", program.name)
		record, _, err := inspect(path, rel, program.entry, "darwin", "arm64", "1", program.tags)
		if err != nil {
			return nil, fmt.Errorf("桌面程序%s构建信息无效：%w", program.name, err)
		}
		if record.Main.Path != program.module {
			return nil, fmt.Errorf("桌面程序%s不属于预期模块", program.name)
		}
		records = append(records, record)
	}
	return records, nil
}

func bundledTools(app string) ([]nativeToolRecord, error) {
	path := filepath.Join(app, "Contents", "Resources", "bundled-tools.json")
	if _, err := regular(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	var manifest nativeToolManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("随包工具清单包含多余内容")
	}
	if manifest.Schema != 1 || len(manifest.Tools) != 1 {
		return nil, errors.New("随包工具清单必须且只能包含固定的m1ddc")
	}
	record := manifest.Tools[0]
	want := nativeToolRecord{
		Name: "m1ddc", File: "Contents/MacOS/m1ddc", SHA256: record.SHA256,
		GOOS: "darwin", GOARCH: "arm64", Version: "1.3.1",
		Repository: "https://github.com/waydabber/m1ddc", Tag: "v1.3.1",
		Commit: "7339ceebb03f691b1eaeb8471dca301d215d19c8", License: "MIT",
		SourceManifest: licenseFile{File: "Contents/Resources/licenses/m1ddc/SOURCE.json", SHA256: m1ddcSourceSHA256},
		Licenses:       []licenseFile{{File: "Contents/Resources/licenses/m1ddc/LICENSE", SHA256: m1ddcLicenseSHA256}},
	}
	for _, source := range m1ddcUpstreamFiles {
		want.Sources = append(want.Sources, licenseFile{File: "Contents/Resources/licenses/m1ddc/upstream/" + source.File, SHA256: source.SHA256})
	}
	if !reflect.DeepEqual(record, want) {
		return nil, errors.New("随包工具来源、版本、许可或路径与固定m1ddc契约不符")
	}
	// 构建入口在工具签名后记录哈希；包装必须先核实原件，再核实复制件，不能信任仅改了清单的外部载荷。
	files := append([]licenseFile{{File: record.File, SHA256: record.SHA256}, record.SourceManifest}, record.Licenses...)
	files = append(files, record.Sources...)
	for _, file := range files {
		path := filepath.Join(app, filepath.FromSlash(file.File))
		info, err := regular(path)
		if err != nil {
			return nil, err
		}
		if info.Size() == 0 {
			return nil, fmt.Errorf("随包文件%s为空", file.File)
		}
		hash, err := digest(path)
		if err != nil {
			return nil, fmt.Errorf("随包文件%s无效：%w", file.File, err)
		}
		if hash != file.SHA256 {
			return nil, fmt.Errorf("随包文件%s哈希不匹配", file.File)
		}
	}
	binary := filepath.Join(app, filepath.FromSlash(record.File))
	info, err := regular(binary)
	if err != nil {
		return nil, err
	}
	if info.Mode()&0111 == 0 {
		return nil, errors.New("随包m1ddc缺少执行权限")
	}
	image, err := macho.Open(binary)
	if err != nil {
		return nil, fmt.Errorf("随包m1ddc不是有效Mach-O：%w", err)
	}
	targetOK := image.Cpu == macho.CpuArm64 && image.Type == macho.TypeExec
	if err := image.Close(); err != nil {
		return nil, err
	}
	if !targetOK {
		return nil, errors.New("随包m1ddc必须为macOS arm64可执行程序")
	}
	if _, err := buildinfo.ReadFile(binary); err == nil {
		return nil, errors.New("随包m1ddc不能由Go程序冒充")
	}
	// 应用只允许已知代码、许可、来源与签名资源；不能把用户配置或任意非Go可执行文件顺带打入包。
	allowedFiles := map[string]bool{
		"Contents/Info.plist": true, "Contents/_CodeSignature/CodeResources": true,
		"Contents/MacOS/displaykvm": true, "Contents/MacOS/displaykvmd": true,
		"Contents/MacOS/displaykvm-cli": true, "Contents/MacOS/m1ddc": true,
		"Contents/Resources/bundled-tools.json":         true,
		"Contents/Resources/licenses/m1ddc/SOURCE.json": true,
		"Contents/Resources/licenses/m1ddc/LICENSE":     true,
	}
	allowedDirs := map[string]bool{
		".": true, "Contents": true, "Contents/MacOS": true, "Contents/Resources": true,
		"Contents/Resources/licenses": true, "Contents/Resources/licenses/m1ddc": true,
		"Contents/_CodeSignature":                            true,
		"Contents/Resources/licenses/m1ddc/upstream":         true,
		"Contents/Resources/licenses/m1ddc/upstream/headers": true,
		"Contents/Resources/licenses/m1ddc/upstream/sources": true,
	}
	for _, source := range record.Sources {
		allowedFiles[source.File] = true
	}
	if err := filepath.WalkDir(app, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(app, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() && allowedDirs[rel] || entry.Type().IsRegular() && allowedFiles[rel] {
			return nil
		}
		return fmt.Errorf("桌面应用含未知文件、目录或链接：%s", rel)
	}); err != nil {
		return nil, err
	}
	// 候选内记录相对应用根，归档记录相对载荷根；只改路径前缀，来源pin及字节哈希保持不变。
	record.File = "DisplayKvm.app/" + record.File
	record.SourceManifest.File = "DisplayKvm.app/" + record.SourceManifest.File
	for i := range record.Licenses {
		record.Licenses[i].File = "DisplayKvm.app/" + record.Licenses[i].File
	}
	for i := range record.Sources {
		record.Sources[i].File = "DisplayKvm.app/" + record.Sources[i].File
	}
	return []nativeToolRecord{record}, nil
}

func verifyBundledCopies(payload string, tools []nativeToolRecord) error {
	for _, tool := range tools {
		files := append([]licenseFile{{File: tool.File, SHA256: tool.SHA256}, tool.SourceManifest}, tool.Licenses...)
		files = append(files, tool.Sources...)
		for _, file := range files {
			hash, err := digest(filepath.Join(payload, filepath.FromSlash(file.File)))
			if err != nil {
				return err
			}
			if hash != file.SHA256 {
				return fmt.Errorf("随包文件%s在包装复制期间变化", file.File)
			}
		}
	}
	return nil
}

func verifyDesktopSignatures(app string) error {
	// 原生工具与三个Go程序均为独立签名对象；源应用和复制件都核对，避免只验证外层资源封印。
	// 这仅证明本地代码签名一致，不证明开发者身份、公证状态或硬件兼容性。
	for _, name := range []string{"displaykvm", "displaykvmd", "displaykvm-cli", "m1ddc"} {
		data, err := exec.Command("/usr/bin/codesign", "--verify", "--strict", filepath.Join(app, "Contents", "MacOS", name)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("桌面程序%s签名验证失败：%s：%w", name, data, err)
		}
	}
	data, err := exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", app).CombinedOutput()
	if err != nil {
		return fmt.Errorf("桌面应用整体签名验证失败：%s：%w", data, err)
	}
	return nil
}

func packageDesktop(repo, runDir, app string) error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return errors.New("桌面仅允许原生macOS arm64包装")
	}
	app, err := desktopAppPath(repo, app)
	if err != nil {
		return err
	}
	plist, err := readPlist(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return err
	}
	if plist["CFBundleExecutable"] != "displaykvm" || !validLabel(plist["CFBundleShortVersionString"]) || !validLabel(plist["CFBundleVersion"]) || plist["LSMinimumSystemVersion"] != "13.0.0" || plist["DisplayKVMStartupVersion"] != "1" {
		return errors.New("Info.plist版本、入口或部署声明不符")
	}
	records, err := desktopBinaries(app)
	if err != nil {
		return err
	}
	tools, err := bundledTools(app)
	if err != nil {
		return err
	}
	if err := verifyDesktopSignatures(app); err != nil {
		return err
	}
	name := "displaykvm-desktop-" + plist["CFBundleShortVersionString"] + "-darwin-arm64"
	payload, err := preparePayload(repo, runDir, name)
	if err != nil {
		return err
	}
	if err := copyTree(app, filepath.Join(payload, "DisplayKvm.app")); err != nil {
		return err
	}
	if err := verifyBundledCopies(payload, tools); err != nil {
		return err
	}
	if err := verifyDesktopSignatures(filepath.Join(payload, "DisplayKvm.app")); err != nil {
		return err
	}
	// 原桌面脚本的锁定缓存仅作输入，复制进本次build目录再离线查询，避免包装写入桌面缓存。
	if err := copyTree(filepath.Join(repo, "desktop", ".cache", "gomod"), filepath.Join(runDir, "cache", "gomod")); err != nil {
		return err
	}
	for i := range records {
		binary := filepath.Join(payload, filepath.FromSlash(records[i].File))
		copiedHash, err := digest(binary)
		if err != nil {
			return err
		}
		if copiedHash != records[i].SHA256 {
			return errors.New("桌面程序在包装复制期间变化，停止生成归档")
		}
		licenseName := filepath.Base(records[i].File)
		if licenseName == "displaykvm" {
			licenseName = "displaykvm-desktop"
		}
		if err := collectLicenses(repo, runDir, payload, "licenses/"+licenseName, &records[i], binary); err != nil {
			return err
		}
	}
	appInfo := map[string]string{"bundle_version": plist["CFBundleVersion"], "short_version": plist["CFBundleShortVersionString"], "minimum_macos_declaration": plist["LSMinimumSystemVersion"], "bundle_id": plist["CFBundleIdentifier"], "signing": "codesign strict verified for all four programs and app before/after copy; local ad-hoc only, developer identity/notarization not verified"}
	manifest := packageRecord{Schema: 1, Label: plist["CFBundleShortVersionString"], Status: "candidate; native runtime/notarization/hardware not verified", App: appInfo, Binaries: records, NativeTools: tools}
	if err := finishPayload(payload, manifest); err != nil {
		return err
	}
	archive, err := makeArchive(payload, filepath.Join(runDir, "dist"), false)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(runDir, "dist", "archives.json"), []archiveRecord{archive})
}

func finishPayload(payload string, manifest packageRecord) error {
	if err := writeJSON(filepath.Join(payload, "manifest.json"), manifest); err != nil {
		return err
	}
	var lines []string
	err := filepath.WalkDir(payload, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		hash, err := digest(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(payload, path)
		if err != nil {
			return err
		}
		lines = append(lines, hash+"  "+filepath.ToSlash(rel)+"\n")
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(lines)
	// 哈希表在最后生成，既不把自身列入，也不把归档哈希塞进归档，避免循环自引用。
	return writeFile(filepath.Join(payload, "SHA256SUMS"), []byte(strings.Join(lines, "")))
}

func makeArchive(payload, dist string, windows bool) (archiveRecord, error) {
	name := filepath.Base(payload) + ".tar.gz"
	if windows {
		name = filepath.Base(payload) + ".zip"
	}
	path := filepath.Join(dist, name)
	if err := noLinks(dist); err != nil {
		return archiveRecord{}, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return archiveRecord{}, err
	}
	if windows {
		err = writeZip(f, payload)
	} else {
		err = writeTar(f, payload)
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return archiveRecord{}, err
	}
	hash, err := digest(path)
	if err != nil {
		return archiveRecord{}, err
	}
	if err := writeFile(path+".sha256", []byte(hash+"  "+name+"\n")); err != nil {
		return archiveRecord{}, err
	}
	return archiveRecord{name, hash}, nil
}

func archiveEntries(payload string, write func(string, string, fs.FileInfo) error) error {
	return filepath.WalkDir(payload, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := noLinks(path); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("归档包含特殊文件")
		}
		rel, err := filepath.Rel(filepath.Dir(payload), path)
		if err != nil {
			return err
		}
		return write(path, filepath.ToSlash(rel), info)
	})
}

func writeTar(out io.Writer, payload string) error {
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	err := archiveEntries(payload, func(path, name string, info fs.FileInfo) error {
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name, header.Uid, header.Gid, header.Uname, header.Gname = name, 0, 0, "", ""
		header.ModTime, header.AccessTime, header.ChangeTime = time.Unix(0, 0), time.Time{}, time.Time{}
		header.Mode = 0644
		if info.IsDir() || info.Mode()&0111 != 0 {
			header.Mode = 0755
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		return errors.Join(copyErr, f.Close())
	})
	return errors.Join(err, tw.Close(), gz.Close())
}

func writeZip(out io.Writer, payload string) error {
	zw := zip.NewWriter(out)
	err := archiveEntries(payload, func(path, name string, info fs.FileInfo) error {
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name, header.Method = name, zip.Deflate
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		if info.IsDir() {
			header.Name += "/"
			header.SetMode(os.ModeDir | 0755)
		} else if info.Mode()&0111 != 0 {
			header.SetMode(0755)
		} else {
			header.SetMode(0644)
		}
		w, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(w, f)
		return errors.Join(copyErr, f.Close())
	})
	return errors.Join(err, zw.Close())
}
