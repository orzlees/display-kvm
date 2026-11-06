package daemon

import (
	"context"

	"github.com/orzlee/displayKvm/internal/model"
)

type ManagementHandler func(context.Context, model.ManagementRequest) (model.ManagementResponse, error)

func (d *Daemon) RegisterManagement(handler ManagementHandler, capabilities func([]model.EventCapability) []model.EventCapability) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || handler == nil || d.management != nil {
		return model.NewError(model.CodeInvalidRequest, "管理能力只能在Run之前注册一次", nil)
	}
	d.management, d.capabilityOverlay = handler, capabilities
	return nil
}

func (d *Daemon) Capabilities() []model.EventCapability {
	base := d.eventCapabilities()
	d.mu.Lock()
	overlay := d.capabilityOverlay
	d.mu.Unlock()
	// 提供者会读取当前Scheduler；不能持Daemon锁回调，避免与快照查询形成锁反转。
	if overlay != nil {
		return overlay(base)
	}
	return base
}
