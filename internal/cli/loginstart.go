package cli

import (
	"context"

	"github.com/orzlee/displayKvm/internal/loginstart"
	"github.com/orzlee/displayKvm/internal/model"
)

func (r *runner) startup(ctx context.Context, args []string) (string, outcome) {
	if len(args) == 0 {
		return "startup", failure(usageError("startup", "必须指定status、configure、enable或disable"))
	}
	command := "startup " + args[0]
	fs := r.flags(command)
	profile := loginstart.ServiceProfile{Version: 2}
	var target loginstart.Target
	var app string
	switch args[0] {
	case "status":
	case "configure":
		fs.StringVar(&profile.ConfigPath, "config", "", "完整配置文件绝对路径")
	case "enable", "disable":
		if len(args) < 2 || (args[1] != string(loginstart.Service) && args[1] != string(loginstart.Desktop)) {
			return command, failure(usageError("target", "登录启动目标必须为service或desktop"))
		}
		target = loginstart.Target(args[1])
		args = append([]string{args[0]}, args[2:]...)
		if args[0] == "enable" {
			fs.StringVar(&app, "app", "", "已安装DisplayKvm.app的稳定绝对位置")
		}
	default:
		return command, failure(usageError("startup", "未知登录启动命令"))
	}
	if err := parse(fs, args[1:]); err != nil {
		return command, r.parseFailure(err)
	}
	// config-dir是既有IPC入口参数，不能偷偷变成第二个启动档案根目录，
	// 否则GUI、CLI和登录任务可能分别读取不同设置。
	if r.directory != "" {
		return command, failure(usageError("config-dir", "startup不接受IPC配置目录参数，使用当前用户固定启动设置目录"))
	}
	if args[0] == "configure" {
		if profile.ConfigPath == "" {
			return command, failure(usageError("config", "保存启动选择必须指定--config"))
		}
		store, err := r.settingsStore()
		if err != nil {
			return command, failure(err)
		}
		_, err = store.ReadFile(profile.ConfigPath)
		if err != nil {
			return command, failure(err)
		}
	}
	if args[0] == "enable" && app == "" {
		return command, failure(usageError("app", "启用登录启动必须指定--app稳定应用位置"))
	}
	manager, err := r.opts.NewLoginStart()
	if err != nil {
		return command, failure(err)
	}
	if manager == nil {
		return command, failure(model.NewError(model.CodeInternal, "启动设置管理器未返回有效实例", nil))
	}
	var result outcome
	switch args[0] {
	case "status":
		result.data, err = manager.Status(ctx)
	case "configure":
		result.data, err = manager.SaveProfile(ctx, profile)
	case "enable":
		result.data, err = manager.Enable(ctx, target, app)
	case "disable":
		result.data, err = manager.Disable(ctx, target)
	}
	if err != nil {
		result.err = err
		result.code = exitCode(err)
	}
	return command, result
}
