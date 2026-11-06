// Package controltools只定位和检查控制工具，不启动程序，也不修改配置或宿主系统。
package controltools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/orzlee/displayKvm/internal/model"
)

// Paths中的非空值用于macOS/Linux高级配置覆盖；这两个平台的空值在解析时使用随包工具。
// 这些平台的解析结果依赖当前安装位置，只供本次运行使用，不能写回用户文档成为新的默认路径。
type Paths struct {
	ToolPath    string `json:"tool_path"`
	BusctlPath  string `json:"busctl_path,omitempty"`
	UdevadmPath string `json:"udevadm_path,omitempty"`
}

// Resolver的Executable是当前程序或待启用服务程序的位置，不能传入构建工作区。
// 不缓存安装目录；应用移动后应根据新的程序位置创建Resolver。
type Resolver struct {
	GOOS       string
	Executable string
}

// Current使用操作系统报告的真实程序位置，不使用启动参数、工作目录或PATH猜测安装位置。
func Current() (Resolver, error) {
	executable, err := os.Executable()
	if err != nil {
		return Resolver{}, model.NewError(model.CodeIO, "无法读取当前程序位置", err)
	}
	executable, err = actualExecutable(executable)
	if err != nil {
		return Resolver{}, err
	}
	return Resolver{GOOS: runtime.GOOS, Executable: executable}, nil
}

// Resolve只返回本次运行需要的路径副本；失败不返回部分可用结果，也不修改入参。
// macOS三个入口都位于.app/Contents/MacOS，Linux入口与工具同目录，运行库由包自身配置。
func (r Resolver) Resolve(paths Paths) (Paths, error) {
	var displayTool string
	switch r.GOOS {
	case "darwin":
		displayTool = "m1ddc"
	case "linux":
		displayTool = "ddcutil"
	case "windows":
		// Windows显示控制及事件来源直接使用系统原生接口，不依赖外部工具或安装位置。
		// 跨平台文档可能保留其他系统的覆盖路径；这里只返回本次运行需要的空路径，
		// 不探测这些文件，也不清空文档，否则切回原平台会丢失高级配置。
		return Paths{}, nil
	default:
		return Paths{}, model.NewError(model.CodeUnsupportedOperation, "当前操作系统尚不支持显示控制工具解析", nil)
	}

	resolved := paths
	tools := []struct {
		path  *string
		field string
		name  string
	}{
		{&resolved.ToolPath, "tool_path", displayTool},
		{&resolved.BusctlPath, "busctl_path", "busctl"},
		{&resolved.UdevadmPath, "udevadm_path", "udevadm"},
	}
	if r.GOOS == "darwin" {
		// macOS不使用Linux事件工具，不因跨平台文档中保留的覆盖路径阻断显示控制。
		tools = tools[:1]
	}
	// Linux的两个事件工具缺少任何一项都不能判为运行就绪；随包程序不代表宿主已经
	// 提供D-Bus/logind/udev，也不在这里探测或安装系统设施。

	directory := ""
	for _, tool := range tools {
		name := "显式指定的工具" + tool.name
		if *tool.path == "" {
			if directory == "" {
				// 启动入口允许是链接，但必须沿它找到真实程序的邻居，不能使用链接所在目录。
				// 工具本身则拒绝链接，避免随包文件或显式覆盖被悄悄转向其他程序。
				executable, err := actualExecutable(r.Executable)
				if err != nil {
					return Paths{}, err
				}
				if err := checkExecutable(executable, "当前程序", "executable"); err != nil {
					return Paths{}, err
				}
				directory = filepath.Dir(executable)
			}
			*tool.path = filepath.Join(directory, tool.name)
			name = "随包工具" + tool.name
		}
		if err := checkExecutable(*tool.path, name, tool.field); err != nil {
			return Paths{}, err
		}
	}
	return resolved, nil
}

// actualExecutable同时供当前进程与待启用的服务入口使用，消除启动别名造成的安装目录歧义。
func actualExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0 {
		err := model.NewError(model.CodeInvalidConfig, "程序位置必须是有效的绝对路径", nil)
		err.Field = "executable"
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		code := model.CodeIO
		if errors.Is(err, os.ErrNotExist) {
			code = model.CodeBackendUnavailable
		} else if errors.Is(err, os.ErrPermission) {
			code = model.CodePermissionDenied
		}
		classified := model.NewError(code, "无法定位实际程序文件，请检查应用包完整性", err)
		classified.Field = "executable"
		return "", classified
	}
	return resolved, nil
}

// checkExecutable只核对普通文件和权限位，不执行版本或能力探测。
// 元信息检查不能证明ACL、动态库或宿主设施可用，也不能消除检查到执行之间的文件替换风险；
// 后端仍须保留实际执行失败的错误，不能把此检查当成工具运行成功的证明。
func checkExecutable(path, name, field string) error {
	if !filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0 {
		err := model.NewError(model.CodeInvalidConfig, name+"必须使用有效的绝对路径", nil)
		err.Field = field
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		code := model.CodeIO
		message := name + "文件信息读取失败"
		if errors.Is(err, os.ErrNotExist) {
			code = model.CodeBackendUnavailable
			message = name + "不存在，请检查应用包完整性或高级配置"
		} else if errors.Is(err, os.ErrPermission) {
			code = model.CodePermissionDenied
			message = name + "文件信息访问被拒绝"
		}
		classified := model.NewError(code, message, err)
		classified.Field = field
		return classified
	}
	var classified *model.Error
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		classified = model.NewError(model.CodeInvalidConfig, name+"不能是符号链接", nil)
	case !info.Mode().IsRegular():
		classified = model.NewError(model.CodeInvalidConfig, name+"必须是普通文件", nil)
	case info.Mode().Perm()&0o111 == 0:
		classified = model.NewError(model.CodePermissionDenied, name+"没有可执行权限", nil)
	default:
		return nil
	}
	classified.Field = field
	return classified
}
