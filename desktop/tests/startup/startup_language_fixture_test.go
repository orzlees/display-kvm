package startup_test

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/startup"
)

var selectedStartupLanguagePattern = regexp.MustCompile(`<option value="(system|zh-CN|en)"[^>]*\bselected\b`)

func newChineseStartupHandler(t *testing.T, backend startup.Backend, dialogs startup.FileDialogs) http.Handler {
	t.Helper()
	// 这些回归用例以中文检查保存结果；每次显式创建独立语言管理器，避免依赖
	// 系统语言或共享全局状态。生产默认 system 在没有系统首选项时仍回退英文。
	language, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	return startup.NewHandler(backend, dialogs, language)
}
