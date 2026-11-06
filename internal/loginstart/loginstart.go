// Package loginstart管理下一次用户登录时的两个独立任务，不管理当前进程生命周期。
package loginstart

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/orzlee/displayKvm/internal/model"
)

type Target string

const (
	Service Target = "service"
	Desktop Target = "desktop"
)

// ServiceProfile只记录完整配置文档的下次启动位置，不复制其中的工具、规则和日程。
// 旧多路径档案不自动迁移，防止不完整来源在普通保存或开关操作中被静默覆盖。
type ServiceProfile struct {
	Version    int    `json:"version"`
	ConfigPath string `json:"config_path"`
}

type ItemStatus struct {
	Target  Target `json:"target"`
	Present *bool  `json:"present"`
	Enabled *bool  `json:"enabled"`
	Loaded  *bool  `json:"loaded"`
	Ready   *bool  `json:"ready"`
	AppPath string `json:"app_path"`
	Message string `json:"message"`
}

type Status struct {
	Service        ItemStatus      `json:"service"`
	Desktop        ItemStatus      `json:"desktop"`
	Profile        *ServiceProfile `json:"profile"`
	ProfileMessage string          `json:"profile_message"`
}

type Result struct {
	Saved  *bool  `json:"saved"`
	Status Status `json:"status"`
}

type Environment struct {
	ConfigDir string
	HomeDir   string
	UID       int
}

type CommandRunner func(context.Context, string, ...string) ([]byte, error)

type Options struct {
	Environment func() (Environment, error)
	Run         CommandRunner
}

type Manager struct {
	env Environment
	run CommandRunner
}

// New只固定本次操作的用户边界，不探测后台，不创建目录。测试必须替换环境和命令
// 执行器，不能通过修改真实用户环境来间接访问登录项。
func New(opts Options) (*Manager, error) {
	if err := platformAvailable(); err != nil {
		return nil, err
	}
	if opts.Environment == nil {
		opts.Environment = func() (Environment, error) {
			configDir, configErr := os.UserConfigDir()
			homeDir, homeErr := os.UserHomeDir()
			return Environment{ConfigDir: configDir, HomeDir: homeDir, UID: os.Getuid()}, errors.Join(configErr, homeErr)
		}
	}
	env, err := opts.Environment()
	if err != nil {
		return nil, model.NewError(model.CodePermissionDenied, "无法确定登录启动的当前用户目录", err)
	}
	if !absolutePath(env.ConfigDir) || !absolutePath(env.HomeDir) || env.UID < 0 {
		return nil, model.NewError(model.CodePermissionDenied, "登录启动必须使用当前用户的绝对目录及有效用户编号", nil)
	}
	if opts.Run == nil {
		opts.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	return &Manager{env: env, run: opts.Run}, nil
}

func (m *Manager) profilePath() string {
	return filepath.Join(m.env.ConfigDir, "DisplayKVM", "startup.json")
}

func (m *Manager) Profile(ctx context.Context) (ServiceProfile, error) {
	if err := ctx.Err(); err != nil {
		return ServiceProfile{}, err
	}
	return loadProfile(m.profilePath(), m.env.UID)
}

func (m *Manager) SaveProfile(ctx context.Context, profile ServiceProfile) (Result, error) {
	return m.saveProfile(ctx, profile)
}

func (m *Manager) Status(ctx context.Context) (Status, error) { return m.status(ctx) }

func (m *Manager) Enable(ctx context.Context, target Target, appPath string) (Result, error) {
	return m.change(ctx, target, appPath, true)
}

func (m *Manager) Disable(ctx context.Context, target Target) (Result, error) {
	return m.change(ctx, target, "", false)
}

func emptyStatus() Status {
	return Status{Service: ItemStatus{Target: Service}, Desktop: ItemStatus{Target: Desktop}}
}

func boolean(value bool) *bool { return &value }

func absolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func label(target Target) string { return "io.github.orzlee.displaykvm." + string(target) }

func validTarget(target Target) error {
	if target != Service && target != Desktop {
		return model.NewError(model.CodeInvalidRequest, "登录启动目标必须为service或desktop", nil)
	}
	return nil
}

// 写入证据和系统状态分开：原子替换成功后，即使launchctl查询失败也不能谎报
// 未保存，否则界面可能重发已完成的副作用。写入后无法读回时saved才保留未知。
func (m *Manager) result(ctx context.Context, saved *bool, operationErr error) (Result, error) {
	status, statusErr := m.Status(ctx)
	return Result{Saved: saved, Status: status}, errors.Join(operationErr, statusErr)
}
