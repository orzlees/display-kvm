//go:build !darwin && !windows && !linux

package host

import (
	"fmt"
	"runtime"
)

func platformLanguages() ([]string, error) {
	return nil, fmt.Errorf("当前平台尚不支持系统语言读取：%s", runtime.GOOS)
}
