package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/automation"
	"github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/daemon"
	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/ipc"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/scheduler"
	"github.com/orzlee/displayKvm/internal/settings"
)

// 本合约只隔离物理I/O，日程读写、IPC、提交表、运行轮次和核心状态均使用真实实现。
// 不等待真实定时到期，启用日程选择距当前至少数小时的墙钟，仅核对核心返回状态。
func TestSchedulesContract(t *testing.T) {
	readState := func(t *testing.T, connection *client.Client, generation model.RuntimeGeneration) ([]scheduler.Schedule, []scheduler.ScheduleStatus, model.ManagementSchedulesData) {
		t.Helper()
		response, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementSchedulesState, ExpectedGeneration: generation})
		if err != nil || response.Generation != generation {
			t.Fatalf("读取真实日程状态失败: %+v %v", response, err)
		}
		var data model.ManagementSchedulesData
		if err := model.DecodeJSON(response.Data, &data); err != nil {
			t.Fatal(err)
		}
		document, err := scheduler.LoadSchedules(bytes.NewReader(data.Document))
		if err != nil {
			t.Fatal(err)
		}
		var statuses struct {
			Items []scheduler.ScheduleStatus `json:"items"`
		}
		if err := model.DecodeJSON(data.Statuses, &statuses); err != nil {
			t.Fatal(err)
		}
		if statuses.Items == nil {
			t.Fatal("真实后台状态对象缺少非null列表")
		}
		return document, statuses.Items, data
	}
	apply := func(t *testing.T, connection *client.Client, root string, generation model.RuntimeGeneration, action model.ManagementAction, data []byte) (*client.Client, model.CommitStatus, model.RuntimeGeneration) {
		t.Helper()
		id, err := client.NewCommitID(generation)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := connection.Management(context.Background(), model.ManagementRequest{Action: action, ExpectedGeneration: generation, CommitID: id, Data: data})
		if err != nil || prepared.Commit == nil || prepared.Commit.Phase != model.CommitAccepted || prepared.Commit.Saved == nil || *prepared.Commit.Saved {
			t.Fatalf("预检错误: %+v %v", prepared, err)
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
			return err == nil && final.Commit != nil && final.Commit.ID == id && (final.Commit.Phase == model.CommitApplied || final.Commit.Phase == model.CommitFailed) && final.Generation != generation
		})
		return connection, *final.Commit, final.Generation
	}
	t.Run("真实落盘刷新且配置规则提交保留新日程", func(t *testing.T) {
		m, cfg, rules, root, physical := managementFixture(t)
		initial := []byte(`{"version":1,"schedules":[{"id":"old","enabled":false,"time":"01:30","timezone":"UTC"}]}`)
		var err error
		m.schedules, err = scheduler.LoadSchedules(bytes.NewReader(initial))
		if err != nil {
			t.Fatal(err)
		}
		doc := m.settingsSnapshot.Document
		doc.Schedules = m.schedules
		managementSetDocument(t, m, doc)
		connection, snapshot := managementStart(t, m, cfg, rules, root)
		_, _, data := readState(t, connection, snapshot.Generation)
		if !data.Writable {
			t.Fatal("完整文档日程不可保存")
		}
		if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementSchedulesState, ExpectedGeneration: "run-stale"}); err == nil {
			t.Fatal("接受跨代日程读取")
		}
		for _, invalid := range []string{`{"version":1,"schedules":[{"id":"bad","enabled":false,"time":"24:00","timezone":"UTC"}]}`, `{"version":1,"schedules":[{"id":"bad","enabled":false,"time":"12:00","timezone":"Local"}]}`, `{"version":1,"schedules":null}`} {
			id, _ := client.NewCommitID(snapshot.Generation)
			if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareSchedules, ExpectedGeneration: snapshot.Generation, CommitID: id, Data: json.RawMessage(invalid)}); err == nil {
				t.Fatal("后台接受非法日程")
			}
		}
		clock := time.Now().UTC().Add(12 * time.Hour).Format("15:04")
		document := []byte(`{"version":1,"schedules":[{"id":"new","enabled":true,"time":"` + clock + `","timezone":"UTC"}]}`)
		connection, status, generation := apply(t, connection, root, snapshot.Generation, model.ManagementPrepareSchedules, document)
		if status.Saved == nil || !*status.Saved || status.Effective == nil || !*status.Effective || status.EffectiveGeneration != generation {
			t.Fatalf("缺落盘/生效证据: %+v", status)
		}
		actual, err := m.settingsWriter.Read()
		if err != nil {
			t.Fatal(err)
		}
		loaded := actual.Document.Schedules
		if len(loaded) != 1 || loaded[0].ID != "new" || !loaded[0].Enabled {
			t.Fatalf("真实完整文件未更新: %+v", actual.Document)
		}
		for _, action := range []model.ManagementAction{model.ManagementPrepareConfig, model.ManagementPrepareRules} {
			var payload []byte
			if action == model.ManagementPrepareConfig {
				payload, err = encodeManagedConfig(cfg)
			} else {
				payload, err = encodeRules(cfg, rules)
			}
			if err != nil {
				t.Fatal(err)
			}
			connection, _, generation = apply(t, connection, root, generation, action, payload)
			got, statuses, _ := readState(t, connection, generation)
			m.mu.Lock()
			engine := m.current.engine
			m.mu.Unlock()
			if !reflect.DeepEqual(got, loaded) || engine == nil || !reflect.DeepEqual(statuses, engine.Status()) || len(statuses) != 1 || statuses[0].NextTrigger == nil || statuses[0].MissedPolicy != "skip" {
				t.Fatalf("后续提交覆盖日程或伪造状态: %+v %+v", got, statuses)
			}
		}
		physical.mu.Lock()
		writes := physical.sets
		physical.mu.Unlock()
		if writes != 0 {
			t.Fatal("保存日程直接写入硬件")
		}
	})
	t.Run("保存失败和实际恢复内容分别决定SavedEffective", func(t *testing.T) {
		for _, mode := range []string{"unchanged", "sync-error", "different-file"} {
			t.Run(mode, func(t *testing.T) {
				m, cfg, rules, root, _ := managementFixture(t)
				before := []byte(`{"version":1,"schedules":[{"id":"before","enabled":false,"time":"09:00","timezone":"UTC"}]}`)
				wanted := []byte(`{"version":1,"schedules":[{"id":"wanted","enabled":false,"time":"10:00","timezone":"UTC"}]}`)
				var err error
				m.schedules, err = scheduler.LoadSchedules(bytes.NewReader(before))
				if err != nil {
					t.Fatal(err)
				}
				doc := m.settingsSnapshot.Document
				doc.Schedules = m.schedules
				managementSetDocument(t, m, doc)
				original, err := os.ReadFile(m.opts.settingsPath)
				if err != nil {
					t.Fatal(err)
				}
				m.saveSettings = func(ctx context.Context, candidate settings.Document, revision settings.Revision) (settings.SaveResult, error) {
					if mode == "unchanged" {
						return settings.SaveResult{Saved: known(false)}, model.NewError(model.CodeIO, "未替换原文件", nil)
					}
					result, err := m.settingsWriter.Save(ctx, candidate, revision)
					if err != nil {
						return result, err
					}
					if mode == "different-file" {
						if err := os.WriteFile(m.opts.settingsPath, original, 0600); err != nil {
							return settings.SaveResult{}, err
						}
					}
					return result, model.NewError(model.CodeIO, "替换后目录同步或外部修改错误", nil)
				}
				connection, snapshot := managementStart(t, m, cfg, rules, root)
				connection, status, generation := apply(t, connection, root, snapshot.Generation, model.ManagementPrepareSchedules, wanted)
				wantSaved, wantEffective := mode != "unchanged", mode == "sync-error"
				if status.Saved == nil || *status.Saved != wantSaved || status.Effective == nil || *status.Effective != wantEffective || status.Error == nil {
					t.Fatalf("混淆真实保存与生效: %+v", status)
				}
				document, statuses, _ := readState(t, connection, generation)
				wantID := "before"
				if wantEffective {
					wantID = "wanted"
				}
				if len(document) != 1 || document[0].ID != wantID || len(statuses) != 1 || statuses[0].Schedule.ID != wantID || statuses[0].NextTrigger != nil {
					t.Fatalf("恢复未使用实际完整文档中的日程: %+v %+v", document, statuses)
				}
			})
		}
	})
	t.Run("失败恢复无法读取完整文档时停止且不复用旧轮次", func(t *testing.T) {
		m, cfg, rules, root, _ := managementFixture(t)
		initial := []byte(`{"version":1,"schedules":[{"id":"old","enabled":false,"time":"09:00","timezone":"UTC"}]}`)
		var err error
		m.schedules, err = scheduler.LoadSchedules(bytes.NewReader(initial))
		if err != nil {
			t.Fatal(err)
		}
		doc := m.settingsSnapshot.Document
		doc.Schedules = m.schedules
		managementSetDocument(t, m, doc)
		m.saveSettings = func(context.Context, settings.Document, settings.Revision) (settings.SaveResult, error) {
			if err := os.WriteFile(m.opts.settingsPath, []byte(`{"kind":"displaykvm-settings","version":1,"schedules":null}`), 0600); err != nil {
				return settings.SaveResult{}, err
			}
			return settings.SaveResult{}, model.NewError(model.CodeIO, "无法证明替换结果", nil)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- m.run(ctx, cfg, rules) }()
		stopped := false
		t.Cleanup(func() {
			cancel()
			if !stopped {
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("失败恢复测试未关停")
				}
			}
		})
		var connection *client.Client
		var generation model.RuntimeGeneration
		managementWait(t, func() bool {
			connection, err = client.Connect(root)
			if err != nil {
				return false
			}
			response, readErr := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementSnapshotAction})
			if readErr != nil || response.Snapshot == nil {
				return false
			}
			generation = response.Generation
			return true
		})
		m.mu.Lock()
		old := m.current
		m.mu.Unlock()
		id, err := client.NewCommitID(generation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Management(ctx, model.ManagementRequest{Action: model.ManagementPrepareSchedules, ExpectedGeneration: generation, CommitID: id, Data: json.RawMessage(`{"version":1,"schedules":[]}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Management(ctx, model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: generation, CommitID: id}); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			stopped = true
			if err == nil {
				t.Fatal("实际日程损坏仍报告正常恢复")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("实际日程损坏后仍继续运行")
		}
		m.mu.Lock()
		current := m.current
		status := m.commits[id].status
		m.mu.Unlock()
		if current != old || status.Phase != model.CommitUnknown || status.Saved != nil || status.Effective == nil || *status.Effective {
			t.Fatalf("重用了旧输入或伪造恢复证据: %+v", status)
		}
	})
	t.Run("无引擎首个日程空列表全部停用", func(t *testing.T) {
		m, cfg, _, root, _ := managementFixture(t)
		doc := m.settingsSnapshot.Document
		doc.Rules = []automation.Rule{}
		managementSetDocument(t, m, doc)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- m.run(ctx, cfg, nil) }()
		t.Cleanup(func() {
			cancel()
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		})
		var connection *client.Client
		var snapshot model.ManagementSnapshot
		managementWait(t, func() bool {
			var err error
			connection, err = client.Connect(root)
			if err != nil {
				return false
			}
			response, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementSnapshotAction})
			if err != nil || response.Snapshot == nil {
				return false
			}
			snapshot = *response.Snapshot
			return true
		})
		_, statuses, state := readState(t, connection, snapshot.Generation)
		if snapshot.EngineAvailable || len(statuses) != 0 || !state.Writable {
			t.Fatal("无引擎被误判不可编辑")
		}
		connection, _, generation := apply(t, connection, root, snapshot.Generation, model.ManagementPrepareSchedules, []byte(`{"version":1,"schedules":[{"id":"first","enabled":false,"time":"09:00","timezone":"UTC"}]}`))
		_, statuses, _ = readState(t, connection, generation)
		if len(statuses) != 1 || statuses[0].NextTrigger != nil {
			t.Fatal("全部停用状态错误")
		}
		connection, _, generation = apply(t, connection, root, generation, model.ManagementPrepareSchedules, []byte(`{"version":1,"schedules":[]}`))
		document, statuses, _ := readState(t, connection, generation)
		if len(document) != 0 || len(statuses) != 0 {
			t.Fatal("空文档未清除实际日程")
		}
	})
	t.Run("完整文档空日程可写且只使用同一文件", func(t *testing.T) {
		m, cfg, rules, root, _ := managementFixture(t)
		connection, snapshot := managementStart(t, m, cfg, rules, root)
		_, _, state := readState(t, connection, snapshot.Generation)
		if !state.Writable {
			t.Fatal("完整文档空日程被误报只读")
		}
		id, _ := client.NewCommitID(snapshot.Generation)
		if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareSchedules, ExpectedGeneration: snapshot.Generation, CommitID: id, Data: json.RawMessage(`{"version":1,"schedules":[]}`)}); err != nil {
			t.Fatalf("完整文档日程预检失败: %v", err)
		}
	})
}

// 仅替换物理I/O边界；诊断、队列、Daemon、规则引擎、协议和文件保存均用生产实现。
type managementBackend struct {
	mu      sync.Mutex
	value   uint16
	sets    int
	active  int
	block   chan struct{}
	entered chan struct{}
}

func (*managementBackend) Name() string { return "management-test" }
func (*managementBackend) Enumerate(context.Context) ([]model.Device, error) {
	return []model.Device{{BackendID: "physical-left", Name: "左屏", Identity: model.IdentityHints{Manufacturer: "ABC", ProductCode: "management-panel", Serial: "same", Path: "management-left"}}}, nil
}
func (*managementBackend) Capabilities(context.Context, model.Device) (model.DeviceCapabilities, error) {
	return model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolVCP}, CanReadInput: true}, nil
}
func (b *managementBackend) ReadInput(context.Context, model.Device, model.InputScheme) (model.InputObservation, error) {
	b.mu.Lock()
	value := b.value
	b.mu.Unlock()
	return model.InputObservation{Value: &value, ObservedAt: time.Now()}, nil
}
func (b *managementBackend) SetInput(ctx context.Context, _ model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
	b.mu.Lock()
	b.sets++
	b.active++
	block, entered := b.block, b.entered
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.active--; b.mu.Unlock() }()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return model.WriteReceipt{}, ctx.Err()
		}
	}
	b.mu.Lock()
	b.value = scheme.Value
	b.mu.Unlock()
	return model.WriteReceipt{SentAt: time.Now()}, nil
}

func managementFixture(t *testing.T) (*runtimeManager, config.Config, []automation.Rule, string, *managementBackend) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := automationConfig(t)
	// 配置构造本身不触发物理访问；管理集成场景必须提供完整身份和稳定原路径，
	// 由真实identity.Resolve确认，不能靠仅枚举一个对象绕过身份门禁。
	cfg.Displays[0].Identity.ProductCode = "management-panel"
	cfg.Displays[0].Identity.Path = "management-left"
	rules, err := automation.LoadRules(bytes.NewBufferString(automationRulesDocument))
	if err != nil {
		t.Fatal(err)
	}
	path, tool := filepath.Join(root, "settings.json"), filepath.Join(root, "tool")
	resolver := controltools.Resolver{GOOS: "darwin", Executable: filepath.Join(root, "displaykvmd")}
	for _, executable := range []string{tool, resolver.Executable} {
		if err := os.WriteFile(executable, []byte("fixture"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := settings.NewStore(settings.Options{Environment: func() (settings.Environment, error) {
		return settings.Environment{ConfigDir: root, UID: os.Getuid()}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	instance, err := ipc.Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := instance.Close(); err != nil {
			t.Error(err)
		}
	})
	physical := &managementBackend{value: 17}
	m := newRuntimeManager(options{settingsPath: path, toolPath: tool, toolResolver: resolver}, instance, physical, nil)
	m.settingsWriter, m.saveSettings = writer, writer.Save
	document := settings.NewDocument()
	document.Configuration, document.Rules, document.Runtime.ToolPath = cfg, rules, tool
	managementSetDocument(t, m, document)
	m.registerSources = func(*daemon.Daemon, options) error { return nil }
	return m, cfg, rules, root, physical
}

// 夹具在后台启动前通过真实Writer落盘并同步快照；失败恢复和Revision校验因此
// 使用同一份完整文档，不靠独立规则/日程文件或启动副本绕过生产保存边界。
func managementSetDocument(t *testing.T, m *runtimeManager, document settings.Document) {
	t.Helper()
	result, err := m.settingsWriter.Save(context.Background(), document, m.settingsSnapshot.Revision)
	if err != nil || result.Saved == nil || !*result.Saved || result.Snapshot == nil {
		t.Fatalf("准备完整文档夹具失败: %+v %v", result, err)
	}
	m.settingsSnapshot = *result.Snapshot
	m.schedules = document.Schedules
}

func managementWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("等待实际运行状态超时")
		case <-tick.C:
		}
	}
}

type managementSwitchReply struct {
	result model.SwitchResult
	err    error
}

func managementConfirmed(t *testing.T, reply managementSwitchReply, id model.RequestID) {
	t.Helper()
	if reply.err != nil || reply.result.RequestID != id || reply.result.Error != nil || reply.result.Validate() != nil || len(reply.result.Displays) != 1 {
		t.Fatalf("原连接未收到完整关联结果: %+v %v", reply.result, reply.err)
	}
	display := reply.result.Displays[0]
	if display.DisplayID != "left" || display.State != model.StateConfirmed || display.DesiredValue != 17 || display.SentAt == nil || display.Observation == nil || display.Observation.Value == nil || *display.Observation.Value != 17 || display.Observation.ObservedAt.Before(*display.SentAt) {
		t.Fatalf("原连接缺少发送/读回证据: %+v", display)
	}
}

func managementDrainGate(t *testing.T, connection *client.Client) {
	t.Helper()
	// Snapshot在Apply登记后就会被管理器拒绝，且不进入硬件队列。继续等待Daemon
	// 自己的排空拒绝，避免用Enumerate排到被阻塞Switch后面，或用短超时制造断连。
	managementWait(t, func() bool {
		_, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementSnapshotAction})
		var classified *model.Error
		return errors.As(err, &classified) && classified.Code == model.CodeBackendUnavailable && classified.Message == "后台正在排空已受理结果，未接受新操作"
	})
}

func managementStart(t *testing.T, m *runtimeManager, cfg config.Config, rules []automation.Rule, root string) (*client.Client, model.ManagementSnapshot) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.run(ctx, cfg, rules) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("管理器未关停")
		}
	})
	var connection *client.Client
	var snapshot model.ManagementSnapshot
	managementWait(t, func() bool {
		var err error
		connection, err = client.Connect(root)
		if err != nil {
			return false
		}
		response, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementSnapshotAction})
		if err != nil || response.Snapshot == nil {
			return false
		}
		snapshot = *response.Snapshot
		return snapshot.EngineAvailable
	})
	return connection, snapshot
}

// 每个来源实例固定本次启动时的已知空USB基线，只隔离系统事件I/O。
// Snapshot不刷新原始观测时间，Run不发送外部事件；补扫、时间校验和记录仍由
// 真实Daemon及同一Engine执行，避免把无来源的补扫失败误当作可成功的启动前提。
type managementEmptyUSBSource struct {
	observedAt time.Time
}

func (*managementEmptyUSBSource) Name() string { return "management-empty-usb" }
func (*managementEmptyUSBSource) Capabilities(ctx context.Context) ([]model.EventCapability, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []model.EventCapability{
		{Kind: model.EventUSBAdded, Available: true},
		{Kind: model.EventUSBRemoved, Available: true},
	}, nil
}
func (*managementEmptyUSBSource) Run(ctx context.Context, _ chan<- model.Event) error {
	<-ctx.Done()
	return ctx.Err()
}
func (s *managementEmptyUSBSource) Snapshot(ctx context.Context) (daemon.Baseline, error) {
	if err := ctx.Err(); err != nil {
		return daemon.Baseline{}, err
	}
	return daemon.Baseline{ObservedAt: s.observedAt, USBKnown: true, USBDevices: []model.USBDevice{}}, nil
}

func TestManagementRuntimePrepareApplyGenerationAndRealEngine(t *testing.T) {
	m, cfg, rules, root, physical := managementFixture(t)
	m.registerSources = func(service *daemon.Daemon, _ options) error {
		return service.RegisterEventSource(func() daemon.Source {
			return &managementEmptyUSBSource{observedAt: time.Now()}
		})
	}
	connection, snapshot := managementStart(t, m, cfg, rules, root)
	old := m.current
	var before []automation.Record
	managementWait(t, func() bool {
		records, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementRuleRecords, ExpectedGeneration: snapshot.Generation})
		if err != nil {
			t.Fatalf("查询真实Engine启动记录失败: %v", err)
		}
		if err := json.Unmarshal(records.Data, &before); err != nil {
			t.Fatalf("解码真实Engine启动记录失败: %v", err)
		}
		for _, record := range before {
			if record.Decision.Reason == "rescan_failed" {
				t.Fatalf("真实Engine启动补扫失败: %+v", record.Error)
			}
			if record.Decision.Reason == "rescan_completed" {
				return true
			}
		}
		return false
	})
	physical.mu.Lock()
	beforeSets := physical.sets
	physical.mu.Unlock()
	event := model.Event{ID: "preview", Kind: model.EventUSBAdded, Source: "usb", Name: "dock", USB: &model.USBDevice{VendorID: 1234, ProductID: 5678, Path: "dock"}, ObservedAt: time.Now()}
	data, err := json.Marshal(model.ManagementPreviewRulesData{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPreviewRules, ExpectedGeneration: snapshot.Generation, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	var decisions []automation.Decision
	if err := json.Unmarshal(preview.Data, &decisions); err != nil || len(decisions) == 0 {
		t.Fatalf("查询须走真实Engine: %s %v", preview.Data, err)
	}
	records, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementRuleRecords, ExpectedGeneration: snapshot.Generation})
	if err != nil {
		t.Fatal(err)
	}
	var history []automation.Record
	if err := json.Unmarshal(records.Data, &history); err != nil || !reflect.DeepEqual(history, before) {
		t.Fatalf("预览不得写记录: %s %v", records.Data, err)
	}
	physical.mu.Lock()
	sets := physical.sets
	physical.mu.Unlock()
	if sets != beforeSets {
		t.Fatal("预览不得写硬件")
	}
	// 草稿必须与活轮次分离；直接改共享切片会让测试先篡改后台内存再验证保存。
	cfg.Targets = append([]model.Target(nil), cfg.Targets...)
	cfg.Targets[0].Name = "保存后的工作机"
	encoded, err := encodeManagedConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.NewCommitID(snapshot.Generation)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareConfig, ExpectedGeneration: snapshot.Generation, CommitID: id, Data: encoded})
	if err != nil || prepared.Commit == nil || prepared.Commit.Phase != model.CommitAccepted || *prepared.Commit.Saved {
		t.Fatalf("Prepare只登记: %+v %v", prepared, err)
	}
	actual, err := m.settingsWriter.Read()
	if err != nil || reflect.DeepEqual(actual.Document.Configuration, cfg) {
		t.Fatal("Prepare不得保存")
	}
	persisted := make(chan struct{}, 1)
	m.saveSettings = func(ctx context.Context, document settings.Document, revision settings.Revision) (settings.SaveResult, error) {
		persisted <- struct{}{}
		// 旧队列已经退出；不能仅以新对象存在来冒充唯一队列证据。
		if _, err := old.queue.Switch(ctx, model.SwitchRequest{ID: "closed-check", TargetID: "work"}); err == nil {
			t.Error("持久化前旧队列仍受理")
		}
		physical.mu.Lock()
		active := physical.active
		physical.mu.Unlock()
		if active != 0 {
			t.Error("持久化前物理操作未结束")
		}
		return m.settingsWriter.Save(ctx, document, revision)
	}
	physical.mu.Lock()
	physical.value, physical.block, physical.entered = 18, make(chan struct{}), make(chan struct{}, 1)
	entered, release := physical.entered, physical.block
	physical.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	inFlight := make(chan managementSwitchReply, 1)
	go func() {
		result, err := connection.Switch(context.Background(), model.SwitchRequest{ID: "in-flight-before-apply", TargetID: "work"})
		inFlight <- managementSwitchReply{result, err}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("关停顺序场景未进入真实队列")
	}
	applied, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: snapshot.Generation, CommitID: id})
	if err != nil || applied.Commit == nil || applied.Commit.Phase != model.CommitApplying {
		t.Fatalf("Apply自身应答发生自等/丢失: %+v %v", applied, err)
	}
	managementDrainGate(t, connection)
	select {
	case <-persisted:
		t.Fatal("在途结果未完成就开始保存")
	default:
	}
	if _, err := connection.Switch(context.Background(), model.SwitchRequest{ID: "new-during-drain", TargetID: "work"}); model.CodeOf(err) != model.CodeBackendUnavailable {
		t.Fatalf("排空期新操作未拒绝: %v", err)
	}
	query, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: id})
	if err != nil || query.Commit == nil || query.Commit.Phase != model.CommitApplying {
		t.Fatalf("排空期同ID不可查询: %+v %v", query, err)
	}
	close(release)
	select {
	case reply := <-inFlight:
		managementConfirmed(t, reply, "in-flight-before-apply")
	case <-time.After(3 * time.Second):
		t.Fatal("旧轮次在途切换未结束")
	}
	var fresh *client.Client
	var status *model.CommitStatus
	managementWait(t, func() bool {
		var err error
		fresh, err = client.Connect(root)
		if err != nil {
			return false
		}
		response, err := fresh.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCommitStatus, ExpectedGeneration: snapshot.Generation, CommitID: id})
		if err != nil || response.Commit == nil {
			return false
		}
		status = response.Commit
		return status.Phase == model.CommitApplied
	})
	if status.Saved == nil || !*status.Saved || status.Effective == nil || !*status.Effective || status.EffectiveGeneration == snapshot.Generation {
		t.Fatalf("提交证据不一致: %+v", status)
	}
	if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementSnapshotAction}); err == nil {
		t.Fatal("旧端点不得继续接收")
	}
	if _, err := fresh.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareConfig, ExpectedGeneration: snapshot.Generation, CommitID: "commit-" + model.RequestID(snapshot.Generation) + "-stale", Data: encoded}); err == nil {
		t.Fatal("旧草稿必须拒绝")
	}
	loaded, err := fresh.Configuration()
	if err != nil || loaded.Targets[0].Name != cfg.Targets[0].Name {
		t.Fatalf("新代快照不一致: %+v %v", loaded, err)
	}
	if other, err := ipc.Acquire(root); err == nil {
		other.Close()
		t.Fatal("受控重建释放了单例锁")
	}
}

func TestManagementRuntimeDrainPreservesTrialEvidenceAndStop(t *testing.T) {
	for _, mode := range []string{"released", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			m, cfg, rules, root, physical := managementFixture(t)
			var saves atomic.Int32
			m.saveSettings = func(ctx context.Context, document settings.Document, revision settings.Revision) (settings.SaveResult, error) {
				saves.Add(1)
				return m.settingsWriter.Save(ctx, document, revision)
			}
			connection, snapshot := managementStart(t, m, cfg, rules, root)
			cfg.Targets = append([]model.Target(nil), cfg.Targets...)
			cfg.Targets[0].Name = "试切证据完成后保存"
			encoded, err := encodeManagedConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			id, err := client.NewCommitID(snapshot.Generation)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareConfig, ExpectedGeneration: snapshot.Generation, CommitID: id, Data: encoded}); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(model.ManagementTrialBeginData{Requests: []model.SwitchRequest{{ID: "trial-drain", TargetID: "work"}}})
			trial, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementTrialBegin, ExpectedGeneration: snapshot.Generation, Data: data})
			if err != nil || trial.Session == nil {
				t.Fatalf("真实试切计划失败: %+v %v", trial, err)
			}
			if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementTrialStep, ExpectedGeneration: snapshot.Generation, SessionID: trial.Session.ID}); model.CodeOf(err) != model.CodeInvalidRequest {
				t.Fatalf("未人工确认就接受步骤: %v", err)
			}
			physical.mu.Lock()
			physical.value, physical.block, physical.entered = 18, make(chan struct{}), make(chan struct{}, 1)
			entered, release := physical.entered, physical.block
			physical.mu.Unlock()
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			type reply struct {
				response model.ManagementResponse
				err      error
			}
			stepped := make(chan reply, 1)
			go func() {
				response, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementTrialStep, ExpectedGeneration: snapshot.Generation, SessionID: trial.Session.ID, Confirmed: true})
				stepped <- reply{response, err}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("真实试切未进入物理边界")
			}
			applied, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: snapshot.Generation, CommitID: id})
			if err != nil || applied.Commit == nil || applied.Commit.Phase != model.CommitApplying {
				t.Fatalf("Apply不能等待自身: %+v %v", applied, err)
			}
			managementDrainGate(t, connection)
			if saves.Load() != 0 {
				t.Fatal("试切在途时已经保存")
			}
			if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareConfig, ExpectedGeneration: snapshot.Generation, CommitID: "commit-" + model.RequestID(snapshot.Generation) + "-blocked", Data: encoded}); model.CodeOf(err) != model.CodeBackendUnavailable {
				t.Fatalf("排空期仍接受新修改: %v", err)
			}
			if mode == "stopped" {
				stopped, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementTrialStop, ExpectedGeneration: snapshot.Generation, SessionID: trial.Session.ID})
				if err != nil || stopped.Session == nil {
					t.Fatalf("排空期既有Stop通道不可用: %+v %v", stopped, err)
				}
				var progress diagnostics.TrialProgress
				if err := json.Unmarshal(stopped.Session.Progress, &progress); err != nil || !progress.Stopped {
					t.Fatalf("Stop没有明确停止证据: %+v %v", progress, err)
				}
			} else {
				close(release)
			}
			select {
			case actual := <-stepped:
				if actual.err != nil || actual.response.Session == nil || actual.response.Session.ID != trial.Session.ID || actual.response.Session.Generation != snapshot.Generation {
					t.Fatalf("原Step连接丢失关联结果: %+v %v", actual.response, actual.err)
				}
				var progress diagnostics.TrialProgress
				if err := json.Unmarshal(actual.response.Session.Progress, &progress); err != nil || progress.Running || progress.Next != 1 || len(progress.Results) != 1 || progress.Warning == "" {
					t.Fatalf("最终步骤/人工确认边界证据不完整: %+v %v", progress, err)
				}
				if mode == "released" {
					managementConfirmed(t, managementSwitchReply{result: progress.Results[0]}, "trial-drain")
				} else {
					result := progress.Results[0]
					if !progress.Stopped || result.RequestID != "trial-drain" || result.Validate() != nil || len(result.Displays) != 1 {
						t.Fatalf("停止后的最终结果不完整: %+v", progress)
					}
					display := result.Displays[0]
					if display.State != model.StateUnknown || display.SentAt != nil || display.Error == nil || display.Observation == nil || display.Observation.Value == nil || *display.Observation.Value != 18 {
						t.Fatalf("取消不能伪造发送或丢弃之前读回: %+v", display)
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("换代丢失原连接试切结果")
			}
			managementWait(t, func() bool {
				fresh, err := client.Connect(root)
				if err != nil {
					return false
				}
				response, err := fresh.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: id})
				return err == nil && response.Commit != nil && response.Commit.Phase == model.CommitApplied && response.Commit.EffectiveGeneration != snapshot.Generation
			})
			if saves.Load() != 1 {
				t.Fatalf("持久化次数=%d", saves.Load())
			}
		})
	}
}

func TestManagementRuntimeDrainFailureKeepsOldRuntimeAndFile(t *testing.T) {
	for _, mode := range []string{"deadline", "write-failed"} {
		t.Run(mode, func(t *testing.T) {
			m, cfg, rules, root, physical := managementFixture(t)
			var saves atomic.Int32
			m.saveSettings = func(ctx context.Context, document settings.Document, revision settings.Revision) (settings.SaveResult, error) {
				saves.Add(1)
				return m.settingsWriter.Save(ctx, document, revision)
			}
			if mode == "deadline" {
				// 仅收紧已有Drain context的期限；真实门禁及完成计数仍走生产实现，
				// 不等65秒、不伪造Drain返回值，也不复制任何运行代算法。
				m.drain = func(ctx context.Context, service *daemon.Daemon) error {
					expired, cancel := context.WithDeadline(ctx, time.Now())
					defer cancel()
					return service.Drain(expired)
				}
			}
			connection, snapshot := managementStart(t, m, cfg, rules, root)
			m.mu.Lock()
			old := m.current
			m.mu.Unlock()
			before, err := os.ReadFile(m.opts.settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Targets = append([]model.Target(nil), cfg.Targets...)
			cfg.Targets[0].Name = "排空失败不得保存"
			encoded, err := encodeManagedConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			id, err := client.NewCommitID(snapshot.Generation)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareConfig, ExpectedGeneration: snapshot.Generation, CommitID: id, Data: encoded}); err != nil {
				t.Fatal(err)
			}
			switchConnection := connection
			if mode == "write-failed" {
				endpoint, err := ipc.LoadEndpoint(root)
				if err != nil {
					t.Fatal(err)
				}
				switchConnection, err = client.NewWithTransport(endpoint, func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
					if err != nil {
						return nil, err
					}
					// RST让后台的最终写帧明确失败；普通TCP关闭可能仍把小帧写进内核，
					// 不能把客户端取消本身当成服务端WriteFrame失败的证据。
					if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
						conn.Close()
						return nil, err
					}
					return conn, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			physical.mu.Lock()
			physical.value, physical.block, physical.entered = 18, make(chan struct{}), make(chan struct{}, 1)
			entered, release := physical.entered, physical.block
			physical.mu.Unlock()
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			switchCtx, stopSwitch := context.WithCancel(context.Background())
			defer stopSwitch()
			inFlight := make(chan managementSwitchReply, 1)
			go func() {
				result, err := switchConnection.Switch(switchCtx, model.SwitchRequest{ID: "drain-failure-original", TargetID: "work"})
				inFlight <- managementSwitchReply{result, err}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("失败场景没有在途真实操作")
			}
			if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: snapshot.Generation, CommitID: id}); err != nil {
				t.Fatal(err)
			}
			if mode == "write-failed" {
				managementDrainGate(t, connection)
				stopSwitch()
			}
			managementWait(t, func() bool {
				response, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: id})
				if err != nil || response.Commit == nil || response.Commit.Phase != model.CommitFailed {
					return false
				}
				if response.Commit.Saved == nil || *response.Commit.Saved || response.Commit.Effective == nil || *response.Commit.Effective || response.Commit.Error == nil {
					t.Fatalf("排空失败冒充保存/生效: %+v", response.Commit)
				}
				if mode == "deadline" && response.Commit.Error.Code != model.CodeTimeout {
					t.Fatalf("实际期限错误被掩盖: %+v", response.Commit)
				}
				m.mu.Lock()
				same, applying := m.current == old, m.applying
				m.mu.Unlock()
				if !same {
					t.Fatal("排空失败更换运行代")
				}
				return !applying
			})
			if mode == "deadline" {
				close(release)
			}
			select {
			case actual := <-inFlight:
				if mode == "deadline" {
					managementConfirmed(t, actual, "drain-failure-original")
				} else if actual.err == nil {
					t.Fatal("外部断连不能冒充收到最终结果")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("排空失败遗留在途操作")
			}
			after, err := os.ReadFile(m.opts.settingsPath)
			if err != nil || !bytes.Equal(before, after) || saves.Load() != 0 {
				t.Fatalf("排空失败仍调用保存或改变文件: saves=%d %v", saves.Load(), err)
			}
			physical.mu.Lock()
			physical.block, physical.entered = nil, nil
			physical.mu.Unlock()
			result, err := connection.Switch(context.Background(), model.SwitchRequest{ID: "old-runtime-still-live", TargetID: "work"})
			managementConfirmed(t, managementSwitchReply{result, err}, "old-runtime-still-live")
			response, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementSnapshotAction})
			if err != nil || response.Generation != snapshot.Generation {
				t.Fatalf("旧轮次未恢复服务: %+v %v", response, err)
			}
		})
	}
}

func TestManagementCalibrationSessionCancelAndFailedSaveRetiresQueue(t *testing.T) {
	m, cfg, rules, root, physical := managementFixture(t)
	connection, snapshot := managementStart(t, m, cfg, rules, root)
	data, _ := json.Marshal(model.ManagementTrialBeginData{Requests: []model.SwitchRequest{{ID: "trial-one", TargetID: "work"}}})
	trial, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementTrialBegin, ExpectedGeneration: snapshot.Generation, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	physical.mu.Lock()
	physical.value = 18
	physical.block = make(chan struct{})
	physical.entered = make(chan struct{}, 1)
	entered := physical.entered
	physical.mu.Unlock()
	stepped := make(chan error, 1)
	go func() {
		_, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementTrialStep, ExpectedGeneration: snapshot.Generation, SessionID: trial.Session.ID, Confirmed: true})
		stepped <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("真实试切未进入后端")
	}
	stopped, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementTrialStop, ExpectedGeneration: snapshot.Generation, SessionID: trial.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	var progress diagnostics.TrialProgress
	if err := json.Unmarshal(stopped.Session.Progress, &progress); err != nil || !progress.Stopped {
		t.Fatalf("取消未保留停止证据: %+v %v", progress, err)
	}
	select {
	case <-stepped:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后在途试切未收口")
	}
	physical.mu.Lock()
	physical.block = nil
	physical.entered = nil
	physical.value = 17
	physical.mu.Unlock()
	beginData, _ := json.Marshal(model.ManagementCalibrationBeginData{DisplayID: "left", TargetID: "work", PortName: "HDMI 1", Scheme: cfg.InputSchemes[0]})
	calibration, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCalibrationBegin, ExpectedGeneration: snapshot.Generation, Data: beginData})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := client.NewCommitID(snapshot.Generation)
	prepared, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareCalibration, ExpectedGeneration: snapshot.Generation, CommitID: id, SessionID: calibration.Session.ID, Confirmed: true})
	if err != nil || prepared.Commit == nil {
		t.Fatalf("准备校准失败: %+v %v", prepared, err)
	}
	old := m.current
	m.saveSettings = func(ctx context.Context, document settings.Document, revision settings.Revision) (settings.SaveResult, error) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return m.settingsWriter.Save(cancelled, document, revision)
	}
	_, _ = connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: snapshot.Generation, CommitID: id})
	managementWait(t, func() bool {
		fresh, err := client.Connect(root)
		if err != nil {
			return false
		}
		response, err := fresh.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: id})
		return err == nil && response.Commit != nil && response.Commit.Phase == model.CommitFailed && response.Commit.Saved != nil && !*response.Commit.Saved
	})
	m.mu.Lock()
	same := m.current == old
	m.mu.Unlock()
	if same {
		t.Fatal("失败后不能复用已退役队列")
	}
	if _, err := old.diagnostic.InspectBackend(context.Background()); err == nil {
		t.Fatal("SaveCalibration失败后原诊断服务必须retired")
	}
	m.mu.Lock()
	remaining := len(m.sessions)
	m.mu.Unlock()
	if remaining != 0 {
		t.Fatal("换代必须失效旧校准/试切会话")
	}
}

func TestManagementSessionAndDraftExpiry(t *testing.T) {
	m, cfg, rules, root, _ := managementFixture(t)
	connection, snapshot := managementStart(t, m, cfg, rules, root)
	data, _ := json.Marshal(model.ManagementCalibrationBeginData{DisplayID: "left", TargetID: "work", PortName: "HDMI 1", Scheme: cfg.InputSchemes[0]})
	response, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCalibrationBegin, ExpectedGeneration: snapshot.Generation, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.sessions[response.Session.ID].view.ExpiresAt = time.Now().Add(-time.Second)
	m.mu.Unlock()
	id, _ := client.NewCommitID(snapshot.Generation)
	if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareCalibration, ExpectedGeneration: snapshot.Generation, CommitID: id, SessionID: response.Session.ID, Confirmed: true}); err == nil {
		t.Fatal("过期会话不能保存")
	}
	encoded, _ := encodeManagedConfig(cfg)
	if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareConfig, ExpectedGeneration: snapshot.Generation, CommitID: id, Data: encoded}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.commits[id].expires = time.Now().Add(-time.Second)
	m.mu.Unlock()
	if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: snapshot.Generation, CommitID: id}); err == nil {
		t.Fatal("过期Prepare不能Apply")
	}
}

func TestManagementCalibrationCommitPersistsMetadataInConfiguration(t *testing.T) {
	m, cfg, rules, root, _ := managementFixture(t)
	connection, snapshot := managementStart(t, m, cfg, rules, root)
	data, _ := json.Marshal(model.ManagementCalibrationBeginData{DisplayID: "left", TargetID: "work", PortName: "已确认的HDMI口", Scheme: cfg.InputSchemes[0]})
	preview, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCalibrationBegin, ExpectedGeneration: snapshot.Generation, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	var evidence diagnostics.CalibrationEvidence
	if err := json.Unmarshal(preview.Session.Evidence, &evidence); err != nil || evidence.ConfirmedAt != nil {
		t.Fatalf("预览不能冒充确认保存: %+v %v", evidence, err)
	}
	id, _ := client.NewCommitID(snapshot.Generation)
	if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareCalibration, ExpectedGeneration: snapshot.Generation, CommitID: id, SessionID: preview.Session.ID, Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	_, _ = connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: snapshot.Generation, CommitID: id})
	managementWait(t, func() bool {
		fresh, err := client.Connect(root)
		if err != nil {
			return false
		}
		response, err := fresh.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: id})
		return err == nil && response.Commit != nil && response.Commit.Phase == model.CommitApplied
	})
	actual, err := m.settingsWriter.Read()
	if err != nil {
		t.Fatal(err)
	}
	loaded := actual.Document.Configuration
	if len(loaded.Calibrations) != 1 || loaded.Calibrations[0].PortName != "已确认的HDMI口" || loaded.Calibrations[0].ConfirmedAt == nil || loaded.Calibrations[0].Basis != "readback" {
		t.Fatalf("名称/依据须与配置同文件保存: %+v", loaded.Calibrations)
	}
}
