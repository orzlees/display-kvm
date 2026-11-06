package host

import (
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// windowLifecycle隔离窗口事件和原生副作用。退出状态同时被窗口钩子和恢复入口读取，
// 使用原子状态避免单实例通知、关闭事件与退出菜单并发时重新显示已退出的窗口。
type windowLifecycle struct {
	showWindow     func()
	hideWindow     func()
	navigateWindow func(string)
	quitApp        func()
	quitting       atomic.Bool
}

func (l *windowLifecycle) closeWindow(event *application.WindowEvent) {
	if l.quitting.Load() {
		return
	}
	// 普通关闭保留唯一窗口及托盘；必须在同步钩子内取消，异步监听无法阻止
	// Wails销毁窗口。显式退出已先设置quitting，不能再被这里改成隐藏。
	event.Cancel()
	l.hideWindow()
}

func (l *windowLifecycle) restore() {
	if !l.quitting.Load() {
		l.showWindow()
	}
}

func (l *windowLifecycle) openStartupSettings() {
	if l.quitting.Load() {
		return
	}
	// 启动设置沿用同一窗口和恢复入口；只在明确菜单操作时导航，不能让
	// 登录启动或普通恢复自动跳转，也不能在退出后响应迟到的菜单回调。
	l.navigateWindow("/startup/")
	l.restore()
}

func (l *windowLifecycle) quit() {
	if l.quitting.CompareAndSwap(false, true) {
		// 只退出桌面宿主；后台进程独立运行，不能在窗口或托盘退出路径停止后台。
		l.quitApp()
	}
}

func launchOptions(args []string, restore func()) (application.Options, application.WebviewWindowOptions) {
	silent := isLoginStart(args)
	policy := application.ActivationPolicyRegular
	if silent {
		// v3.1.0的Regular启动会主动激活应用，仅Hidden无法保证不抢前台。
		// 登录启动改用Accessory且隐藏窗口；手动恢复依靠托盘、reopen和单实例通知。
		policy = application.ActivationPolicyAccessory
	}
	return application.Options{
		Name: "DisplayKVM", Description: "简洁的显示器切换客户端",
		Mac: application.MacOptions{
			ActivationPolicy: policy,
			// 关闭窗口后继续驻留，退出只由明确的退出操作触发。
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "com.orzlee.displaykvm.desktop",
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				// 两种启动共用实例标识。手动再次打开恢复原窗口；重复的登录任务
				// 只是确认驻留，不能打断用户当前工作或创建第二个长期GUI。
				if !isLoginStart(data.Args) {
					restore()
				}
			},
		},
	}, application.WebviewWindowOptions{
		Title: "DisplayKVM", Width: 960, Height: 680, MinWidth: 640, MinHeight: 520,
		BackgroundColour: application.NewRGB(17, 19, 20), URL: "/", Hidden: silent,
	}
}

func isLoginStart(args []string) bool {
	// 只读取宿主自己的精确标记，不改写参数或调用全局解析器；其它参数仍保留
	// 原有处理机会，参数终止符之后的同名文本也不应改变启动方式。
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--login-start" {
			return true
		}
	}
	return false
}
