//go:build darwin || linux

package ipc

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/orzlee/displayKvm/internal/model"
)

func privateRuntime(configDirectory string, create bool) (directory string, release func() error, result error) {
	if configDirectory == "" || !filepath.IsAbs(configDirectory) {
		return "", nil, model.NewError(model.CodePermissionDenied, "配置目录必须是当前用户的绝对私有位置", nil)
	}
	base := filepath.Clean(configDirectory)
	var missing []string
	// 检查整条父路径，不能只检查末端0700后忽略可由其他用户替换的祖先目录。
	// 系统拥有的sticky临时根目录允许存在；sticky保证其他用户不能替换本用户子项。
	for path := base; ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if create && errors.Is(err, os.ErrNotExist) {
			missing = append(missing, path)
			continue
		}
		if err != nil {
			return "", nil, model.NewError(model.CodeIO, "检查配置父目录失败", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (stat.Uid != uint32(os.Getuid()) && stat.Uid != 0) || (info.Mode().Perm()&0022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0)) {
			return "", nil, model.NewError(model.CodePermissionDenied, "配置父路径的所有者、类型或写权限不安全", nil)
		}
		if path == base && stat.Uid != uint32(os.Getuid()) {
			return "", nil, model.NewError(model.CodePermissionDenied, "配置目录不属于当前用户", nil)
		}
		if parent := filepath.Dir(path); parent == path {
			break
		}
	}
	// 首次登录时系统配置目录也可能尚不存在；先完整核验现有祖先，再按从父到子
	// 的顺序创建0700目录。已经存在的目录绝不chmod修复，以免掩盖不安全的路径。
	for index := len(missing) - 1; index >= 0; index-- {
		path := missing[index]
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", nil, model.NewError(model.CodeIO, "创建用户私有配置目录失败", err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", nil, model.NewError(model.CodeIO, "检查新建配置目录失败", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Getuid()) {
			return "", nil, model.NewError(model.CodePermissionDenied, "新建配置目录的所有者或私有权限无效", nil)
		}
	}
	directory = base
	for _, name := range []string{"DisplayKVM", "runtime"} {
		directory = filepath.Join(directory, name)
		if create {
			if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", nil, model.NewError(model.CodeIO, "创建后台私有目录失败", err)
			}
		}
		info, err := os.Lstat(directory)
		if err != nil {
			return "", nil, model.NewError(model.CodeIO, "读取后台私有目录失败", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Getuid()) {
			return "", nil, model.NewError(model.CodePermissionDenied, "后台目录必须属于当前用户、非符号链接且权限为0700", nil)
		}
	}
	return directory, func() error { return nil }, nil
}

func openPrivate(path string, create bool) (*os.File, error) {
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
	if create {
		flags = syscall.O_RDWR | syscall.O_CREAT | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
	}
	fd, err := syscall.Open(path, flags, 0600)
	if err != nil {
		code := model.CodeIO
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			code = model.CodePermissionDenied
		}
		return nil, model.NewError(code, "无法安全打开私有文件", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, model.NewError(model.CodeIO, "检查私有文件失败", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		file.Close()
		return nil, model.NewError(model.CodePermissionDenied, "私有文件必须属于当前用户、单链接且权限为0600", nil)
	}
	return file, nil
}

func lockExclusive(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return model.NewError(model.CodeDaemonUnavailable, "DisplayKVM后台已运行或单实例锁不可用", err)
	}
	return nil
}
