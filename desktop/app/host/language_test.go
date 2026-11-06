package host

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestSettingsProductSystemLanguagesNormalizeAndFallbackWithoutSystemQuery(t *testing.T) {
	failure := errors.New("系统首选语言读取失败")
	for _, tt := range []struct {
		name string
		raw  []string
		err  error
		want []string
	}{
		{name: "ordered", raw: []string{" zh_Hant_TW.UTF-8 ", "en_US", "zh_Hant_TW.UTF-8"}, want: []string{"zh-Hant-TW", "en-US"}},
		{name: "unsupported remains for manager", raw: []string{"fr-FR", "zh-CN"}, want: []string{"fr-FR", "zh-CN"}},
		{name: "empty", want: []string{"en"}},
		{name: "failure", raw: []string{"zh-CN"}, err: failure, want: []string{"en"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := systemLanguages(func() ([]string, error) { return tt.raw, tt.err })
			wantError := tt.err != nil || len(tt.raw) == 0
			if !reflect.DeepEqual(got, tt.want) || (err != nil) != wantError || (tt.err != nil && !errors.Is(err, tt.err)) {
				t.Fatalf("系统语言及失败诊断必须保留：got=%v err=%v", got, err)
			}
		})
	}
}

func TestSettingsProductLinuxLanguageEnvironmentPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"language order", map[string]string{"LANGUAGE": "zh_TW:en_US", "LANG": "fr_FR.UTF-8"}, []string{"zh-TW", "en-US", "fr-FR"}},
		{"category override", map[string]string{"LC_ALL": "en_GB.UTF-8", "LC_MESSAGES": "zh_CN.UTF-8", "LANG": "fr_FR.UTF-8"}, []string{"en-GB"}},
		{"messages override", map[string]string{"LC_MESSAGES": "zh_CN.UTF-8", "LANG": "en_US.UTF-8"}, []string{"zh-CN"}},
		{"C ignores LANGUAGE", map[string]string{"LANGUAGE": "zh_CN", "LC_ALL": "C.UTF-8"}, []string{"en"}},
		{"POSIX", map[string]string{"LANG": "POSIX"}, []string{"en"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := environmentLanguages(func(key string) string { return tt.env[key] })
			got, normalizeErr := systemLanguages(func() ([]string, error) { return raw, err })
			if normalizeErr != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Linux语言优先级错误：got=%v err=%v", got, normalizeErr)
			}
		})
	}
}

func TestSettingsProductWindowsLanguageListRejectsTruncation(t *testing.T) {
	buffer := utf16.Encode([]rune("zh-CN\x00en-US\x00\x00"))
	got, err := windowsLanguageList(buffer, 2)
	if err != nil || !reflect.DeepEqual(got, []string{"zh-CN", "en-US"}) {
		t.Fatalf("Windows首选语言必须保留顺序：got=%v err=%v", got, err)
	}
	for _, invalid := range [][]uint16{nil, buffer[:len(buffer)-1], utf16.Encode([]rune("zh-CN\x00\x00"))} {
		if _, err := windowsLanguageList(invalid, 2); err == nil {
			t.Fatal("长度、数量或终止符错误不能伪装成可用系统语言")
		}
	}
}

func TestSettingsProductDialogCatalogUpdatesAndInstancesRemainIndependent(t *testing.T) {
	language, err := i18n.New("en", []string{"zh-CN"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := i18n.New("zh-CN", []string{"en"})
	if err != nil {
		t.Fatal(err)
	}
	bridge, otherBridge := NewStartupBridge(), NewStartupBridge()
	bridge.SetLanguage(language)
	otherBridge.SetLanguage(other)
	before := bridge.dialogCatalog()
	open := openDialogOptions(before, "config")
	directory := filepath.Join(t.TempDir(), "Settings Folder")
	save := saveDialogOptions(before, filepath.Join(directory, "settings.json"))
	if open.Title != before.Text("host.dialog.open.config.title", nil) || open.Message != before.Text("host.dialog.open.config.message", nil) || open.ButtonText != before.Text("host.dialog.open.button", nil) {
		t.Fatal("打开面板的应用标题、消息和按钮必须来自同一Catalog")
	}
	if save.Title != before.Text("host.dialog.save.title", nil) || save.Message != before.Text("host.dialog.save.message", nil) || save.ButtonText != before.Text("host.dialog.save.button", nil) || save.Directory != directory || save.Filename != "settings.json" {
		t.Fatalf("保存面板必须保留建议位置并翻译应用文案：%+v", save)
	}
	if !open.CanChooseFiles || open.CanChooseDirectories || open.CanCreateDirectories || open.AllowsMultipleSelection || !open.AllowsOtherFileTypes || !save.CanCreateDirectories || !save.AllowOtherFileTypes {
		t.Fatal("语言选项不能扩大原有文件选择能力")
	}
	if err := language.SetPreference("zh-CN"); err != nil {
		t.Fatal(err)
	}
	after := bridge.dialogCatalog()
	if before.Locale != "en" || after.Locale != "zh-CN" || after.Revision <= before.Revision || otherBridge.dialogCatalog().Preference != "zh-CN" {
		t.Fatalf("桥必须读取实例的最新Catalog且不修改其它实例：before=%+v after=%+v", before, after)
	}
	if err := language.SetPreference("en"); err != nil {
		t.Fatal(err)
	}
	if otherBridge.dialogCatalog().Locale != "zh-CN" {
		t.Fatal("一个桥切回英文不能改变其它桥的中文")
	}
	if got := openDialogOptions(after, "config"); got.Title == open.Title || got.Message == open.Message || got.ButtonText == open.ButtonText {
		t.Fatal("切换语言后的下一次面板必须使用更新文案")
	}
	if _, canceled, err := bridge.PickFile(context.Background(), "rules"); err == nil || canceled {
		t.Fatal("已移除的旧独立规则文件入口必须拒绝")
	}
}

func TestSettingsProductDialogErrorsTranslateWithoutLosingCause(t *testing.T) {
	language, err := i18n.New("en", nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge := NewStartupBridge()
	bridge.SetLanguage(language)
	suggested := filepath.Join(t.TempDir(), "settings.json")
	_, canceled, failure := bridge.SaveFile(context.Background(), suggested)
	if canceled || failure == nil || failure.Error() != language.Snapshot().Text("host.dialog.error.not_ready", nil) {
		t.Fatal("未绑定宿主必须显示当前语言错误，不可作为用户取消")
	}
	if err := language.SetPreference("zh-CN"); err != nil {
		t.Fatal(err)
	}
	_, _, translated := bridge.SaveFile(context.Background(), suggested)
	if translated == nil || translated.Error() == failure.Error() {
		t.Fatal("语言变化后错误文案也必须更新")
	}
	nativeFailure := errors.New("native fixture failure")
	bridge.bind(context.Background(), nil, func(context.Context, string) (string, error) { return "", nativeFailure })
	_, canceled, failure = bridge.SaveFile(context.Background(), suggested)
	if canceled || !errors.Is(failure, nativeFailure) || failure.Error() != language.Snapshot().Text("host.dialog.error.native", map[string]string{"reason": nativeFailure.Error()}) {
		t.Fatalf("原生错误翻译后必须保留原始原因：%v", failure)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, canceled, failure = bridge.SaveFile(ctx, suggested)
	if canceled || !errors.Is(failure, context.Canceled) {
		t.Fatal("请求取消是可识别的错误，不能混同用户取消原生窗口")
	}
}

func TestSettingsProductLanguageNotificationUsesSafeCatalogAndNoNavigation(t *testing.T) {
	catalog := i18n.Catalog{Preference: "en", Locale: "en", Revision: 8, Messages: map[string]string{
		"host.window.title": "Settings </script>\u2028');window.bad=true;//",
		"tray.label":        "DisplayKVM", "tray.tooltip": "Open settings",
	}}
	var title, label, tooltip, script string
	var emitted i18n.Catalog
	menus := 0
	target := languageTarget{
		setWindowTitle: func(value string) { title = value },
		setTrayLabel:   func(value string) { label = value },
		setTrayTooltip: func(value string) { tooltip = value },
		updateMenu:     func() { menus++ },
		emit:           func(value i18n.Catalog) { emitted = value },
		execJS:         func(value string) { script = value },
	}
	if err := target.apply(catalog); err != nil {
		t.Fatal(err)
	}
	if title != catalog.Messages["host.window.title"] || label != "DisplayKVM" || tooltip != "Open settings" || menus != 1 || emitted.Revision != catalog.Revision {
		t.Fatal("原生标题、托盘和事件必须来自同一次Catalog")
	}
	const prefix = `window.dispatchEvent(new CustomEvent("desktop:language",{detail:`
	const suffix = `}));`
	if !strings.HasPrefix(script, prefix) || !strings.HasSuffix(script, suffix) || strings.Contains(script, "</script>") || strings.Contains(script, "location") || strings.Contains(script, "reload") {
		t.Fatalf("语言事件必须以安全JSON传值且不导航：%s", script)
	}
	var decoded i18n.Catalog
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(script, prefix), suffix)), &decoded); err != nil || !reflect.DeepEqual(decoded, catalog) {
		t.Fatalf("DOM事件必须携带完整Catalog：decoded=%+v err=%v", decoded, err)
	}
}

func TestSettingsProductLanguageWatcherHonorsShutdownAndServiceChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	language, err := i18n.New("en", nil)
	if err != nil {
		t.Fatal(err)
	}
	serviceChanges := make(chan struct{}, 1)
	queued := make(chan func(), 2)
	done := make(chan struct{})
	menus, languageUpdates := 0, 0
	var applied i18n.Catalog
	next := func() func() {
		t.Helper()
		select {
		case update := <-queued:
			return update
		case <-time.After(2 * time.Second):
			t.Fatal("宿主通知未被派发")
			return nil
		}
	}
	go func() {
		watchHostChanges(ctx, serviceChanges, language, func(fn func()) { queued <- fn }, func() { menus++ }, func(catalog i18n.Catalog) { languageUpdates++; applied = catalog })
		close(done)
	}()
	serviceChanges <- struct{}{}
	next()()
	if menus != 1 || languageUpdates != 0 {
		t.Fatal("普通后台变化只能更新菜单，不能伪造语言变更")
	}
	if err := language.SetPreference("zh-CN"); err != nil {
		t.Fatal(err)
	}
	next()()
	if languageUpdates != 1 || applied.Locale != "zh-CN" {
		t.Fatal("后台未连接时语言仍必须独立更新")
	}
	if err := language.SetPreference("en"); err != nil {
		t.Fatal(err)
	}
	update := next()
	cancel()
	update()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("退出后宿主通知监听未结束")
	}
	if languageUpdates != 1 {
		t.Fatal("退出后排队的主线程语言回调必须失效")
	}
}

func TestSettingsProductLanguageOptionsPreserveLifecycle(t *testing.T) {
	language, err := i18n.New("en", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, silent := range []bool{false, true} {
		args := []string{"DisplayKVM"}
		if silent {
			args = append(args, "--login-start")
		}
		options, window := launchOptions(args, func() {})
		applyLanguageOptions(&options, &window, language.Snapshot())
		wantPolicy := application.ActivationPolicyRegular
		if silent {
			wantPolicy = application.ActivationPolicyAccessory
		}
		if window.Hidden != silent || options.Mac.ActivationPolicy != wantPolicy || options.Mac.ApplicationShouldTerminateAfterLastWindowClosed || options.SingleInstance == nil || options.SingleInstance.UniqueID != "com.orzlee.displaykvm.desktop" {
			t.Fatal("语言初始化不能改变登录隐藏、Dock、关闭驻留或单实例设置")
		}
	}
}

func TestSettingsProductMainReadsSelectedDocumentAndSharesLanguage(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "../../main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	languageName := ""
	isSelector := func(expression ast.Expr, object, name string) bool {
		selector, ok := expression.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != name {
			return false
		}
		identifier, ok := selector.X.(*ast.Ident)
		return ok && identifier.Name == object
	}
	ast.Inspect(source, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) == 0 || len(assignment.Rhs) != 1 {
			return true
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if ok && isSelector(call.Fun, "i18n", "New") {
			if identifier, ok := assignment.Lhs[0].(*ast.Ident); ok {
				languageName = identifier.Name
			}
		}
		return true
	})
	bridgeShared, hostShared := false, false
	ast.Inspect(source, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
			calls[selector.Sel.Name]++
		}
		if isSelector(call.Fun, "bridge", "SetLanguage") && len(call.Args) == 1 {
			if identifier, ok := call.Args[0].(*ast.Ident); ok {
				bridgeShared = languageName != "" && identifier.Name == languageName
			}
		}
		if isSelector(call.Fun, "host", "Run") && len(call.Args) >= 3 {
			if identifier, ok := call.Args[2].(*ast.Ident); ok {
				hostShared = languageName != "" && identifier.Name == languageName
			}
		}
		return true
	})
	if !bridgeShared || !hostShared {
		t.Fatal("主入口必须把同一个语言管理器传给原生桥和宿主")
	}
	for _, name := range []string{"Profile", "ReadFile", "SystemLanguages", "New", "SetLanguage", "Run"} {
		if calls[name] == 0 {
			t.Errorf("启动必须读取已选文档并共享语言实例，缺少调用：%s", name)
		}
	}
	for _, name := range []string{"SaveProfile", "OpenWriter", "Save", "Enable", "Disable", "WriteFile"} {
		if calls[name] != 0 {
			t.Errorf("语言初始化不得写用户文件或改变系统登录项：%s", name)
		}
	}
}

func TestSettingsProductHostRejectsMissingLanguageBeforeNativeStartup(t *testing.T) {
	if err := Run(nil, nil, nil); err == nil {
		t.Fatal("缺少语言实例必须在创建原生应用和后台服务前失败")
	}
}
