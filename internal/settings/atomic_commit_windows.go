//go:build windows

package settings

import (
	"os"

	"github.com/orzlee/displayKvm/internal/model"
)

func commitManagedTemporary(temporary, path string) error {
	// 调用方已同步并关闭临时文件。Windows的os.Open只读目录句柄不具备
	// FlushFileBuffers所需访问能力，不能沿用Unix目录Sync；这里只执行既有同目录替换。
	// 文件内容同步不代表断电后目录项耐久性，非Unix的Rename也不保证Unix等价原子性。
	// 上层SaveManagedFileWith及Writer仍须读回核对，不能仅凭本函数返回值判定Saved。
	if err := os.Rename(temporary, path); err != nil {
		return model.NewError(model.CodeIO, "原子替换保存目标失败", err)
	}
	return nil
}
