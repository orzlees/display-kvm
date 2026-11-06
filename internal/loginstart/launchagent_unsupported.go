//go:build !darwin

package loginstart

import (
	"context"
	"errors"
	"os"

	"github.com/orzlee/displayKvm/internal/model"
)

// 沿用项目公共错误类别并保留标准库哨兵，调用方无需解析中文说明来判断平台。
func unsupported() error {
	return model.NewError(model.CodeUnsupportedOperation, "当前平台尚不支持登录启动设置", errors.ErrUnsupported)
}

func platformAvailable() error { return unsupported() }

func openSourceFile(string) (*os.File, error) { return nil, unsupported() }

func secureDirectory(string, int, bool, bool) error        { return unsupported() }
func readPrivate(string, int) ([]byte, os.FileInfo, error) { return nil, nil, unsupported() }
func (m *Manager) status(context.Context) (Status, error)  { return emptyStatus(), unsupported() }
func (m *Manager) saveProfile(context.Context, ServiceProfile) (Result, error) {
	return Result{Saved: boolean(false), Status: emptyStatus()}, unsupported()
}
func (m *Manager) change(context.Context, Target, string, bool) (Result, error) {
	return Result{Saved: boolean(false), Status: emptyStatus()}, unsupported()
}
