package main

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/orzlee/displayKvm/internal/automation"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/scheduler"
	"github.com/orzlee/displayKvm/internal/settings"
)

type unifiedSettings struct {
	writer   *settings.Writer
	snapshot settings.Snapshot
	resolved controltools.Paths
}

// 从首次读取前到全部运行代退出始终持有同一完整文档写者。若校验失败，立即
// 释放写锁；所有运行代共用该完整文档及其Revision，避免并行离线修改覆盖提交。
func openUnifiedSettings(ctx context.Context, path string, resolver controltools.Resolver, store *settings.Store) (*unifiedSettings, error) {
	if !filepath.IsAbs(path) {
		return nil, model.NewError(model.CodeInvalidConfig, "完整配置必须指定绝对路径", nil)
	}
	writer, err := store.OpenWriter(ctx, path)
	if err != nil {
		return nil, err
	}
	snapshot, err := writer.Read()
	var resolved controltools.Paths
	if err == nil {
		resolved, err = snapshot.Document.ValidateForService(resolver)
	}
	if err != nil {
		return nil, errors.Join(err, writer.Close())
	}
	return &unifiedSettings{writer: writer, snapshot: snapshot, resolved: resolved}, nil
}

// saveCandidate是唯一管理循环使用的保存边界。只替本次分区，保留最新快照的
// 其它分区、桌面语言和原始工具覆盖，按所读Revision恰好保存一次，防止相邻设置
// 互相覆盖；解析后的安装绝对路径只供后端使用，不能替代用户文档中的空默认值。
func (m *runtimeManager) saveCandidate(ctx context.Context, kind model.ManagementAction, cfg config.Config, rules []automation.Rule, schedules []scheduler.Schedule) persistenceResult {
	document := m.settingsSnapshot.Document
	switch kind {
	case model.ManagementPrepareConfig, model.ManagementPrepareCalibration:
		document.Configuration = cfg
	case model.ManagementPrepareRules:
		document.Rules = rules
	case model.ManagementPrepareSchedules:
		document.Schedules = schedules
	default:
		return persistenceResult{saved: known(false), err: managementError("kind", "未知完整配置保存分区")}
	}
	result, err := m.saveSettings(ctx, document, m.settingsSnapshot.Revision)
	if result.Snapshot != nil {
		m.settingsSnapshot = *result.Snapshot
	}
	return persistenceResult{saved: result.Saved, err: err}
}

func (m *runtimeManager) reloadUnifiedSettings() (config.Config, []automation.Rule, []scheduler.Schedule, error) {
	snapshot, err := m.settingsWriter.Read()
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	resolved, err := snapshot.Document.ValidateForService(m.opts.toolResolver)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	activeRuntime := controltools.Paths{ToolPath: m.opts.toolPath, BusctlPath: m.opts.busctlPath, UdevadmPath: m.opts.udevadmPath}
	// 后端及事件来源在启动时绑定工具路径。失败落盘后若磁盘工具已变化，重用旧
	// 后端会把新配置伪装成已生效，因此明确停止，交由下一次显式启动重新构造。
	// 空覆盖表示按程序位置使用包内工具，不代表后端路径为空；必须比较同一
	// 启动解析器得出的实际路径，否则正常默认设置也会被误判为工具变化。
	if resolved != activeRuntime {
		return config.Config{}, nil, nil, model.NewError(model.CodeInvalidConfig, "实际完整配置的运行工具已变化，当前后端无法恢复，请显式重新启动", nil)
	}
	m.settingsSnapshot = snapshot
	return snapshot.Document.Configuration, snapshot.Document.Rules, snapshot.Document.Schedules, nil
}
