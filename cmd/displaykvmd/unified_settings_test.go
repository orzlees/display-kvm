package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/orzlee/displayKvm/internal/automation"
	"github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/loginstart"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/scheduler"
	"github.com/orzlee/displayKvm/internal/settings"
)

func TestUnifiedSettingsOptions(t *testing.T) {
	for _, name := range []string{"config", "rules", "schedules", "tool", "busctl", "udevadm", "startup-profile"} {
		for _, args := range [][]string{{"-settings", "/settings.json", "-" + name, ""}, {"-settings", "", "-" + name, "/explicit"}} {
			if _, err := parseOptions(args, func(string) (loginstart.ServiceProfile, error) {
				t.Fatal("互斥参数仍读取档案")
				return loginstart.ServiceProfile{}, nil
			}); model.CodeOf(err) != model.CodeInvalidRequest {
				t.Fatalf("未拒绝%v: %v", args, err)
			}
		}
	}
	for _, path := range []string{"", "relative.json"} {
		if _, err := parseOptions([]string{"-settings", path}, nil); model.CodeOf(err) != model.CodeInvalidRequest {
			t.Fatalf("接受无效完整路径%q: %v", path, err)
		}
	}
	root := t.TempDir()
	settingsPath := filepath.Join(root, "settings.json")
	profilePath := filepath.Join(root, "profile.json")
	opts, err := parseOptions([]string{"-startup-profile", profilePath}, func(string) (loginstart.ServiceProfile, error) {
		return loginstart.ServiceProfile{Version: 2, ConfigPath: settingsPath}, nil
	})
	if err != nil || opts.settingsPath != settingsPath || opts.toolPath != "" {
		t.Fatalf("v2未进入完整文档入口: %+v %v", opts, err)
	}
}

func unifiedRuntimeFixture(t *testing.T) (*runtimeManager, settings.Document, string) {
	t.Helper()
	m, _, _, root, _ := managementFixture(t)
	document := m.settingsSnapshot.Document
	document.Schedules = []scheduler.Schedule{{ID: "initial", Enabled: false, Time: "09:00", Timezone: "UTC"}}
	resolver, resolved := settingsProductTools(t, runtime.GOOS, filepath.Join(root, "bundle"))
	document.Runtime = settings.RuntimeSettings{}
	document.Desktop.Language = "en"
	managementSetDocument(t, m, document)
	m.opts.toolResolver = resolver
	m.opts.toolPath, m.opts.busctlPath, m.opts.udevadmPath = resolved.ToolPath, resolved.BusctlPath, resolved.UdevadmPath
	return m, document, root
}

func unifiedApply(t *testing.T, connection *client.Client, root string, generation model.RuntimeGeneration, request model.ManagementRequest) (*client.Client, model.CommitStatus, model.RuntimeGeneration) {
	t.Helper()
	id, err := client.NewCommitID(generation)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedGeneration, request.CommitID = generation, id
	if response, err := connection.Management(context.Background(), request); err != nil || response.Commit == nil || response.Commit.Phase != model.CommitAccepted {
		t.Fatalf("完整文档准备失败: %+v %v", response, err)
	}
	if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: generation, CommitID: id}); err != nil {
		t.Fatal(err)
	}
	var final model.ManagementResponse
	managementWait(t, func() bool {
		connection, err = client.Connect(root)
		if err != nil {
			return false
		}
		final, err = connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: id})
		return err == nil && final.Commit != nil && final.Generation != generation && (final.Commit.Phase == model.CommitApplied || final.Commit.Phase == model.CommitFailed || final.Commit.Phase == model.CommitUnknown)
	})
	return connection, *final.Commit, final.Generation
}

func TestSettingsProductSectionsAndCalibrationPreserveDesktopAndRuntime(t *testing.T) {
	m, wanted, root := unifiedRuntimeFixture(t)
	var saves atomic.Int32
	m.saveSettings = func(ctx context.Context, document settings.Document, revision settings.Revision) (settings.SaveResult, error) {
		saves.Add(1)
		return m.settingsWriter.Save(ctx, document, revision)
	}
	connection, snapshot := managementStart(t, m, wanted.Configuration, wanted.Rules, root)
	if !snapshot.RulesWritable {
		t.Fatal("完整文档规则被报告为只读")
	}
	state, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementSchedulesState, ExpectedGeneration: snapshot.Generation})
	var scheduleState model.ManagementSchedulesData
	if err != nil || model.DecodeJSON(state.Data, &scheduleState) != nil || !scheduleState.Writable {
		t.Fatalf("完整文档日程被报告为只读: %v", err)
	}
	generation := snapshot.Generation
	for _, action := range []model.ManagementAction{model.ManagementPrepareSchedules, model.ManagementPrepareRules, model.ManagementPrepareConfig, model.ManagementPrepareCalibration} {
		request := model.ManagementRequest{Action: action}
		switch action {
		case model.ManagementPrepareSchedules:
			wanted.Schedules = []scheduler.Schedule{{ID: "updated", Enabled: false, Time: "10:00", Timezone: "UTC"}}
			request.Data, err = encodeSchedules(wanted.Schedules)
		case model.ManagementPrepareRules:
			wanted.Rules = append([]automation.Rule{}, wanted.Rules...)
			wanted.Rules[0].Enabled = false
			request.Data, err = encodeRules(wanted.Configuration, wanted.Rules)
		case model.ManagementPrepareConfig:
			wanted.Configuration.Targets = append([]model.Target{}, wanted.Configuration.Targets...)
			wanted.Configuration.Targets[0].Name = "完整文档保存后"
			request.Data, err = encodeManagedConfig(wanted.Configuration)
		case model.ManagementPrepareCalibration:
			data, marshalErr := json.Marshal(model.ManagementCalibrationBeginData{DisplayID: "left", TargetID: "work", PortName: "完整文档校准", Scheme: wanted.Configuration.InputSchemes[0]})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			preview, beginErr := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCalibrationBegin, ExpectedGeneration: generation, Data: data})
			if beginErr != nil || preview.Session == nil {
				t.Fatalf("校准失败: %v", beginErr)
			}
			request.SessionID, request.Confirmed = preview.Session.ID, true
		}
		if err != nil {
			t.Fatal(err)
		}
		var status model.CommitStatus
		connection, status, generation = unifiedApply(t, connection, root, generation, request)
		if status.Saved == nil || !*status.Saved || status.Effective == nil || !*status.Effective || status.EffectiveGeneration != generation {
			t.Fatalf("缺完整文档落盘/运行代证据: %+v", status)
		}
		actual, readErr := m.settingsWriter.Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if action == model.ManagementPrepareCalibration {
			if len(actual.Document.Configuration.Calibrations) != 1 || actual.Document.Configuration.Calibrations[0].PortName != "完整文档校准" || actual.Document.Configuration.Calibrations[0].ConfirmedAt == nil {
				t.Fatalf("校准未同文件保存: %+v", actual.Document.Configuration.Calibrations)
			}
			wanted.Configuration.Calibrations = actual.Document.Configuration.Calibrations
		}
		if !reflect.DeepEqual(actual.Document, wanted) {
			t.Fatalf("保存%v覆盖相邻分区: %+v", action, actual.Document)
		}
	}
	if saves.Load() != 4 {
		t.Fatalf("每个事务未恰好保存一次: %d", saves.Load())
	}
}

func TestUnifiedSettingsFailedSaveReloadsOnlyCompleteDocument(t *testing.T) {
	for _, mode := range []string{"未替换", "已替换但报错", "未知保存结果"} {
		t.Run(mode, func(t *testing.T) {
			written := mode == "已替换但报错"
			m, document, root := unifiedRuntimeFixture(t)
			m.saveSettings = func(ctx context.Context, candidate settings.Document, revision settings.Revision) (settings.SaveResult, error) {
				result := settings.SaveResult{Saved: known(false)}
				if mode == "未知保存结果" {
					result.Saved = nil
				}
				if written {
					var err error
					result, err = m.settingsWriter.Save(ctx, candidate, revision)
					if err != nil {
						return result, err
					}
				}
				return result, model.NewError(model.CodeIO, "注入保存失败", nil)
			}
			connection, snapshot := managementStart(t, m, document.Configuration, document.Rules, root)
			payload, err := encodeSchedules([]scheduler.Schedule{})
			if err != nil {
				t.Fatal(err)
			}
			_, status, _ := unifiedApply(t, connection, root, snapshot.Generation, model.ManagementRequest{Action: model.ManagementPrepareSchedules, Data: payload})
			if mode == "未知保存结果" && status.Saved != nil || mode != "未知保存结果" && (status.Saved == nil || *status.Saved != written) || status.Effective == nil || *status.Effective != written || status.Error == nil {
				t.Fatalf("保存失败证据错误: %+v", status)
			}
		})
	}
	m, document, root := unifiedRuntimeFixture(t)
	beforeReload := m.settingsSnapshot.Document
	document.Runtime.ToolPath = filepath.Join(root, "changed-tool")
	if err := os.WriteFile(document.Runtime.ToolPath, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := settings.Encode(&encoded, document); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.opts.settingsPath, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, _, reloadErr := m.reload()
	if runtime.GOOS == "windows" {
		// Windows原生控制不绑定外部工具；历史覆盖可以变化，重读仍须保留原字段，
		// 且不能将文档覆盖写成当前后端的实际工具路径。
		if reloadErr != nil || !reflect.DeepEqual(m.settingsSnapshot.Document, document) || m.opts.toolPath != "" || m.opts.busctlPath != "" || m.opts.udevadmPath != "" {
			t.Fatalf("Windows历史工具覆盖影响原生重读或丢失字段: document=%+v err=%v", m.settingsSnapshot.Document, reloadErr)
		}
	} else if reloadErr == nil || !reflect.DeepEqual(m.settingsSnapshot.Document, beforeReload) {
		t.Fatalf("实际工具变化仍复用旧后端恢复或改写运行快照: %v", reloadErr)
	}
	afterReload, err := os.ReadFile(m.opts.settingsPath)
	if err != nil || !bytes.Equal(afterReload, encoded.Bytes()) {
		t.Fatal("重读工具字段改写了磁盘完整文档")
	}
	if err := os.WriteFile(m.opts.settingsPath, []byte(`{"kind":"displaykvm-settings","version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.reload(); err == nil {
		t.Fatal("损坏完整文档仍拼接旧多文件恢复")
	}
	for _, content := range []string{`{"version":1,"displays":[],"targets":[],"groups":[]}`, `{"version":2,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`, `{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`} {
		if err := os.WriteFile(m.opts.settingsPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := m.reload(); err == nil {
			t.Fatalf("接受裸配置恢复: %s", content)
		}
	}
}
