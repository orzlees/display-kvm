//go:build darwin && !cgo

package host

import "errors"

func platformLanguages() ([]string, error) {
	return nil, errors.New("macOS系统语言读取需要cgo CoreFoundation支持，已回退英文")
}
