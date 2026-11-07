package settings

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/orzlee/displayKvm/internal/model"
)

type PersistenceResult struct {
	Saved *bool
	Err   error
}

func known(value bool) *bool { return &value }

// ReadManagedFile只接受普通文件，不跟随末级符号链接。保存结果由实际字节核验，
// 不把Rename或Sync返回错误直接等同于“原文件未改变”。配置和规则共用这一边界。
func ReadManagedFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || !singleLink(info) {
		return nil, true, model.NewError(model.CodePermissionDenied, "保存目标必须为普通文件且不能是符号链接或硬链接", nil)
	}
	file, err := openManaged(path)
	if err != nil {
		return nil, true, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || !singleLink(opened) {
		return nil, true, errors.Join(statErr, file.Close(), model.NewError(model.CodeIO, "保存目标在读取时已被替换", nil))
	}
	data, readErr := io.ReadAll(io.LimitReader(file, model.MaxMessageBytes+1))
	// 打开后的描述符可能仍指向旧inode；读完再核对路径，防止把已被替换的
	// 旧文件当成当前版本。O_NOFOLLOW同时关闭Lstat到Open之间的末级链接窗口。
	current, identityErr := os.Lstat(path)
	if identityErr == nil && (!os.SameFile(opened, current) || !current.Mode().IsRegular() || !singleLink(current)) {
		identityErr = model.NewError(model.CodeIO, "保存目标在读取时已被替换", nil)
	}
	if err := errors.Join(readErr, identityErr, file.Close()); err != nil {
		return nil, true, err
	}
	if len(data) > model.MaxMessageBytes {
		return nil, true, model.NewError(model.CodeMessageTooLarge, "保存目标超过1MiB", nil)
	}
	return data, true, nil
}

func SaveManagedFile(ctx context.Context, path string, data []byte) PersistenceResult {
	return SaveManagedFileWith(ctx, path, data, AtomicManagedWrite)
}

// 写入边界单独注入，核对仍读取真实文件；这使目录同步等“已改文件却报错”的
// 路径可直接验证，避免测试用返回码模拟实际保存结果。
// 此结果判定和下方临时文件替换流程机械承接daemon原算法，旧入口仅保留薄适配。
func SaveManagedFileWith(ctx context.Context, path string, data []byte, write func(context.Context, string, []byte) error) PersistenceResult {
	if path == "" || len(data) > model.MaxMessageBytes {
		return PersistenceResult{Saved: known(false), Err: model.NewError(model.CodeInvalidRequest, "保存路径缺失或文档过大", nil)}
	}
	before, existed, err := ReadManagedFile(path)
	if err != nil {
		return PersistenceResult{Err: model.NewError(model.CodeIO, "无法核对原保存目标", err)}
	}
	writeErr := write(ctx, path, data)
	after, exists, readErr := ReadManagedFile(path)
	result := PersistenceResult{Err: writeErr}
	switch {
	case readErr != nil:
		result.Err = errors.Join(writeErr, model.NewError(model.CodeIO, "提交后无法核对实际文件，结果未知", readErr))
	case exists && bytes.Equal(after, data):
		result.Saved = known(true)
	case exists == existed && bytes.Equal(after, before):
		result.Saved = known(false)
		if result.Err == nil {
			result.Err = model.NewError(model.CodeIO, "原子保存后文件未成为预期内容", nil)
		}
	default:
		result.Err = errors.Join(writeErr, model.NewError(model.CodeIO, "保存目标被并发修改，无法证明提交结果", nil))
	}
	return result
}

func AtomicManagedWrite(ctx context.Context, path string, data []byte) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".displaykvm-save-*")
	if err != nil {
		return model.NewError(model.CodeIO, "创建私有保存临时文件失败", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if n, err := file.Write(data); err != nil || n != len(data) {
		return model.NewError(model.CodeIO, "写入保存临时文件失败", errors.Join(err, io.ErrShortWrite))
	}
	if err := file.Sync(); err != nil {
		return model.NewError(model.CodeIO, "同步保存临时文件失败", err)
	}
	if err := file.Close(); err != nil {
		return model.NewError(model.CodeIO, "关闭保存临时文件失败", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 替换前再次拒绝符号链接或非普通目标；不会为错误路径修权限或创建目录。
	if _, _, err := ReadManagedFile(path); err != nil {
		return err
	}
	return commitManagedTemporary(temporary, path)
}
