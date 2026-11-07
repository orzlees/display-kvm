//go:build !windows

package settings

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/orzlee/displayKvm/internal/model"
)

func commitManagedTemporary(temporary, path string) error {
	// 非Windows保留原提交顺序：替换后同步父目录，打开、同步和关闭错误均不能忽略。
	// 此时替换可能已经发生，上层仍须按真实字节判定Saved并保留本阶段错误。
	if err := os.Rename(temporary, path); err != nil {
		return model.NewError(model.CodeIO, "原子替换保存目标失败", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return model.NewError(model.CodeIO, "打开保存目录核对持久化失败", err)
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return model.NewError(model.CodeIO, "同步保存目录失败，须核对实际文件", err)
	}
	return nil
}
