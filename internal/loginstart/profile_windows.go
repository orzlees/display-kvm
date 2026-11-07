//go:build windows

package loginstart

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/settings"
)

func openSourceFile(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// 不跟随最终重解析点且不共享写入/删除；业务配置不要求私有ACL，完整
	// 应用程序和启动档案由各自调用者再检查所有者和授权，避免扩大配置读取契约。
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|0x00020000, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, loginWindowsFailure("安全打开登录启动来源文件失败", err)
	}
	file := os.NewFile(uintptr(handle), path)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if info.FileAttributes&(syscall.FILE_ATTRIBUTE_DIRECTORY|syscall.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || info.NumberOfLinks != 1 {
		return nil, errors.Join(model.NewError(model.CodePermissionDenied, "登录启动来源必须是单链接普通文件，不能是重解析点", nil), file.Close())
	}
	return file, nil
}

func readPrivate(path string, _ int) (data []byte, info os.FileInfo, result error) {
	directories, err := pinLoginWindowsDirectory(filepath.Dir(path), false, true)
	if err != nil {
		return nil, nil, err
	}
	defer func() { result = errors.Join(result, closeLoginWindowsDirectories(directories)) }()
	sid, err := loginWindowsSID()
	if err != nil {
		return nil, nil, err
	}
	file, err := openSourceFile(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	if err := checkLoginWindowsHandle(file, sid, false, true); err != nil {
		return nil, nil, err
	}
	info, err = file.Stat()
	if err != nil {
		return nil, nil, err
	}
	data, err = readBounded(file)
	if err != nil {
		return nil, nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
		return nil, nil, model.NewError(model.CodePermissionDenied, "私有启动档案在读取期间改变身份", err)
	}
	return data, info, nil
}

func sameLoginWindowsDestination(path string, expected os.FileInfo) error {
	_, actual, err := readPrivate(path, -1)
	if expected == nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if expected == nil || !os.SameFile(expected, actual) {
		return model.NewError(model.CodePermissionDenied, "启动档案目标在写入前改变身份", nil)
	}
	return nil
}

func atomicPrivate(path string, data []byte, _ int, expected os.FileInfo) (*bool, error) {
	directories, err := pinLoginWindowsDirectory(filepath.Dir(path), false, true)
	if err != nil {
		return boolean(false), err
	}
	write := func(ctx context.Context, target string, bytes []byte) (result error) {
		if err := sameLoginWindowsDestination(target, expected); err != nil {
			return err
		}
		sid, err := loginWindowsSID()
		if err != nil {
			return err
		}
		attributes, release, err := loginWindowsSecurity(sid, false)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, release()) }()
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		temporary := filepath.Join(filepath.Dir(target), fmt.Sprintf(".startup-%x.tmp", random))
		name, err := syscall.UTF16PtrFromString(temporary)
		if err != nil {
			return err
		}
		// 不能先CreateTemp再收紧ACL：继承宽权限期间会短暂暴露私有档案。
		// CREATE_NEW与显式当前用户DACL在同一步建立文件，随机撞名直接报错。
		handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE|0x00020000, 0, attributes, syscall.CREATE_NEW, syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return loginWindowsFailure("创建私有启动档案临时文件失败", err)
		}
		file := os.NewFile(uintptr(handle), temporary)
		defer func() {
			if file != nil {
				result = errors.Join(result, file.Close())
			}
			if temporary != "" {
				result = errors.Join(result, os.Remove(temporary))
			}
		}()
		if err := checkLoginWindowsHandle(file, sid, false, true); err != nil {
			return err
		}
		if _, err := file.Write(bytes); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		closeErr := file.Close()
		file = nil
		if closeErr != nil {
			return closeErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sameLoginWindowsDestination(target, expected); err != nil {
			return err
		}
		if err := os.Rename(temporary, target); err != nil {
			return err
		}
		temporary = ""
		return nil
	}
	// 复用配置持久化的实际内容核对：写调用失败也可能已提交，无法核实必须
	// 返回未知。随后再核验私有ACL，避免只因字节正确就认可被外部换身的档案。
	result := settings.SaveManagedFileWith(context.Background(), path, data, write)
	_, _, checkErr := readPrivate(path, -1)
	if checkErr != nil {
		if errors.Is(checkErr, os.ErrNotExist) && result.Saved != nil && !*result.Saved {
			checkErr = nil
		} else {
			result.Saved = nil
		}
	}
	return result.Saved, errors.Join(result.Err, checkErr, closeLoginWindowsDirectories(directories))
}
