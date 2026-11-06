package startup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/orzlee/displayKvm/desktop/app/presentation"
	"os"

	"github.com/orzlee/displayKvm/internal/loginstart"
	"github.com/orzlee/displayKvm/internal/settings"
)

// Manager只包含共用登录启动能力；适配器不能借保存设置启动或停止当前服务。
type Manager interface {
	Profile(context.Context) (loginstart.ServiceProfile, error)
	SaveProfile(context.Context, loginstart.ServiceProfile) (loginstart.Result, error)
	Status(context.Context) (loginstart.Status, error)
	Enable(context.Context, loginstart.Target, string) (loginstart.Result, error)
	Disable(context.Context, loginstart.Target) (loginstart.Result, error)
}

type backend struct {
	manager Manager
	files   *settings.Store
	appPath string
}

func NewBackend(manager Manager, files *settings.Store, appPath string) Backend {
	return &backend{manager: manager, files: files, appPath: appPath}
}

func (b *backend) Read(ctx context.Context) (State, error) {
	if b.manager == nil {
		return State{}, messageError("settings.error.backend")
	}
	// 核心返回汇总错误时仍保留逐项证据，例如服务参数损坏但桌面Enabled可信。
	// 不能因一项失败丢弃另一项状态，否则连关闭正常桌面启动都被错误封锁。
	status, statusErr := b.manager.Status(ctx)
	state := State{Service: loginState(status.Service), Desktop: loginState(status.Desktop)}
	profile, err := b.manager.Profile(ctx)
	var profileErr error
	switch {
	case err == nil:
		state.ConfigPath = profile.ConfigPath
		state.ConfigKnown, state.ProfileExists = true, true
	case errors.Is(err, os.ErrNotExist):
		// 首次没有参数文件是可编辑状态；权限错误、格式损坏等不能当作首次安装，
		// 更不能根据状态消息的文字猜测后用空白配置覆盖原文件。
		state.ConfigKnown = true
	default:
		state.ConfigError = err.Error()
		profileErr = err
	}
	// 文档保存会改变Ready和说明文字，但不会改变用户选择。版本只跟踪实际
	// 可编辑值及其可信性，避免本次文件写成功后被自身状态变化误判为外部冲突；
	// 文件内容的并发保护仍由独立Revision和存储锁负责，不能省略。
	type itemVersion struct{ Supported, Known, Enabled bool }
	encoded, err := json.Marshal(struct {
		ConfigPath                 string
		ConfigKnown, ProfileExists bool
		Service, Desktop           itemVersion
	}{state.ConfigPath, state.ConfigKnown, state.ProfileExists,
		itemVersion{state.Service.Supported, state.Service.Known, state.Service.Enabled},
		itemVersion{state.Desktop.Supported, state.Desktop.Known, state.Desktop.Enabled}})
	if err != nil {
		return state, fmt.Errorf("无法确认设置版本：%w", err)
	}
	state.Version = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return state, errors.Join(statusErr, profileErr)
}

func loginState(item loginstart.ItemStatus) LoginState {
	state := LoginState{Supported: true, Known: item.Enabled != nil, Ready: item.Ready, Error: item.Message}
	if item.Enabled != nil {
		state.Enabled = *item.Enabled
	}
	return state
}

func (b *backend) Save(ctx context.Context, request SaveRequest) (SaveResult, error) {
	result := SaveResult{}
	current, readErr := b.Read(ctx)
	// 这是第一步写入前的乐观检查，用于拒绝已知过期页面。核心各操作分别加锁，
	// 检查之后CLI仍可能操作，不能把多步保存描述成跨进程原子事务。
	if request.Expected.Version != "" && current.Version != request.Expected.Version {
		result.Problems = []Problem{{Message: "settings.error.external_change"}}
		return result, nil
	}
	profileReady := current.ConfigKnown && current.ProfileExists
	if request.ConfigPath != nil {
		result.Configuration.Attempted = true
		// 配置选择只记录完整文档；控制程序由核心在启服务时解析并再次校验。
		// 仅禁用或只改桌面项不依赖文档有效，避免历史坏文件阻断安全关闭。
		if !current.ConfigKnown {
			result.Problems = []Problem{{Message: "settings.error.profile_unread"}, {Message: current.ConfigError}}
		} else if err := absolutePath(*request.ConfigPath); err != nil {
			result.Problems = []Problem{{Field: "config_path", Message: "settings.error.absolute_path"}}
		}
		if len(result.Problems) == 0 {
			profile := loginstart.ServiceProfile{Version: 2, ConfigPath: *request.ConfigPath}
			saved, saveErr := b.manager.SaveProfile(ctx, profile)
			result.Configuration = outcome(saved, saveErr)
		}
		profileReady = result.Configuration.Succeeded && !result.Configuration.Unknown
		if !profileReady && result.Configuration.Message == "" {
			result.Configuration.Message = "settings.error.profile_unsaved"
		}
	}
	for _, item := range []struct {
		target  loginstart.Target
		desired *bool
		state   LoginState
		result  *Outcome
	}{
		{loginstart.Service, request.ServiceEnabled, current.Service, &result.Service},
		{loginstart.Desktop, request.DesktopEnabled, current.Desktop, &result.Desktop},
	} {
		if item.desired == nil {
			continue
		}
		item.result.Attempted = true
		if !item.state.Known || !item.state.Supported {
			item.result.Message = "settings.error.item_unknown"
			if readErr != nil {
				item.result.Diagnostic = presentation.DiagnosticFromError(readErr)
			}
			continue
		}
		if item.target == loginstart.Service && *item.desired && !profileReady {
			item.result.Message = "settings.error.service_blocked"
			continue
		}
		var saved loginstart.Result
		var saveErr error
		if *item.desired {
			// 安装位置由宿主提供、核心验证，不在页面中搜索、复制应用或猜测路径。
			saved, saveErr = b.manager.Enable(ctx, item.target, b.appPath)
		} else {
			saved, saveErr = b.manager.Disable(ctx, item.target)
		}
		*item.result = outcome(saved, saveErr)
	}
	return result, nil
}

func (b *backend) ReadDocument(ctx context.Context, path string) (settings.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return settings.Snapshot{}, err
	}
	if b.files == nil {
		return settings.Snapshot{}, messageError("settings.error.files_unavailable")
	}
	return b.files.ReadFile(path)
}

func (b *backend) SaveDocument(ctx context.Context, path string, document settings.Document, revision settings.Revision) (result settings.SaveResult, err error) {
	if b.files == nil {
		return result, messageError("settings.error.files_unavailable")
	}
	writer, err := b.files.OpenWriter(ctx, path)
	if err != nil {
		return settings.SaveResult{Saved: boolValue(false)}, err
	}
	// 离线页面只在一次文档写入期间持锁；后台持锁时由共享存储拒绝覆盖。
	// Save负责Revision比较及原子保存，页面不复制持久化或竞争判断逻辑。
	defer func() { err = errors.Join(err, writer.Close()) }()
	return writer.Save(ctx, document, revision)
}

func outcome(result loginstart.Result, err error) Outcome {
	value := Outcome{Attempted: true, Unknown: result.Saved == nil}
	if result.Saved != nil {
		value.Succeeded = *result.Saved
	}
	if err != nil {
		value.Message = err.Error()
		value.Diagnostic = presentation.DiagnosticFromError(err)
	} else if value.Unknown {
		value.Message = "settings.error.system_unknown"
	} else if !value.Succeeded {
		value.Message = "settings.error.system_unsaved"
	}
	return value
}
