//go:build !darwin && !linux && !windows

package settings

import (
	"context"
	"os"

	"github.com/orzlee/displayKvm/internal/model"
)

func singleLink(info os.FileInfo) bool { return true }

func openManaged(path string) (*os.File, error) { return os.Open(path) }

func acquireSettingsLock(context.Context, Environment, string) (*os.File, error) {
	return nil, model.NewError(model.CodeUnsupportedOperation, "本平台尚不支持完整配置写锁", nil)
}

func checkSettingsLock(*os.File) error {
	return model.NewError(model.CodeUnsupportedOperation, "本平台尚不支持完整配置写锁", nil)
}
