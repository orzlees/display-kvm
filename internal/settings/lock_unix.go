//go:build darwin || linux

package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/orzlee/displayKvm/internal/model"
)

func singleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func openManaged(path string) (*os.File, error) {
	// NONBLOCK防止Lstat后的目标被换成FIFO时阻塞；NOFOLLOW拒绝末级链接。
	// 普通配置不要求0600或特定所有者，私有权限只施加于内部锁文件。
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func acquireSettingsLock(ctx context.Context, env Environment, target string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env.UID != os.Getuid() {
		return nil, model.NewError(model.CodePermissionDenied, "内部配置锁必须属于当前用户", nil)
	}
	var path string
	if runtime.GOOS == "darwin" {
		path = localSettingsLockPath(target)
		// 只在取得写者时创建隐藏锁目录；已有目录绝不chmod修复。先核对目录
		// 再打开锁文件，避免目录链接把创建操作引到另一个位置或不同设备。
		if err := os.Mkdir(filepath.Dir(path), 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, model.NewError(model.CodeIO, "创建目标旁的内部配置锁目录失败", err)
		}
		if err := checkSettingsLockDirectories(path); err != nil {
			return nil, err
		}
	} else {
		directory, err := privateSettingsDirectory(env)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256([]byte(target))
		path = filepath.Join(directory, hex.EncodeToString(digest[:])+".lock")
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, model.NewError(model.CodePermissionDenied, "无法安全打开内部配置锁", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := checkSettingsLock(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	// 独立打开的描述符在同进程中也互斥；不以全局map代替跨进程锁。
	// 不等待或重试占用，服务持锁时离线保存应立即给出明确冲突。
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(model.NewError(model.CodeInvalidRequest, "配置正被另一写者占用，不能保存", err), file.Close())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := checkSettingsLock(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func checkSettingsLock(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return model.NewError(model.CodePermissionDenied, "内部配置锁必须属于当前用户、单链接且权限为0600", nil)
	}
	current, err := os.Lstat(file.Name())
	if err != nil || !os.SameFile(info, current) || !current.Mode().IsRegular() {
		return model.NewError(model.CodePermissionDenied, "内部配置锁路径在持有期间已被替换", err)
	}
	return checkSettingsLockDirectories(file.Name())
}

func checkSettingsLockDirectories(path string) error {
	// 锁目录也不能被替换为链接或开放写权限，否则另一个写者可能锁住不同inode。
	// Darwin只有隐藏目录要求私有，配置父目录保留普通权限；锁目录必须同设备，
	// 才能让卷自身以相同大小写/Unicode规则解析配置名和锁名，不能跨挂载点。
	lockDirectory := filepath.Dir(path)
	var lockStat *syscall.Stat_t
	for directory := lockDirectory; ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (stat.Uid != uint32(os.Getuid()) && stat.Uid != 0) || (info.Mode().Perm()&0022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0)) {
			return model.NewError(model.CodePermissionDenied, "内部配置锁父路径不安全", nil)
		}
		if directory == lockDirectory {
			lockStat = stat
		}
		if runtime.GOOS == "darwin" && directory == filepath.Dir(lockDirectory) && stat.Dev != lockStat.Dev {
			return model.NewError(model.CodePermissionDenied, "内部配置锁目录必须与配置父目录位于同一设备", nil)
		}
		if directory == lockDirectory || (runtime.GOOS != "darwin" && directory == filepath.Dir(lockDirectory)) {
			if stat.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
				return model.NewError(model.CodePermissionDenied, "内部配置锁目录必须属于当前用户且权限为0700", nil)
			}
		}
		if filepath.Dir(directory) == directory {
			break
		}
	}
	return nil
}

// privateSettingsDirectory沿用现有私有运行目录的标准库校验模式：先完整核对
// 祖先，再从父到子创建缺失目录；已有目录绝不chmod修复，以免掩盖不安全路径。
func privateSettingsDirectory(env Environment) (string, error) {
	base := filepath.Clean(env.ConfigDir)
	var missing []string
	for path := base; ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, path)
		} else {
			if err != nil {
				return "", model.NewError(model.CodeIO, "检查内部配置锁父目录失败", err)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (stat.Uid != uint32(env.UID) && stat.Uid != 0) || (info.Mode().Perm()&0022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0)) {
				return "", model.NewError(model.CodePermissionDenied, "内部配置锁父路径的所有者、类型或写权限不安全", nil)
			}
			if path == base && stat.Uid != uint32(env.UID) {
				return "", model.NewError(model.CodePermissionDenied, "内部配置锁配置目录不属于当前用户", nil)
			}
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	for index := len(missing) - 1; index >= 0; index-- {
		if err := os.Mkdir(missing[index], 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", model.NewError(model.CodeIO, "创建内部配置锁父目录失败", err)
		}
		info, err := os.Lstat(missing[index])
		if err != nil {
			return "", err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || stat.Uid != uint32(env.UID) {
			return "", model.NewError(model.CodePermissionDenied, "新建内部配置锁父目录权限无效", nil)
		}
	}
	directory := base
	for _, name := range []string{"DisplayKVM", "settings-locks"} {
		directory = filepath.Join(directory, name)
		if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", model.NewError(model.CodeIO, "创建内部配置锁目录失败", err)
		}
		info, err := os.Lstat(directory)
		if err != nil {
			return "", err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || stat.Uid != uint32(env.UID) {
			return "", model.NewError(model.CodePermissionDenied, "内部配置锁目录必须属于当前用户、非符号链接且权限为0700", nil)
		}
	}
	return directory, nil
}
