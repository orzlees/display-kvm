//go:build darwin || windows

package loginstart

import (
	"context"
	"encoding/json"
	"errors"
	"os"
)

// 两个平台保存相同的版本2档案：只有完整配置有效且原档案可识别才允许替换。
// 锁、私有文件检查和替换由平台提供，避免Windows另存一份配置或降低旧档案保护。
func (m *Manager) saveProfile(ctx context.Context, profile ServiceProfile) (Result, error) {
	if err := ctx.Err(); err != nil {
		return m.result(ctx, boolean(false), err)
	}
	if _, err := loadProfileDocument(profile); err != nil {
		return m.result(ctx, boolean(false), err)
	}
	lock, err := m.settingsLock()
	if err != nil {
		return m.result(ctx, boolean(false), err)
	}
	var expected os.FileInfo
	data, info, err := readPrivate(m.profilePath(), m.env.UID)
	if err == nil {
		_, err = decodeProfile(data)
		expected = info
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return m.result(ctx, boolean(false), errors.Join(err, lock.Close()))
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return m.result(ctx, boolean(false), errors.Join(err, lock.Close()))
	}
	if err := ctx.Err(); err != nil {
		return m.result(ctx, boolean(false), errors.Join(err, lock.Close()))
	}
	saved, writeErr := atomicPrivate(m.profilePath(), append(encoded, '\n'), m.env.UID, expected)
	return m.result(ctx, saved, errors.Join(writeErr, lock.Close()))
}
