package host

import (
	"context"
	"encoding/json"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/wailsapp/wails/v3/pkg/application"
)

func applyLanguageOptions(options *application.Options, window *application.WebviewWindowOptions, catalog i18n.Catalog) {
	options.Description = catalog.Text("host.app.description", nil)
	window.Title = catalog.Text("host.window.title", nil)
}

// languageTarget只包含文字更新和事件能力，不具备导航、显示、关闭或后台连接能力。
// 宿主标题和提示使用同一快照；托盘菜单由控制器读取当前快照，再把完整语言代际
// 通知页面。连续切换可能合并通知，页面仍可按Revision忽略重复或过时事件。
type languageTarget struct {
	setWindowTitle func(string)
	setTrayLabel   func(string)
	setTrayTooltip func(string)
	updateMenu     func()
	emit           func(i18n.Catalog)
	execJS         func(string)
}

func (target languageTarget) apply(catalog i18n.Catalog) error {
	data, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	target.setWindowTitle(catalog.Text("host.window.title", nil))
	target.setTrayLabel(catalog.Text("tray.label", nil))
	target.setTrayTooltip(catalog.Text("tray.tooltip", nil))
	target.updateMenu()
	target.emit(catalog)
	// 前端消费明确的DOM事件，不猜测Wails运行时全局对象。JSON编码会转义
	// 引号、HTML边界和U+2028/U+2029；词典内容不会成为可执行源码。
	target.execJS(`window.dispatchEvent(new CustomEvent("desktop:language",{detail:` + string(data) + `}));`)
	return nil
}

// 语言监听独立于后台连接等待。排入主线程的回调再次检查退出上下文，
// 确保迟到的通知不会重新触碰已关闭窗口或托盘，也不触发新的后台重连。
func watchHostChanges(ctx context.Context, serviceChanges <-chan struct{}, language *i18n.Manager, dispatch func(func()), updateMenu func(), apply func(i18n.Catalog)) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-serviceChanges:
			if !open {
				serviceChanges = nil
				continue
			}
			dispatch(func() {
				if ctx.Err() == nil {
					updateMenu()
				}
			})
		case _, open := <-language.Changes():
			if !open {
				return
			}
			dispatch(func() {
				if ctx.Err() == nil {
					apply(language.Snapshot())
				}
			})
		}
	}
}
