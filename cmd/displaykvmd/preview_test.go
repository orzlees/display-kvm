package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/settings"
)

func TestManagementPreviewStartCreatesBackgroundSessionForSavedTarget(t *testing.T) {
	m, cfg, rules, root, physical := managementFixture(t)
	// 保存目标为17，真实预读为18；二者必须不同，避免“无需切换”的分支
	// 掩盖后台预览会话未建立或错误复用普通Switch的问题。
	physical.mu.Lock()
	physical.value = 18
	physical.mu.Unlock()
	connection, snapshot := managementStart(t, m, cfg, rules, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, err := connection.Management(ctx, model.ManagementRequest{
		Action:             model.ManagementAction("preview_start"),
		ExpectedGeneration: snapshot.Generation,
		Data:               json.RawMessage(`{"request":{"id":"preview-saved-target","target_id":"work","display_ids":["left"]}}`),
	})
	if err != nil {
		t.Fatalf("已保存目标的预览应建立后台会话，不能作为未知管理动作拒绝: %v", err)
	}
	if response.Session == nil || response.Session.Kind != "input_preview" || response.Session.Generation != snapshot.Generation {
		t.Fatalf("预览没有返回与当前运行代关联的后台会话: %+v", response)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("后台预览会话缺少有效身份或到期时间: %v", err)
	}
	var status struct {
		State    string         `json:"state"`
		TargetID model.TargetID `json:"target_id"`
	}
	if err := json.Unmarshal(response.Data, &status); err != nil {
		t.Fatalf("后台预览未返回可查询的状态: %v", err)
	}
	if status.TargetID != "work" || status.State != "queued" && status.State != "running" {
		t.Fatalf("启动应返回已保存目标正在后台执行的状态: %+v", status)
	}
}

func TestManagementPreviewStartStatusAndLifetimeAreIndependentOfRequest(t *testing.T) {
	m, cfg, rules, root, physical := managementFixture(t)
	physical.mu.Lock()
	physical.value = 18
	physical.block, physical.entered = make(chan struct{}), make(chan struct{}, 1)
	release, entered := physical.block, physical.entered
	physical.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	connection, snapshot := managementStart(t, m, cfg, rules, root)
	before, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementAction("preview_status"), ExpectedGeneration: snapshot.Generation})
	if err != nil || before.Session != nil || len(before.Data) != 0 {
		t.Fatalf("没有预览时状态查询应为空: %+v %v", before, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	start, err := connection.Management(ctx, model.ManagementRequest{Action: model.ManagementAction("preview_start"), ExpectedGeneration: snapshot.Generation, Data: json.RawMessage(`{"request":{"id":"http-independent","target_id":"work","display_ids":["left"]}}`)})
	cancel()
	if err != nil || start.Session == nil {
		t.Fatalf("快速启动后台会话失败: %+v %v", start, err)
	}
	var queued model.InputPreviewStatus
	if err := model.DecodeJSON(start.Data, &queued); err != nil || queued.State != "queued" || len(queued.Displays) != 1 || queued.Displays[0].DisplayID != "left" || queued.Displays[0].TargetInput != 17 || queued.Displays[0].SwitchStatus != "pending" || queued.Displays[0].RestoreStatus != "pending" || queued.Displays[0].OriginalInput != nil {
		t.Fatalf("启动应答没有完整排队行，或伪造原输入: %+v %v", queued, err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("预览没有到达真实物理写边界")
	}
	m.mu.Lock()
	round := m.current
	m.sessions[start.Session.ID].view.ExpiresAt = time.Now().Add(-time.Second)
	m.mu.Unlock()
	latest, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementAction("preview_status"), ExpectedGeneration: snapshot.Generation})
	if err != nil || latest.Session == nil || latest.Session.ID != start.Session.ID {
		t.Fatalf("请求取消或会话过期抹掉了尚未完成的恢复责任: %+v %v", latest, err)
	}
	close(release)
	managementWait(t, func() bool {
		response, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementAction("preview_status"), ExpectedGeneration: snapshot.Generation, SessionID: start.Session.ID})
		if err != nil {
			t.Fatalf("已知会话查询失败: %v", err)
		}
		var status model.InputPreviewStatus
		if err := json.Unmarshal(response.Data, &status); err != nil {
			t.Fatal(err)
		}
		return status.Phase == "holding" && status.Displays[0].SwitchStatus == "verified"
	})
	physical.mu.Lock()
	value := physical.value
	physical.mu.Unlock()
	if value != 17 {
		t.Fatal("HTTP请求结束后后台未保持测试输入")
	}
	// 与配置换代/进程退出复用同一关闭受理入口，不能在Wait期间重新登记。
	m.closeRound(round)
	finished := make(chan struct{})
	go func() { round.previewWG.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("轮次关闭没有完成后台恢复")
	}
	physical.mu.Lock()
	value, writes := physical.value, physical.sets
	physical.mu.Unlock()
	m.mu.Lock()
	final := m.sessions[start.Session.ID].preview.Clone()
	m.mu.Unlock()
	if value != 18 || writes != 2 || final.State != "cancelled" || final.Displays[0].RestoreStatus != "restored" {
		t.Fatalf("轮次关闭没有等待真实恢复及最终快照: value=%d writes=%d status=%+v", value, writes, final)
	}
}

func TestManagementPreviewExpiryKeepsActiveTerminalSnapshotUntilResponsibilityReleased(t *testing.T) {
	now := time.Now()
	status := model.InputPreviewStatus{State: "completed", Phase: "finished", FinishedAt: &now}
	round := &runtimeRound{activePreview: "session-active", latestPreview: "session-active"}
	m := &runtimeManager{current: round, sessions: map[string]*managedSession{
		"session-active": {view: model.CalibrationSession{ExpiresAt: now.Add(-time.Second)}, preview: &status},
	}}
	m.mu.Lock()
	m.expireSessions()
	retained := m.sessions["session-active"] != nil
	round.activePreview = ""
	m.expireSessions()
	released := m.sessions["session-active"] == nil
	m.mu.Unlock()
	if !retained || !released {
		t.Fatalf("终态progress与释放active之间丢失会话，或释放后不再过期: retained=%v released=%v", retained, released)
	}
}

// 仅在物理恢复写处模拟不可抢占的原生调用；管理受理、配置提交、换代和队列
// 关闭均使用真实实现，借实际I/O边界证明新轮次不能提前取得后端所有权。
type previewRecoveryBackend struct {
	*managementBackend
	restoreEntered chan context.Context
	release        <-chan struct{}
}

func (b *previewRecoveryBackend) SetInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
	if scheme.Value == 18 {
		b.restoreEntered <- ctx
		<-b.release
	}
	return b.managementBackend.SetInput(ctx, device, scheme)
}

func TestManagementPreviewConfigGenerationWaitsForPhysicalRecovery(t *testing.T) {
	m, cfg, rules, root, physical := managementFixture(t)
	physical.mu.Lock()
	physical.value = 18
	physical.mu.Unlock()
	release := make(chan struct{})
	backend := &previewRecoveryBackend{managementBackend: physical, restoreEntered: make(chan context.Context, 1), release: release}
	m.selected = backend
	connection, snapshot := managementStart(t, m, cfg, rules, root)
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	started, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPreviewStart, ExpectedGeneration: snapshot.Generation, Data: json.RawMessage(`{"request":{"id":"generation-preview","target_id":"work"}}`)})
	if err != nil || started.Session == nil {
		t.Fatalf("启动换代场景预览失败: %+v %v", started, err)
	}
	managementWait(t, func() bool {
		response, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPreviewStatus, ExpectedGeneration: snapshot.Generation})
		if err != nil {
			t.Fatal(err)
		}
		var status model.InputPreviewStatus
		if err := model.DecodeJSON(response.Data, &status); err != nil {
			t.Fatal(err)
		}
		return status.Phase == "holding" && status.Displays[0].SwitchStatus == "verified"
	})
	m.mu.Lock()
	old := m.current
	m.mu.Unlock()
	// 运行轮次持有原配置快照；草稿只复制将修改的目标切片，不能在测试中
	// 直接编辑共享的运行配置来绕过真实提交和换代。
	cfg.Targets = append([]model.Target(nil), cfg.Targets...)
	cfg.Targets[0].Name = "恢复后新目标名称"
	encoded, err := encodeManagedConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.NewCommitID(snapshot.Generation)
	if err != nil {
		t.Fatal(err)
	}
	persisted := make(chan struct{}, 1)
	m.saveSettings = func(ctx context.Context, document settings.Document, revision settings.Revision) (settings.SaveResult, error) {
		persisted <- struct{}{}
		return m.settingsWriter.Save(ctx, document, revision)
	}
	if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPrepareConfig, ExpectedGeneration: snapshot.Generation, CommitID: id, Data: encoded}); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: snapshot.Generation, CommitID: id}); err != nil {
		t.Fatal(err)
	}
	var recovery context.Context
	select {
	case recovery = <-backend.restoreEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("换代未取消保持并进入恢复")
	}
	if _, bounded := recovery.Deadline(); !bounded || recovery.Err() != nil {
		t.Fatal("换代恢复没有独立有限预算")
	}
	select {
	case <-persisted:
		t.Fatal("物理恢复尚未返回即保存新配置")
	default:
	}
	m.mu.Lock()
	stillOwned := m.current == old && old.activePreview == started.Session.ID && old.closing
	m.mu.Unlock()
	if !stillOwned {
		t.Fatal("恢复尚未完成就清除旧轮次或活动会话")
	}
	finish()
	var fresh *client.Client
	var final model.ManagementResponse
	managementWait(t, func() bool {
		var err error
		fresh, err = client.Connect(root)
		if err != nil {
			return false
		}
		final, err = fresh.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: id})
		return err == nil && final.Commit != nil && final.Commit.Phase == model.CommitApplied && final.Generation != snapshot.Generation
	})
	physical.mu.Lock()
	value, writes := physical.value, physical.sets
	physical.mu.Unlock()
	m.mu.Lock()
	released := old.activePreview == "" && m.current != old
	m.mu.Unlock()
	if value != 18 || writes != 2 || !released || final.Commit.Saved == nil || !*final.Commit.Saved || final.Commit.Effective == nil || !*final.Commit.Effective {
		t.Fatalf("换代没有收口预览恢复及最终登记: value=%d writes=%d released=%v commit=%+v", value, writes, released, final.Commit)
	}
	status, err := fresh.Management(context.Background(), model.ManagementRequest{Action: model.ManagementPreviewStatus, ExpectedGeneration: final.Generation})
	if err != nil || status.Session != nil || len(status.Data) != 0 {
		t.Fatalf("新运行代继承了旧代预览: %+v %v", status, err)
	}
}

func TestManagementPreviewCloseRacesStartsWithoutAddingAfterWait(t *testing.T) {
	m, cfg, rules, root, physical := managementFixture(t)
	physical.mu.Lock()
	physical.value = 18
	physical.mu.Unlock()
	_, snapshot := managementStart(t, m, cfg, rules, root)
	m.mu.Lock()
	round := m.current
	m.mu.Unlock()
	gate := make(chan struct{})
	results := make(chan error, 20)
	var starts sync.WaitGroup
	for i := 0; i < 20; i++ {
		starts.Add(1)
		go func() {
			defer starts.Done()
			<-gate
			_, err := m.management(context.Background(), round, model.ManagementRequest{Action: model.ManagementPreviewStart, ExpectedGeneration: snapshot.Generation, Data: json.RawMessage(`{"request":{"id":"start-close-race","target_id":"work"}}`)})
			results <- err
		}()
	}
	closed := make(chan struct{})
	go func() {
		<-gate
		m.closeRound(round)
		round.previewWG.Wait()
		close(closed)
	}()
	close(gate)
	starts.Wait()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("关闭与Start竞态未收口后台任务")
	}
	close(results)
	for err := range results {
		if err != nil && model.CodeOf(err) != model.CodeBackendUnavailable {
			t.Fatalf("Start竞态产生非预期错误: %v", err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !round.closing || round.activePreview != "" {
		t.Fatalf("Wait返回后仍有活动预览: closing=%v active=%q", round.closing, round.activePreview)
	}
	for _, session := range m.sessions {
		if session.preview != nil && session.preview.FinishedAt == nil {
			t.Fatalf("Wait返回后新增或遗留未终结会话: %+v", session.preview)
		}
		if session.preview != nil {
			for _, display := range session.preview.Displays {
				if display.SwitchStatus == "pending" || display.RestoreStatus == "pending" {
					t.Fatalf("关闭竞态终态仍有永久pending行: %+v", display)
				}
			}
		}
	}
}

func TestManagementPreviewConcurrentStartsAndClosedRoundRejectRegistration(t *testing.T) {
	m, cfg, rules, root, physical := managementFixture(t)
	physical.mu.Lock()
	physical.value = 18
	physical.block = make(chan struct{})
	release := physical.block
	physical.mu.Unlock()
	t.Cleanup(func() { close(release) })
	connection, snapshot := managementStart(t, m, cfg, rules, root)
	gate := make(chan struct{})
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"concurrent-first", "concurrent-second"} {
		workers.Add(1)
		go func(id string) {
			defer workers.Done()
			<-gate
			_, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementAction("preview_start"), ExpectedGeneration: snapshot.Generation, Data: json.RawMessage(`{"request":{"id":"` + id + `","target_id":"work","display_ids":["left"]}}`)})
			results <- err
		}(id)
	}
	close(gate)
	workers.Wait()
	close(results)
	accepted, busy := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if model.CodeOf(err) == model.CodeBackendUnavailable {
			busy++
		} else {
			t.Fatalf("并发预览返回意外错误: %v", err)
		}
	}
	if accepted != 1 || busy != 1 {
		t.Fatalf("同运行代同时登记多个活动预览: accepted=%d busy=%d", accepted, busy)
	}
	m.mu.Lock()
	round := m.current
	m.mu.Unlock()
	m.closeRound(round)
	for i := 0; i < 20; i++ {
		_, err := m.management(context.Background(), round, model.ManagementRequest{Action: model.ManagementAction("preview_start"), ExpectedGeneration: snapshot.Generation, Data: json.RawMessage(`{"request":{"id":"after-close","target_id":"work"}}`)})
		if model.CodeOf(err) != model.CodeBackendUnavailable {
			t.Fatalf("已封闭受理的旧轮次继续登记预览: %v", err)
		}
	}
	m.mu.Lock()
	count := len(m.sessions)
	m.mu.Unlock()
	if count != 1 {
		t.Fatalf("关闭后生成了额外会话: %d", count)
	}
}

func TestManagementPreviewStatusRejectsStaleGenerationAndUnknownSession(t *testing.T) {
	m, cfg, rules, root, _ := managementFixture(t)
	connection, snapshot := managementStart(t, m, cfg, rules, root)
	for _, request := range []model.ManagementRequest{
		{Action: model.ManagementAction("preview_status"), ExpectedGeneration: "run-stale"},
		{Action: model.ManagementAction("preview_status"), ExpectedGeneration: snapshot.Generation, SessionID: "session-unknown"},
		{Action: model.ManagementAction("preview_status"), ExpectedGeneration: snapshot.Generation, Data: json.RawMessage(`{}`)},
		{Action: model.ManagementAction("preview_start"), ExpectedGeneration: snapshot.Generation, Data: json.RawMessage(`{"request":{"id":"unsaved-target","target_id":"missing"}}`)},
		{Action: model.ManagementAction("preview_start"), ExpectedGeneration: snapshot.Generation, Data: json.RawMessage(`{"request":{"id":"extra-field","target_id":"work"},"duration":1}`)},
	} {
		if _, err := connection.Management(context.Background(), request); err == nil {
			t.Fatalf("预览接受过期代、未知会话或非法负载: %+v", request)
		}
	}
}
