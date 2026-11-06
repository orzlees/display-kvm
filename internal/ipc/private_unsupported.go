//go:build !darwin && !linux && !windows

package ipc

import (
	"os"

	"github.com/orzlee/displayKvm/internal/model"
)

// 其他平台尚无可核验的私有保护及对应系统互斥接线，在创建目录或监听前明确拒绝。
// 不能把仅调整POSIX模式位作为这些平台的安全替代继续启动。
func privateRuntime(string, bool) (string, func() error, error) {
	return "", nil, model.NewError(model.CodePermissionDenied, "当前平台尚未实现可核验的私有ACL与单实例互斥，拒绝启动或连接后台", nil)
}

func openPrivate(string, bool) (*os.File, error) {
	return nil, model.NewError(model.CodePermissionDenied, "当前平台私有文件保护尚不可用", nil)
}

func lockExclusive(*os.File) error {
	return model.NewError(model.CodePermissionDenied, "当前平台系统互斥尚不可用", nil)
}
