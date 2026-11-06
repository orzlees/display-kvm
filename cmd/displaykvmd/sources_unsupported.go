//go:build !darwin && !linux && !windows

package main

import (
	"github.com/orzlee/displayKvm/internal/daemon"
	"github.com/orzlee/displayKvm/internal/model"
)

func registerPlatformSources(*daemon.Daemon, options) error {
	return model.NewError(model.CodeBackendUnavailable, "当前平台没有事件来源实现", nil)
}
