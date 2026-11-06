//go:build !darwin || !cgo

package darwin

import (
	"context"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

type unavailableSubscription struct{}

func openNative(ctx context.Context) (subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return unavailableSubscription{}, nil
}

func (unavailableSubscription) capabilities() []model.EventCapability {
	reason := "IOKit事件需要macOS及启用cgo的系统框架桥接，当前构建不具备此能力"
	return eventCapabilities(false, false, reason, reason)
}

func (unavailableSubscription) scan(context.Context) ([]nativeDevice, error) {
	return nil, model.NewError(model.CodeBackendUnavailable, "当前构建没有IOKit USB枚举能力", nil)
}

func (unavailableSubscription) poll(context.Context, time.Duration) ([]notification, error) {
	return nil, model.NewError(model.CodeBackendUnavailable, "当前构建没有IOKit通知能力", nil)
}

func (unavailableSubscription) close() error { return nil }
