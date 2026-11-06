//go:build !windows

package windows

import (
	"context"

	"github.com/orzlee/displayKvm/internal/model"
)

type otherPlatform struct{}

func newPlatform() platform { return otherPlatform{} }

func (otherPlatform) probe(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return model.NewError(model.CodeCanceled, "Windows来源探测已取消", err)
	}
	return model.NewError(model.CodeBackendUnavailable, "当前平台不是Windows，无法订阅Windows系统事件", nil)
}

func (p otherPlatform) open(ctx context.Context) (subscription, error) {
	return nil, p.probe(ctx)
}
