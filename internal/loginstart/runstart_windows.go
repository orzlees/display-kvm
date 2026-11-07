//go:build windows

package loginstart

import (
	"context"
	"debug/buildinfo"
	"debug/pe"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/settings"
)

const windowsRunPath = `Software\Microsoft\Windows\CurrentVersion\Run`

func platformAvailable() error { return nil }

func platformIdentity() error {
	_, err := loginWindowsSID()
	return err
}

func (m *Manager) windowsRegistration() *runRegistration {
	return &runRegistration{
		manager: m, open: openWindowsRun, lock: m.settingsLock, profile: m.Profile,
		validate: m.validateWindowsApp,
		ready: func(profile ServiceProfile, executable string) error {
			return validateServiceReady(profile, controltools.Resolver{GOOS: "windows", Executable: executable})
		},
	}
}

func (m *Manager) status(ctx context.Context) (Status, error) {
	return m.windowsRegistration().status(ctx)
}

func (m *Manager) change(ctx context.Context, target Target, app string, enable bool) (Result, error) {
	return m.windowsRegistration().change(ctx, target, app, enable)
}

func (m *Manager) settingsLock() (io.Closer, error) {
	if err := secureDirectory(filepath.Dir(m.profilePath()), m.env.UID, true, true); err != nil {
		return nil, err
	}
	// 复用配置模块已有的Windows路径规范、私有锁目录和跨进程锁，不再建立
	// 第二套锁协议。Writer只用于锁住档案路径，不把启动档案当完整业务文档保存。
	store, err := settings.NewStore(settings.Options{Environment: func() (settings.Environment, error) {
		return settings.Environment{ConfigDir: m.env.ConfigDir, UID: m.env.UID}, nil
	}})
	if err != nil {
		return nil, err
	}
	return store.OpenWriter(context.Background(), m.profilePath())
}

func (m *Manager) validateWindowsApp(app string, target Target, complete bool) (result error) {
	executable, err := runAppPath(app)
	if err != nil {
		return err
	}
	directories, err := pinLoginWindowsDirectory(filepath.Dir(executable), false, false)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, closeLoginWindowsDirectories(directories)) }()
	sid, err := loginWindowsSID()
	if err != nil {
		return err
	}
	// 文件名不能证明角色。核心原始产物的displaykvm.exe是CLI，桌面候选包才
	// 使用此名存放GUI；必须按真实Go入口区分同目录三程序，不能接受互相改名。
	for _, program := range runPrograms {
		if !complete && program.target != target {
			continue
		}
		path := filepath.Join(filepath.Dir(executable), program.name)
		file, err := openSource(path, true)
		if err != nil {
			return err
		}
		if err := checkLoginWindowsHandle(file, sid, false, false); err != nil {
			return errors.Join(err, file.Close())
		}
		image, imageErr := pe.NewFile(file)
		if imageErr != nil {
			return errors.Join(model.NewError(model.CodeInvalidConfig, "同包程序必须是有效Windows PE文件："+program.name, imageErr), file.Close())
		}
		wantMachine := uint16(pe.IMAGE_FILE_MACHINE_AMD64)
		if runtime.GOARCH == "arm64" {
			wantMachine = pe.IMAGE_FILE_MACHINE_ARM64
		} else if runtime.GOARCH == "386" {
			wantMachine = pe.IMAGE_FILE_MACHINE_I386
		}
		if image.Machine != wantMachine || image.Characteristics&pe.IMAGE_FILE_DLL != 0 || image.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 {
			return errors.Join(model.NewError(model.CodeInvalidConfig, "同包程序架构或可执行类型不匹配："+program.name, nil), file.Close())
		}
		build, buildErr := buildinfo.Read(file)
		if err := errors.Join(buildErr, file.Close()); err != nil {
			return model.NewError(model.CodeInvalidConfig, "无法核实同包程序的真实Go入口："+program.name, err)
		}
		var goos, goarch string
		for _, setting := range build.Settings {
			switch setting.Key {
			case "GOOS":
				goos = setting.Value
			case "GOARCH":
				goarch = setting.Value
			}
		}
		if err := program.validate(build.Path, goos, goarch, runtime.GOARCH); err != nil {
			return err
		}
	}
	return nil
}

type windowsRunKey struct{ handle syscall.Handle }

func openWindowsRun(write bool) (runKey, error) {
	path, err := syscall.UTF16PtrFromString(windowsRunPath)
	if err != nil {
		return nil, err
	}
	var handle syscall.Handle
	if write {
		proc, err := loginWindowsProc("RegCreateKeyExW")
		if err != nil {
			return nil, err
		}
		// 仅HKCU的两个本应用值；不枚举、不清理Run键，不写HKLM/RunOnce，
		// 也不强制另一注册表架构视图而产生两份互不一致的登录项。
		status, _, _ := proc.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(path)), 0, 0, 0, syscall.KEY_QUERY_VALUE|syscall.KEY_SET_VALUE, 0, uintptr(unsafe.Pointer(&handle)), 0)
		if status != 0 {
			return nil, loginWindowsFailure("打开当前用户Run写入键失败", syscall.Errno(status))
		}
	} else if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, syscall.KEY_QUERY_VALUE, &handle); err != nil {
		return nil, loginWindowsFailure("查询当前用户Run键失败", err)
	}
	return &windowsRunKey{handle: handle}, nil
}

func (key *windowsRunKey) Read(name string) (string, error) {
	if name != label(Service) && name != label(Desktop) {
		return "", model.NewError(model.CodeInvalidRequest, "只能查询本应用的两个Run目标", nil)
	}
	valueName, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	var kind, size uint32
	if err := syscall.RegQueryValueEx(key.handle, valueName, nil, &kind, nil, &size); err != nil {
		return "", loginWindowsFailure("查询本应用Run值失败", err)
	}
	if kind != syscall.REG_SZ || size < 2 || size > maxStartupBytes || size%2 != 0 {
		return "", model.NewError(model.CodePermissionDenied, "同名Run值不是本功能的明确REG_SZ命令", nil)
	}
	buffer := make([]uint16, size/2)
	if err := syscall.RegQueryValueEx(key.handle, valueName, nil, &kind, (*byte)(unsafe.Pointer(&buffer[0])), &size); err != nil {
		return "", loginWindowsFailure("读取本应用Run值失败", err)
	}
	if kind != syscall.REG_SZ || size < 2 || size%2 != 0 || int(size/2) > len(buffer) {
		return "", model.NewError(model.CodePermissionDenied, "Run值在读取期间改变类型或长度", nil)
	}
	buffer = buffer[:size/2]
	if buffer[len(buffer)-1] != 0 {
		return "", model.NewError(model.CodePermissionDenied, "Run值缺少字符串结束符", nil)
	}
	for _, character := range buffer[:len(buffer)-1] {
		if character == 0 {
			return "", model.NewError(model.CodePermissionDenied, "Run值包含隐藏的额外命令数据", nil)
		}
	}
	command := syscall.UTF16ToString(buffer)
	// 非法代理项不能在解码时静默变成替换字符，否则读回字符串看似相同，
	// 实际注册表字节却可能属于另一条命令，破坏精确归属与写后核实。
	if !slices.Equal(utf16.Encode([]rune(command)), buffer[:len(buffer)-1]) {
		return "", model.NewError(model.CodePermissionDenied, "Run值包含无效UTF-16命令数据", nil)
	}
	return command, nil
}

func (key *windowsRunKey) Write(name, command string) error {
	if name != label(Service) && name != label(Desktop) {
		return model.NewError(model.CodeInvalidRequest, "只能写入本应用的两个Run目标", nil)
	}
	valueName, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	value, err := syscall.UTF16FromString(command)
	if err != nil {
		return err
	}
	proc, err := loginWindowsProc("RegSetValueExW")
	if err != nil {
		return err
	}
	status, _, _ := proc.Call(uintptr(key.handle), uintptr(unsafe.Pointer(valueName)), 0, syscall.REG_SZ, uintptr(unsafe.Pointer(&value[0])), uintptr(len(value)*2))
	if status != 0 {
		return loginWindowsFailure("写入本应用Run值失败", syscall.Errno(status))
	}
	return nil
}

func (key *windowsRunKey) Delete(name string) error {
	if name != label(Service) && name != label(Desktop) {
		return model.NewError(model.CodeInvalidRequest, "只能删除本应用的两个Run目标", nil)
	}
	valueName, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	proc, err := loginWindowsProc("RegDeleteValueW")
	if err != nil {
		return err
	}
	status, _, _ := proc.Call(uintptr(key.handle), uintptr(unsafe.Pointer(valueName)))
	if status != 0 && syscall.Errno(status) != syscall.ERROR_FILE_NOT_FOUND {
		return loginWindowsFailure("删除本应用Run值失败", syscall.Errno(status))
	}
	return nil
}

func (key *windowsRunKey) Close() error {
	if key.handle == 0 {
		return nil
	}
	handle := key.handle
	key.handle = 0
	return syscall.RegCloseKey(handle)
}
