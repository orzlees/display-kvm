package host

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestLaunchOptionsKeepManualWindowAndLoginStartSilent(t *testing.T) {
	for _, tt := range []struct {
		name   string
		args   []string
		hidden bool
		policy application.ActivationPolicy
	}{
		{"manual", []string{"DisplayKVM"}, false, application.ActivationPolicyRegular},
		{"login", []string{"DisplayKVM", "--login-start"}, true, application.ActivationPolicyAccessory},
		{"other arguments", []string{"DisplayKVM", "--other"}, false, application.ActivationPolicyRegular},
		{"argument terminator", []string{"DisplayKVM", "--", "--login-start"}, false, application.ActivationPolicyRegular},
	} {
		t.Run(tt.name, func(t *testing.T) {
			options, window := launchOptions(tt.args, func() { t.Fatal("启动选项不能主动显示窗口") })
			if window.Hidden != tt.hidden || options.Mac.ActivationPolicy != tt.policy {
				t.Fatalf("启动模式错误：hidden=%v, policy=%v", window.Hidden, options.Mac.ActivationPolicy)
			}
			if options.Mac.ApplicationShouldTerminateAfterLastWindowClosed {
				t.Fatal("关闭主窗口不应结束 GUI")
			}
		})
	}
}

func TestWindowLifecycleRepeatedCloseAndRestoreRetainsWindow(t *testing.T) {
	visible, focused, destroyed := true, true, false
	lifecycle := windowLifecycle{
		showWindow: func() { visible, focused = true, true },
		hideWindow: func() { visible, focused = false, false },
		quitApp:    func() { destroyed = true },
	}
	for range 3 {
		event := application.NewWindowEvent()
		lifecycle.closeWindow(event)
		if !event.IsCancelled() || visible || focused || destroyed {
			t.Fatalf("关闭应取消销毁并隐藏：cancelled=%v visible=%v focused=%v destroyed=%v", event.IsCancelled(), visible, focused, destroyed)
		}
		lifecycle.restore()
		if !visible || !focused || destroyed {
			t.Fatalf("恢复应显示并聚焦保留的窗口：visible=%v focused=%v destroyed=%v", visible, focused, destroyed)
		}
	}
}

func TestWindowLifecycleQuitBypassesHideAndRejectsLateRestore(t *testing.T) {
	visible, running := true, true
	lifecycle := windowLifecycle{
		showWindow: func() { visible = true },
		hideWindow: func() { t.Fatal("显式退出后的关闭事件不能转成隐藏") },
		quitApp:    func() { visible, running = false, false },
	}
	lifecycle.quit()
	event := application.NewWindowEvent()
	lifecycle.closeWindow(event)
	lifecycle.restore()
	if event.IsCancelled() || visible || running {
		t.Fatalf("退出后不能阻止关闭或恢复窗口：cancelled=%v visible=%v running=%v", event.IsCancelled(), visible, running)
	}
}

func TestSingleInstanceManualLaunchRestoresAndLoginLaunchStaysSilent(t *testing.T) {
	var uniqueID string
	for _, initialArgs := range [][]string{{"DisplayKVM"}, {"DisplayKVM", "--login-start"}} {
		visible, focused := false, false
		lifecycle := windowLifecycle{showWindow: func() { visible, focused = true, true }}
		options, _ := launchOptions(initialArgs, lifecycle.restore)
		if options.SingleInstance == nil || options.SingleInstance.UniqueID == "" {
			t.Fatal("两种启动形式均必须启用单实例")
		}
		if uniqueID != "" && uniqueID != options.SingleInstance.UniqueID {
			t.Fatal("手动与登录启动必须共用同一实例标识")
		}
		uniqueID = options.SingleInstance.UniqueID
		options.SingleInstance.OnSecondInstanceLaunch(application.SecondInstanceData{Args: []string{"DisplayKVM", "--login-start"}})
		if visible || focused {
			t.Fatal("第二次登录启动请求不能显示或聚焦既有窗口")
		}
		options.SingleInstance.OnSecondInstanceLaunch(application.SecondInstanceData{Args: []string{"DisplayKVM"}})
		if !visible || !focused {
			t.Fatal("手动再次打开必须恢复既有窗口")
		}
	}
}

func TestStartupSettingsRestoresHiddenWindowAtLocalRoute(t *testing.T) {
	route, visible, focused := "/", true, true
	lifecycle := windowLifecycle{
		showWindow:     func() { visible, focused = true, true },
		hideWindow:     func() { visible, focused = false, false },
		navigateWindow: func(path string) { route = path },
	}
	event := application.NewWindowEvent()
	lifecycle.closeWindow(event)
	if visible || !event.IsCancelled() {
		t.Fatal("关闭应保留并隐藏既有窗口")
	}
	lifecycle.openStartupSettings()
	if route != "/startup/" || !visible || !focused {
		t.Fatalf("启动设置应在既有窗口显示并聚焦：route=%q visible=%v focused=%v", route, visible, focused)
	}
	lifecycle.closeWindow(application.NewWindowEvent())
	lifecycle.restore()
	if route != "/startup/" || !visible || !focused {
		t.Fatal("打开启动设置后，普通关闭与恢复仍应保留当前页面")
	}
}

func TestStartupSettingsRejectsNavigationAfterQuit(t *testing.T) {
	route, visible := "/", false
	lifecycle := windowLifecycle{
		showWindow:     func() { visible = true },
		navigateWindow: func(path string) { route = path },
		quitApp:        func() {},
	}
	lifecycle.quit()
	lifecycle.openStartupSettings()
	if route != "/" || visible {
		t.Fatalf("退出后的迟到菜单动作不能导航或恢复：route=%q visible=%v", route, visible)
	}
}
