package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// StartupBridge在页面构造时即可提供文件选择接口，但只有原生宿主启动后才绑定。
// 不导入具体页面包，避免宿主和页面互相依赖，也不在选择路径时保存或注册登录项。
type StartupBridge struct {
	mu        sync.Mutex
	appCtx    context.Context
	pick      func(context.Context, string) (string, error)
	save      func(context.Context, string) (string, error)
	language  *i18n.Manager
	selecting bool
}

func NewStartupBridge() *StartupBridge { return &StartupBridge{} }

// SetLanguage注入与Service和宿主相同的实例，不改变已经打开的原生面板。
// 下一次面板构造时再取快照，避免把界面的语言变更当成关闭或重开面板的理由。
func (b *StartupBridge) SetLanguage(language *i18n.Manager) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.language = language
}

func (b *StartupBridge) dialogCatalog() i18n.Catalog {
	b.mu.Lock()
	language := b.language
	b.mu.Unlock()
	if language == nil {
		// 未绑定语言的旧构造入口仍可报告未就绪；不创建全局或另存一份用户偏好。
		return i18n.Catalog{}
	}
	return language.Snapshot()
}

// 保留底层原因，让调用者仍可用errors.Is识别取消/退出，而显示文案来自Catalog。
type dialogError struct {
	message string
	cause   error
}

func (err *dialogError) Error() string { return err.message }
func (err *dialogError) Unwrap() error { return err.cause }

func (b *StartupBridge) dialogError(key string, values map[string]string, cause error) error {
	return &dialogError{message: b.dialogCatalog().Text(key, values), cause: cause}
}

// 原生选项集中表达应用拥有的文案和文件边界，便于无GUI核对；所有文字必须
// 来自同一次快照，不能在Title、Message和ButtonText之间重新取不同代际。
func openDialogOptions(catalog i18n.Catalog, kind string) application.OpenFileDialogOptions {
	return application.OpenFileDialogOptions{
		Title:          catalog.Text("host.dialog.open."+kind+".title", nil),
		Message:        catalog.Text("host.dialog.open."+kind+".message", nil),
		ButtonText:     catalog.Text("host.dialog.open.button", nil),
		CanChooseFiles: true, AllowsOtherFileTypes: true,
	}
}

func saveDialogOptions(catalog i18n.Catalog, suggestedPath string) application.SaveFileDialogOptions {
	return application.SaveFileDialogOptions{
		Title:      catalog.Text("host.dialog.save.title", nil),
		Message:    catalog.Text("host.dialog.save.message", nil),
		ButtonText: catalog.Text("host.dialog.save.button", nil),
		Directory:  filepath.Dir(suggestedPath), Filename: filepath.Base(suggestedPath),
		CanCreateDirectories: true, AllowOtherFileTypes: true,
	}
}

func (b *StartupBridge) bind(ctx context.Context, pick, save func(context.Context, string) (string, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appCtx, b.pick, b.save = ctx, pick, save
}

func (b *StartupBridge) bindNative(ctx context.Context, native *application.App, window application.Window) {
	b.bind(ctx, func(pickCtx context.Context, kind string) (string, error) {
		if runtime.GOOS != "darwin" {
			return "", b.dialogError("host.dialog.error.unsupported_open", map[string]string{"platform": runtime.GOOS}, nil)
		}
		var dialog *application.OpenFileDialogStruct
		var prepareErr error
		application.InvokeSync(func() {
			if prepareErr = ctx.Err(); prepareErr != nil {
				return
			}
			if prepareErr = pickCtx.Err(); prepareErr != nil {
				return
			}
			options := openDialogOptions(b.dialogCatalog(), kind)
			options.Window = window
			dialog = native.Dialog.OpenFileWithOptions(&options)
		})
		if prepareErr != nil {
			return "", prepareErr
		}
		// v3.1.0内部把原生show派发至主线程，再等待异步选择结果。
		// 此处必须留在请求协程，不能在主线程阻塞结果回调。
		return dialog.PromptForSingleSelection()
	}, func(saveCtx context.Context, suggestedPath string) (string, error) {
		if runtime.GOOS != "darwin" {
			return "", b.dialogError("host.dialog.error.unsupported_save", map[string]string{"platform": runtime.GOOS}, nil)
		}
		var dialog *application.SaveFileDialogStruct
		var prepareErr error
		application.InvokeSync(func() {
			if prepareErr = ctx.Err(); prepareErr != nil {
				return
			}
			if prepareErr = saveCtx.Err(); prepareErr != nil {
				return
			}
			// 建议路径只用于初始化目录和文件名；选择目标本身不创建或覆盖配置，
			// 实际写入及冲突校验仍由页面调用完整配置存储完成。
			options := saveDialogOptions(b.dialogCatalog(), suggestedPath)
			options.Window = window
			dialog = native.Dialog.SaveFileWithOptions(&options)
		})
		if prepareErr != nil {
			return "", prepareErr
		}
		// 与打开对话框相同，v3.1.0在Prompt内部派发show并等待异步结果；
		// 只能在请求协程等待，避免阻塞主线程上的原生关闭回调。
		return dialog.PromptForSingleSelection()
	})
}

// PickFile返回canceled=true仅表示用户关闭选择器；宿主未就绪、退出、请求取消和
// 原生选择失败均返回错误，防止页面把真实错误当成保留草稿的普通取消。
func (b *StartupBridge) PickFile(ctx context.Context, kind string) (string, bool, error) {
	switch kind {
	case "config", "tool":
	default:
		return "", false, b.dialogError("host.dialog.error.kind", map[string]string{"kind": kind}, nil)
	}
	return b.selectFile(ctx, kind, false)
}

// SaveFile仅选择完整配置的保存目标；第二参必须是建议的绝对文件路径。
// canceled=true只表示用户关闭原生窗口，真实写入由完整配置存储另行处理。
func (b *StartupBridge) SaveFile(ctx context.Context, suggestedPath string) (path string, canceled bool, err error) {
	if !filepath.IsAbs(suggestedPath) {
		return "", false, b.dialogError("host.dialog.error.absolute_path", map[string]string{"path": suggestedPath}, nil)
	}
	return b.selectFile(ctx, suggestedPath, true)
}

// 打开和保存共用宿主上下文与占用状态。请求取消不会关闭原生窗口，因此
// 必须由同一个等待流程管理占用，避免跨类型重试时叠加对话框。
func (b *StartupBridge) selectFile(ctx context.Context, argument string, saving bool) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, b.dialogError("host.dialog.error.canceled", nil, err)
	}
	b.mu.Lock()
	choose := b.pick
	if saving {
		choose = b.save
	}
	if choose == nil || b.appCtx == nil {
		b.mu.Unlock()
		return "", false, b.dialogError("host.dialog.error.not_ready", nil, nil)
	}
	appCtx := b.appCtx
	if err := appCtx.Err(); err != nil {
		b.mu.Unlock()
		return "", false, b.dialogError("host.dialog.error.exited", map[string]string{"reason": err.Error()}, err)
	}
	if b.selecting {
		b.mu.Unlock()
		return "", false, b.dialogError("host.dialog.error.busy", nil, nil)
	}
	b.selecting = true
	b.mu.Unlock()

	type selection struct {
		path string
		err  error
	}
	result := make(chan selection, 1)
	go func() {
		var path string
		err := ctx.Err()
		if err == nil {
			err = appCtx.Err()
		}
		if err == nil {
			path, err = choose(ctx, argument)
		}
		// 请求取消并不代表原生窗口已经关闭。占用必须保留到原生调用返回，
		// 否则重试会叠加多个对话框；缓冲结果允许已取消的请求安全离开。
		b.mu.Lock()
		b.selecting = false
		b.mu.Unlock()
		result <- selection{path, err}
	}()
	select {
	case <-ctx.Done():
		return "", false, b.dialogError("host.dialog.error.canceled", nil, ctx.Err())
	case <-appCtx.Done():
		return "", false, b.dialogError("host.dialog.error.exited", map[string]string{"reason": appCtx.Err().Error()}, appCtx.Err())
	case selected := <-result:
		if err := ctx.Err(); err != nil {
			return "", false, b.dialogError("host.dialog.error.canceled", nil, err)
		}
		if err := appCtx.Err(); err != nil {
			return "", false, b.dialogError("host.dialog.error.exited", map[string]string{"reason": err.Error()}, err)
		}
		var localized *dialogError
		if selected.err != nil && !errors.As(selected.err, &localized) {
			return selected.path, false, b.dialogError("host.dialog.error.native", map[string]string{"reason": selected.err.Error()}, selected.err)
		}
		return selected.path, selected.err == nil && selected.path == "", selected.err
	}
}

// ApplicationPath仅定位当前可执行文件所属的应用包，不安装或复制任何文件。
// 开发态没有.app结构时保留实际可执行路径，允许离线编辑；是否可用于持久
// 登录启动仍由核心在启用时校验，不能因构建目录不稳定而阻止整个GUI打开。
func ApplicationPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("读取当前桌面程序路径失败：%w", err)
	}
	return applicationPath(executable), nil
}

func applicationPath(executable string) string {
	macOS := filepath.Dir(executable)
	contents := filepath.Dir(macOS)
	bundle := filepath.Dir(contents)
	if filepath.Base(macOS) == "MacOS" && filepath.Base(contents) == "Contents" && filepath.Ext(bundle) == ".app" {
		return bundle
	}
	return executable
}
