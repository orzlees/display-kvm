// Package host隔离Wails原生宿主；纯Go处理器和测试均不导入此包。
package host

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io/fs"
	"log"
	"net/http"
	"os"
	"runtime"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/tray"
	"github.com/orzlee/displayKvm/desktop/views/overview"
	coreclient "github.com/orzlee/displayKvm/internal/client"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// Page供后续页面注册处理器和内嵌资源；处理器接收同一个service，不另建连接。
// Route、ResourcePath须各自使用独立前缀，避免未来页面争写main或覆盖手动入口。
type Page struct {
	Route        string
	Handler      func(*app.Service) http.Handler
	ResourcePath string
	Resources    fs.FS
}

func Run(assets fs.FS, bridge *StartupBridge, language *i18n.Manager, pages ...Page) error {
	if language == nil {
		return fmt.Errorf("桌面语言管理器未初始化")
	}
	service := app.NewWithLanguage(func() (app.Client, error) { return coreclient.Connect("") }, language)
	defer service.Close()
	if bridge != nil {
		bridge.SetLanguage(language)
	}
	mux := http.NewServeMux()
	mux.Handle("/client/", overview.NewHandler(service))
	for _, page := range pages {
		if page.Handler != nil {
			mux.Handle(page.Route, page.Handler(service))
		}
		if page.Resources != nil {
			mux.Handle(page.ResourcePath, http.StripPrefix(page.ResourcePath, http.FileServerFS(page.Resources)))
		}
	}
	// Wails 保留自身 /wails 路由；内嵌处理器提供 runtime.js 和页面资源。
	// 不调用 ListenAndServe，不新增端口，更不能把这个路由当成后台连接。
	mux.Handle("/", application.BundledAssetFileServer(assets))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lifecycle := &windowLifecycle{}
	show := func() {
		application.InvokeAsync(func() {
			if ctx.Err() == nil {
				lifecycle.restore()
			}
		})
	}
	options, windowOptions := launchOptions(os.Args, show)
	applyLanguageOptions(&options, &windowOptions, language.Snapshot())
	options.Assets = application.AssetOptions{Handler: mux}
	native := application.New(options)
	window := native.Window.NewWithOptions(windowOptions)
	lifecycle.showWindow = func() { window.Show().Focus() }
	lifecycle.hideWindow = func() { window.Hide() }
	// SetURL会由Wails按资源基址解析站内路径，macOS使用wails://localhost；
	// 保留框架解析，不能把/startup/当成磁盘路径或另起原生窗口。
	lifecycle.navigateWindow = func(route string) { window.SetURL(route) }
	lifecycle.quitApp = native.Quit
	window.RegisterHook(events.Common.WindowClosing, lifecycle.closeWindow)
	native.OnShutdown(func() { lifecycle.quitting.Store(true); cancel(); service.Close() })
	if runtime.GOOS == "darwin" {
		// Wails默认reopen只Show；由同一恢复入口补上Focus，并取消默认监听，
		// 确保Dock、Finder或Spotlight再次打开时仍使用唯一的既有窗口。
		native.Event.RegisterApplicationEventHook(events.Mac.ApplicationShouldHandleReopen, func(event *application.ApplicationEvent) {
			event.Cancel()
			show()
		})
	}
	actions := tray.NewWithLanguage(service, show, func() { application.InvokeAsync(lifecycle.quit) }, language)
	actions.SetStartupAction(func() {
		application.InvokeAsync(func() {
			if ctx.Err() == nil {
				lifecycle.openStartupSettings()
			}
		})
	})
	systemTray := native.SystemTray.New()
	catalog := language.Snapshot()
	systemTray.SetLabel(catalog.Text("tray.label", nil))
	systemTray.SetTooltip(catalog.Text("tray.tooltip", nil))
	icon, err := trayIcon(runtime.GOOS == "darwin")
	if err != nil {
		return fmt.Errorf("编码菜单栏恢复图标失败：%w", err)
	}
	if runtime.GOOS == "darwin" {
		systemTray.SetTemplateIcon(icon)
	} else {
		systemTray.SetIcon(icon)
	}
	systemTray.OnClick(show)
	menu := application.NewMenu()
	updateMenu := func() {
		// 复用菜单容器，让框架管理已显示旧项的生命周期；旧项即使晚到，仍由
		// service检查快照代际和当前忙碌状态，不能绕过窗口侧同一请求门禁。
		menu.Clear()
		for _, entry := range actions.Entries() {
			if entry.Kind == "connect" {
				menu.AddSeparator()
			}
			item := menu.Add(entry.Label).SetEnabled(entry.Enabled)
			if entry.Kind == "status" {
				continue
			}
			item.OnClick(func(*application.Context) {
				// 连接读取不能阻塞原生菜单回调；界面动作和后台等待都归同一GUI生命周期。
				go func() {
					if err := actions.Activate(ctx, entry); err != nil {
						log.Printf("执行托盘操作失败：%v", err)
						show()
					}
				}()
			})
		}
		systemTray.SetMenu(menu)
	}
	updateMenu()
	languageUI := languageTarget{
		setWindowTitle: func(title string) { window.SetTitle(title) },
		setTrayLabel:   systemTray.SetLabel,
		setTrayTooltip: systemTray.SetTooltip,
		updateMenu:     updateMenu,
		emit:           func(catalog i18n.Catalog) { native.Event.Emit("desktop:language", catalog) },
		execJS:         window.ExecJS,
	}
	applyLanguage := func(catalog i18n.Catalog) {
		if err := languageUI.apply(catalog); err != nil {
			log.Printf("更新桌面语言通知失败：%v", err)
		}
	}
	// v3.1.0没有托盘可用性回执。创建菜单不代表系统实际显示了图标，因此不
	// AttachWindow；手动启动保留Dock与主窗口，登录启动保留reopen和单实例
	// 恢复作为备份。菜单栏真实可见性仍须人工观察，不能据此取消关闭驻留。
	native.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		if bridge != nil {
			bridge.bindNative(ctx, native, window)
		}
		application.InvokeAsync(func() {
			if ctx.Err() == nil {
				applyLanguage(language.Snapshot())
			}
		})
		go watchHostChanges(ctx, service.Changes(), language, application.InvokeAsync, updateMenu, applyLanguage)
		go func() {
			if err := service.Reconnect(ctx); err != nil {
				log.Printf("首次连接桌面后台失败：%v", err)
			}
		}()
	})
	return native.Run()
}

func trayIcon(template bool) ([]byte, error) {
	icon := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	stroke := color.NRGBA{R: 141, G: 216, B: 190, A: 255}
	if template {
		stroke = color.NRGBA{A: 255}
	} else {
		draw.Draw(icon, image.Rect(1, 1, 31, 31), &image.Uniform{C: color.NRGBA{R: 17, G: 35, B: 29, A: 255}}, image.Point{}, draw.Src)
	}
	for _, rect := range []image.Rectangle{
		image.Rect(5, 7, 24, 9), image.Rect(5, 9, 7, 21), image.Rect(22, 9, 24, 21), image.Rect(5, 19, 24, 21),
		image.Rect(14, 21, 16, 26), image.Rect(10, 25, 22, 27), image.Rect(27, 12, 29, 25), image.Rect(24, 24, 29, 26),
	} {
		draw.Draw(icon, rect, &image.Uniform{C: stroke}, image.Point{}, draw.Src)
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, icon); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
