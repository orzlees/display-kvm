// displaykvmd是显式启动的登录后用户后台，不安装服务、不设置自启、不启动GUI。
// 登录任务只传入私有启动档案，仍进入同一后台运行及独占IPC路径。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/orzlee/displayKvm/internal/backend"
	darwinbackend "github.com/orzlee/displayKvm/internal/backend/darwin"
	linuxbackend "github.com/orzlee/displayKvm/internal/backend/linux"
	windowsbackend "github.com/orzlee/displayKvm/internal/backend/windows"
	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/ipc"
	"github.com/orzlee/displayKvm/internal/loginstart"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/settings"
)

type options struct {
	settingsPath string
	toolPath     string
	busctlPath   string
	udevadmPath  string
	toolResolver controltools.Resolver
}

func main() {
	opts, err := parseOptions(os.Args[1:], loginstart.LoadServiceProfile)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, ipc.PublicError(err))
		// 保留原flag解析失败的用法退出码；档案读取或权限错误仍按运行失败报告。
		if model.CodeOf(err) == model.CodeInvalidRequest {
			os.Exit(2)
		}
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, opts); err != nil {
		// 仅输出可序列化错误说明，不打印私有端点、认证材料或外部命令原始输出。
		fmt.Fprintln(os.Stderr, ipc.PublicError(err))
		os.Exit(1)
	}
}

// 参数只选择完整文档入口；运行工具统一由该文档提供，避免登录任务和直接启动
// 因命令行覆盖而使用不同后端。显式空值也参与互斥，不能悄悄忽略一个入口。
func parseOptions(args []string, load func(string) (loginstart.ServiceProfile, error)) (options, error) {
	var opts options
	var startupProfile string
	fs := flag.NewFlagSet("displaykvmd", flag.ContinueOnError)
	fs.StringVar(&opts.settingsPath, "settings", "", "完整配置文件绝对路径，不与启动档案参数混用")
	fs.StringVar(&startupProfile, "startup-profile", "", "私有启动档案绝对路径，不与-settings混用")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, model.NewError(model.CodeInvalidRequest, "后台命令行参数无效", err)
	}
	if len(fs.Args()) != 0 {
		return opts, model.NewError(model.CodeInvalidRequest, "后台不接受额外位置参数", nil)
	}
	var hasProfile, hasSettings bool
	fs.Visit(func(entry *flag.Flag) {
		switch entry.Name {
		case "startup-profile":
			hasProfile = true
		case "settings":
			hasSettings = true
		}
	})
	if hasProfile && hasSettings {
		return opts, model.NewError(model.CodeInvalidRequest, "-settings和-startup-profile不能混用", nil)
	}
	if !hasProfile && !hasSettings {
		return opts, model.NewError(model.CodeInvalidRequest, "必须指定-settings或-startup-profile", nil)
	}
	if hasSettings && (opts.settingsPath == "" || !filepath.IsAbs(opts.settingsPath)) {
		return opts, model.NewError(model.CodeInvalidRequest, "-settings必须指定完整配置文件绝对路径", nil)
	}
	if !hasProfile {
		return opts, nil
	}
	if startupProfile == "" || !filepath.IsAbs(startupProfile) {
		return opts, model.NewError(model.CodeInvalidRequest, "-startup-profile必须指定私有档案绝对路径", nil)
	}
	profile, err := load(startupProfile)
	if err != nil {
		return opts, err
	}
	if profile.Version != 2 {
		return opts, model.NewError(model.CodeUnsupportedVersion, "不支持的启动档案版本", nil)
	}
	if !filepath.IsAbs(profile.ConfigPath) {
		return opts, model.NewError(model.CodeInvalidConfig, "启动档案必须选择完整配置文件绝对路径", nil)
	}
	opts.settingsPath = profile.ConfigPath
	return opts, nil
}

func run(ctx context.Context, opts options) (result error) {
	// 程序位置与后端同时绑定，并贯穿后续失败重读。命令行和文档不提供
	// 解析器入口，避免随包工具被PATH或另一套安装位置配置悄悄覆盖。
	if opts.toolResolver == (controltools.Resolver{}) {
		resolver, err := controltools.Current()
		if err != nil {
			return err
		}
		opts.toolResolver = resolver
	}
	store, err := settings.NewStore(settings.Options{})
	if err != nil {
		return err
	}
	unified, err := openUnifiedSettings(ctx, opts.settingsPath, opts.toolResolver, store)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, unified.writer.Close()) }()
	document := unified.snapshot.Document
	cfg, rules, schedules := document.Configuration, document.Rules, document.Schedules
	opts.toolPath, opts.busctlPath, opts.udevadmPath = unified.resolved.ToolPath, unified.resolved.BusctlPath, unified.resolved.UdevadmPath
	configDirectory, err := os.UserConfigDir()
	if err != nil || configDirectory == "" {
		return model.NewError(model.CodePermissionDenied, "无法确定当前用户配置目录", err)
	}
	instance, err := ipc.Acquire(configDirectory)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, instance.Close()) }()
	var selected backend.Backend
	switch runtime.GOOS {
	case "darwin":
		selected = darwinbackend.New(opts.toolPath, nil)
	case "linux":
		selected = linuxbackend.New(opts.toolPath, nil)
	case "windows":
		// 当前Windows原生传输未接线；保留NewNative的不可用说明，不回退其他后端。
		selected = windowsbackend.NewNative()
	default:
		return model.NewError(model.CodeBackendUnavailable, "当前平台没有可用显示器后端", nil)
	}
	// 没有规则且没有日程时明确关闭，不注册空规则引擎；没有自动化消费者时仍保留能力查询
	// 和emit_event不可用语义。真正引擎由后台调用Run并等待退出，不能自行启动。
	// 管理器让同一系统实例锁贯穿全部轮次，每轮仍只有一个队列和真实Engine。
	fmt.Fprintln(os.Stderr, "已加载完整配置文件中的自动化设置")
	if len(schedules) > 0 {
		fmt.Fprintf(os.Stderr, "已加载%d条显式日程；恢复跳过保证取决于真实恢复来源。\n", len(schedules))
	}
	// 队列与只读发现快照使用同一次严格加载的cfg，期间不重读原文件。必须先
	// 完成私有快照再发布端点，防止界面连入已运行后台后找不到真实分组和目标。
	manager := newRuntimeManager(opts, instance, selected, schedules)
	manager.settingsWriter, manager.settingsSnapshot, manager.saveSettings = unified.writer, unified.snapshot, unified.writer.Save
	err = manager.run(ctx, cfg, rules)
	// 仅纯取消表示正常停止；若取消与清理错误一起返回，仍继续向外报告真实失败。
	if err == context.Canceled {
		return nil
	}
	return err
}
