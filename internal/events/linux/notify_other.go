//go:build !linux

package linux

import (
	"context"

	"github.com/orzlee/displayKvm/internal/model"
)

// 非Linux宿主只通过注入命令与通知替身运行单元测试，不能把缺少内核凭据校验
// 降级成仅检查正文或进程存在，否则macOS上的成功会掩盖真实Linux的身份边界。
type notifyListener struct{ path string }

func newNotifyListener() (*notifyListener, error) {
	return nil, model.NewError(model.CodeUnsupportedOperation, "真实logind通知握手仅支持Linux", nil)
}
func (*notifyListener) wait(context.Context, int) error {
	return model.NewError(model.CodeUnsupportedOperation, "当前平台不支持Linux发送者凭据", nil)
}
func (*notifyListener) close() error { return nil }
