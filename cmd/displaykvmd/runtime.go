package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/automation"
	"github.com/orzlee/displayKvm/internal/backend"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/daemon"
	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/ipc"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/scheduler"
	"github.com/orzlee/displayKvm/internal/settings"
	"github.com/orzlee/displayKvm/internal/switcher"
)

const managementLifetime = 15 * time.Minute

type runtimeRound struct {
	generation model.RuntimeGeneration
	cfg        config.Config
	rules      []automation.Rule
	schedules  []scheduler.Schedule
	service    *daemon.Daemon
	queue      *switcher.Queue
	diagnostic *diagnostics.Service
	engine     *scheduler.Scheduler
	life       context.Context
	cancel     context.CancelFunc
	done       chan error
	// closing、activePreview及latestPreview均由runtimeManager.mu保护。关闭受理
	// 与previewWG.Add必须同锁排序，不能在Wait返回后再登记旧轮次后台任务。
	closing       bool
	previewWG     sync.WaitGroup
	activePreview string
	latestPreview string
}

type managedSession struct {
	view        model.CalibrationSession
	calibration *diagnostics.Calibration
	trial       *diagnostics.Trial
	preview     *model.InputPreviewStatus
}

type managedCommit struct {
	status      model.CommitStatus
	expires     time.Time
	cfg         config.Config
	rules       []automation.Rule
	schedules   []scheduler.Schedule
	calibration *diagnostics.Calibration
	round       *runtimeRound
}

// runtimeManager贯穿全部轮次，拥有提交表及唯一的Apply执行通道。表只在本进程内
// 有效；容量耗尽明确拒绝新提交，不能淘汰ID后把重发误认成全新保存。
type runtimeManager struct {
	mu               sync.Mutex
	opts             options
	instance         *ipc.Instance
	selected         backend.Backend
	schedules        []scheduler.Schedule
	current          *runtimeRound
	sessions         map[string]*managedSession
	commits          map[model.RequestID]*managedCommit
	applying         bool
	operations       sync.WaitGroup
	requests         chan *managedCommit
	drain            func(context.Context, *daemon.Daemon) error
	registerSources  func(*daemon.Daemon, options) error
	settingsWriter   *settings.Writer
	settingsSnapshot settings.Snapshot
	saveSettings     func(context.Context, settings.Document, settings.Revision) (settings.SaveResult, error)
}

func newRuntimeManager(opts options, instance *ipc.Instance, selected backend.Backend, schedules []scheduler.Schedule) *runtimeManager {
	return &runtimeManager{opts: opts, instance: instance, selected: selected, schedules: append([]scheduler.Schedule{}, schedules...),
		sessions: make(map[string]*managedSession), commits: make(map[model.RequestID]*managedCommit),
		requests: make(chan *managedCommit, 1), registerSources: registerPlatformSources,
		drain: func(ctx context.Context, service *daemon.Daemon) error { return service.Drain(ctx) }}
}

func runtimeID(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", model.NewError(model.CodeInternal, "无法生成管理身份", err)
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func (m *runtimeManager) start(ctx context.Context, cfg config.Config, rules []automation.Rule, schedules []scheduler.Schedule) (_ *runtimeRound, result error) {
	id, err := runtimeID("run-")
	if err != nil {
		return nil, err
	}
	queue, err := switcher.New(m.selected, cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, queue.Close())
		}
	}()
	service, err := daemon.New(queue)
	if err != nil {
		return nil, err
	}
	diagnostic, err := diagnostics.New(queue, cfg)
	if err != nil {
		return nil, err
	}
	if err := m.registerSources(service, m.opts); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	// 每轮固定独立日程副本；闭包不能捕获启动时m.schedules，否则日程保存后的
	// 下一次配置或规则提交会悄悄恢复旧日程，提交回执也会与实际调度输入不一致。
	round := &runtimeRound{generation: model.RuntimeGeneration(id), cfg: cfg, rules: rules, schedules: append([]scheduler.Schedule{}, schedules...), service: service, queue: queue, diagnostic: diagnostic, life: life, cancel: cancel, done: make(chan error, 1)}
	defer func() {
		if result != nil {
			cancel()
		}
	}()
	if len(rules) > 0 || len(round.schedules) > 0 {
		if err := service.RegisterAutomation(func(runtime daemon.AutomationRuntime) (daemon.Automation, error) {
			engine, err := scheduler.New(runtime, cfg, rules, round.schedules, scheduler.Options{})
			if err == nil {
				m.mu.Lock()
				round.engine = engine
				m.mu.Unlock()
			}
			return engine, err
		}); err != nil {
			return nil, err
		}
	}
	if err := service.RegisterManagement(func(ctx context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		return m.management(ctx, round, request)
	}, func(caps []model.EventCapability) []model.EventCapability { return m.capabilities(round, caps) }); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, model.NewError(model.CodeTransport, "创建后台回环监听失败", err)
	}
	defer func() {
		if result != nil {
			listener.Close()
		}
	}()
	endpoint, err := m.instance.PublishRuntime(listener.Addr().String(), cfg)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.current = round
	m.mu.Unlock()
	go func() {
		err := service.Run(life, listener, endpoint.Token)
		m.closeRound(round)
		round.done <- err
	}()
	return round, nil
}

func (m *runtimeManager) run(ctx context.Context, cfg config.Config, rules []automation.Rule) error {
	round, err := m.start(ctx, cfg, rules, m.schedules)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			m.closeRound(round)
			err := <-round.done
			round.previewWG.Wait()
			m.operations.Wait()
			if err == context.Canceled {
				return nil
			}
			return err
		case err := <-round.done:
			m.closeRound(round)
			round.previewWG.Wait()
			m.operations.Wait()
			return err
		case commit := <-m.requests:
			// Prepare已在独立请求中应答。Apply不在处理器内等待自身的响应，而由此
			// 唯一管理循环等最终帧通知；此时不能取消旧life，否则队列虽保留了逐屏结果，
			// 原连接却已被关闭。最长业务期限加写帧期限只是排空上界，不是猜测完成的宽限期。
			drainCtx, finishDrain := context.WithTimeout(ctx, model.MaxRequestTimeout+model.ResponseTimeout)
			drainErr := m.drain(drainCtx, round.service)
			finishDrain()
			if drainErr != nil {
				// Drain失败会撤销准入门禁，旧队列及连接仍由原轮次拥有。未调用保存，
				// 因而saved=false可确认；不能因超时创建第二轮次或强行断开在途结果。
				m.finishCommit(commit, model.CommitFailed, known(false), known(false), "", drainErr)
				m.mu.Lock()
				m.applying = false
				m.mu.Unlock()
				continue
			}
			m.closeRound(round)
			stopErr := <-round.done
			round.previewWG.Wait()
			m.operations.Wait()
			if stopErr != nil && stopErr != context.Canceled {
				m.finishCommit(commit, model.CommitFailed, known(false), known(false), "", stopErr)
				return stopErr
			}
			var nextCfg config.Config
			var nextRules []automation.Rule
			var nextSchedules []scheduler.Schedule
			nextCfg, nextRules, nextSchedules, err = m.persist(ctx, commit)
			if err != nil {
				// 已关闭的队列绝不复用。只有实际文件能重新严格解码并验证交叉引用，
				// 才以该内容恢复全新一轮；无法证明时停留unknown并停止后台。
				nextCfg, nextRules, nextSchedules, err = m.reload()
				if err != nil {
					return err
				}
			}
			newRound, startErr := m.start(ctx, nextCfg, nextRules, nextSchedules)
			if startErr != nil {
				m.mu.Lock()
				saved := commit.status.Saved
				m.mu.Unlock()
				m.finishCommit(commit, model.CommitFailed, saved, known(false), "", startErr)
				return startErr
			}
			round = newRound
			m.mu.Lock()
			matches := false
			switch commit.status.Kind {
			case model.ManagementPrepareRules:
				matches = reflect.DeepEqual(nextRules, commit.rules)
			case model.ManagementPrepareSchedules:
				// 日程提交只能以真实重建使用的日程判断生效；配置不变不能作为证据。
				matches = reflect.DeepEqual(nextSchedules, commit.schedules)
			default:
				matches = reflect.DeepEqual(nextCfg, commit.cfg)
			}
			if matches {
				commit.status.Effective, commit.status.EffectiveGeneration = known(true), round.generation
				if commit.status.Saved != nil && *commit.status.Saved {
					commit.status.Phase = model.CommitApplied
				}
			} else {
				commit.status.Effective, commit.status.EffectiveGeneration = known(false), ""
			}
			m.applying = false
			for id, session := range m.sessions {
				if session.trial != nil {
					session.trial.Stop()
				}
				delete(m.sessions, id)
			}
			m.mu.Unlock()
		}
	}
}

// closeRound先封闭受理，再在锁外取消；即使服务因其它错误自行退出，也必须
// 在发布round.done前建立同一屏障。原生写及恢复由Daemon的Queue.Close收口，
// 调用方随后在锁外等待previewWG，确保会话最终状态已经发布而非仅硬件已停止。
func (m *runtimeManager) closeRound(round *runtimeRound) {
	m.mu.Lock()
	round.closing = true
	m.mu.Unlock()
	round.cancel()
}

func (m *runtimeManager) reload() (config.Config, []automation.Rule, []scheduler.Schedule, error) {
	// 保存报错不代表原文件未变，只重读当前Writer绑定的完整文档；不能借
	// 启动副本或其它文件拼接恢复，否则会把未知磁盘结果伪装成已回滚。
	return m.reloadUnifiedSettings()
}

func (m *runtimeManager) finishCommit(commit *managedCommit, phase model.CommitPhase, saved, effective *bool, generation model.RuntimeGeneration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	commit.status.Phase, commit.status.Saved, commit.status.Effective, commit.status.EffectiveGeneration = phase, saved, effective, generation
	if err != nil {
		commit.status.Error = ipc.PublicError(err)
	}
}

func encodeRules(cfg config.Config, rules []automation.Rule) ([]byte, error) {
	if err := automation.ValidateRules(cfg, rules); err != nil {
		return nil, err
	}
	if rules == nil {
		rules = []automation.Rule{}
	}
	data, err := json.Marshal(struct {
		Version int               `json:"version"`
		Rules   []automation.Rule `json:"rules"`
	}{1, rules})
	if err != nil {
		return nil, err
	}
	if _, err := automation.LoadRules(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return data, nil
}

func encodeSchedules(schedules []scheduler.Schedule) ([]byte, error) {
	if err := scheduler.ValidateSchedules(schedules); err != nil {
		return nil, err
	}
	if schedules == nil {
		schedules = []scheduler.Schedule{}
	}
	data, err := json.Marshal(struct {
		Version   int                  `json:"version"`
		Schedules []scheduler.Schedule `json:"schedules"`
	}{1, schedules})
	if err != nil {
		return nil, err
	}
	if _, err := scheduler.LoadSchedules(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return data, nil
}

func (m *runtimeManager) persist(ctx context.Context, commit *managedCommit) (config.Config, []automation.Rule, []scheduler.Schedule, error) {
	m.finishCommit(commit, model.CommitSaving, known(false), known(false), "", nil)
	cfg, rules := commit.round.cfg, commit.round.rules
	schedules := append([]scheduler.Schedule{}, commit.round.schedules...)
	result := persistenceResult{saved: known(false)}
	var err error
	switch commit.status.Kind {
	case model.ManagementPrepareConfig:
		cfg = commit.cfg
	case model.ManagementPrepareRules:
		rules = commit.rules
	case model.ManagementPrepareCalibration:
		_, err = commit.round.diagnostic.SaveCalibration(ctx, commit.calibration, true, func(ctx context.Context, updated config.Config, evidence diagnostics.CalibrationEvidence) error {
			metadata := config.CalibrationMetadata{DisplayID: evidence.DisplayID, TargetID: evidence.TargetID, PortName: evidence.PortName, Scheme: evidence.Scheme, Basis: string(evidence.Basis), Observation: evidence.Observation, ObservationIssue: evidence.ObservationIssue, ConfirmedAt: evidence.ConfirmedAt}
			found := false
			for i, old := range updated.Calibrations {
				if old.DisplayID == metadata.DisplayID && old.TargetID == metadata.TargetID {
					updated.Calibrations[i] = metadata
					found = true
				}
			}
			if !found {
				updated.Calibrations = append(updated.Calibrations, metadata)
			}
			if err := automation.ValidateRules(updated, rules); err != nil {
				return err
			}
			result = m.saveCandidate(ctx, commit.status.Kind, updated, rules, schedules)
			cfg = updated
			commit.cfg = updated
			return result.err
		})
	case model.ManagementPrepareSchedules:
		schedules = append([]scheduler.Schedule{}, commit.schedules...)
	}
	if commit.status.Kind != model.ManagementPrepareCalibration {
		result = m.saveCandidate(ctx, commit.status.Kind, cfg, rules, schedules)
	}
	err = errors.Join(err, result.err)
	if result.saved == nil {
		if err == nil {
			err = model.NewError(model.CodeIO, "没有可证明的保存结果", nil)
		}
		m.finishCommit(commit, model.CommitUnknown, nil, known(false), "", err)
		return cfg, rules, schedules, err
	}
	if err != nil || !*result.saved {
		if err == nil {
			err = model.NewError(model.CodeIO, "实际文件未保存为准备提交内容", nil)
		}
		m.finishCommit(commit, model.CommitFailed, result.saved, known(false), "", err)
		return cfg, rules, schedules, err
	}
	m.finishCommit(commit, model.CommitApplying, known(true), known(false), "", nil)
	return cfg, rules, schedules, nil
}

func timerRuntimeState(configured, available bool, caps []model.EventCapability) model.TimerRuntimeState {
	timer := model.TimerRuntimeState{Configured: configured, Running: available && configured}
	for _, capability := range caps {
		if capability.Kind == model.EventResume {
			timer.ResumeAvailable = capability.Available
		}
		if capability.Kind == model.EventTimer {
			timer.Reason, timer.Running = capability.Reason, capability.Available
		}
	}
	timer.SkipGuaranteed = timer.Running && timer.ResumeAvailable
	return timer
}

func (m *runtimeManager) capabilities(round *runtimeRound, caps []model.EventCapability) []model.EventCapability {
	m.mu.Lock()
	engine := round.engine
	running := m.current == round && engine != nil && !m.applying
	m.mu.Unlock()
	enabled := false
	if engine != nil {
		for _, status := range engine.Status() {
			enabled = enabled || status.Schedule.Enabled && status.NextTrigger != nil
		}
	}
	resume := false
	for _, capability := range caps {
		if capability.Kind == model.EventResume {
			resume = capability.Available
		}
	}
	for i := range caps {
		if caps[i].Kind == model.EventTimer {
			caps[i].Available = running && enabled
			caps[i].Reason = "未配置启用的日程或当前日程运行实例尚未可用"
			if caps[i].Available {
				caps[i].Reason = "日程仅产生事件，实际动作由同一规则引擎决定"
				if !resume {
					caps[i].Reason += "；没有真实恢复来源，不能保证休眠错过跳过"
				}
			}
			caps[i].FilterFields = []string{"name", "source", "attributes.scheduled_at", "attributes.timezone", "attributes.missed_policy"}
		}
	}
	return caps
}

func managementError(field, text string) *model.Error {
	err := model.NewError(model.CodeInvalidRequest, text, nil)
	err.Field = field
	return err
}

func managementData(data json.RawMessage, target any) error {
	if len(data) == 0 {
		return managementError("data", "操作缺少数据")
	}
	if err := model.DecodeJSON(data, target); err != nil {
		return model.NewError(model.CodeInvalidRequest, "管理数据结构无效", err)
	}
	return nil
}

func (m *runtimeManager) management(ctx context.Context, round *runtimeRound, request model.ManagementRequest) (model.ManagementResponse, error) {
	if err := request.Validate(); err != nil {
		return model.ManagementResponse{}, err
	}
	m.mu.Lock()
	response := model.ManagementResponse{Generation: round.generation}
	if m.current != nil {
		response.Generation = m.current.generation
	}
	if request.Action == model.ManagementCommitStatus {
		if len(request.Data) != 0 {
			m.mu.Unlock()
			return response, managementError("data", "提交查询不接受负载")
		}
		if commit := m.commits[request.CommitID]; commit != nil {
			m.expireCommit(commit)
			status := commit.status
			response.Commit = &status
		} else {
			response.Commit = &model.CommitStatus{ID: request.CommitID, Phase: model.CommitUnknown}
		}
		m.mu.Unlock()
		return response, nil
	}
	if m.current != round || round.closing || m.applying && request.Action != model.ManagementTrialStop {
		m.mu.Unlock()
		return response, model.NewError(model.CodeBackendUnavailable, "运行代正在关停或已更换，请重新连接后查询", nil)
	}
	if request.Action != model.ManagementSnapshotAction && request.ExpectedGeneration != round.generation {
		m.mu.Unlock()
		return response, managementError("expected_generation", "草稿运行代已过期，请重新读取后编辑")
	}
	if request.Action == model.ManagementApplyCommit {
		commit := m.commits[request.CommitID]
		if commit == nil {
			m.mu.Unlock()
			return response, managementError("commit_id", "没有已登记的准备提交；不能自动重放")
		}
		m.expireCommit(commit)
		if commit.status.Phase != model.CommitAccepted {
			m.mu.Unlock()
			return response, managementError("commit_id", "提交已应用、过期或正在执行，不能重复Apply")
		}
		if len(request.Data) != 0 {
			m.mu.Unlock()
			return response, managementError("data", "Apply不接受替换草稿")
		}
		m.applying = true
		commit.status.Phase = model.CommitApplying
		status := commit.status
		response.Commit = &status
		m.requests <- commit
		m.mu.Unlock()
		return response, nil
	}
	// 登记屏障与applying共用锁；Apply之后仅保留取消既有试切的通道，换代先等待
	// Daemon的最终响应屏障，再等待这些管理操作退出，不能与新登记操作并发Wait。
	m.operations.Add(1)
	m.mu.Unlock()
	defer m.operations.Done()
	switch request.Action {
	case model.ManagementSnapshotAction:
		if len(request.Data) != 0 {
			return response, managementError("data", "快照不接受负载")
		}
		cfg, err := encodeManagedConfig(round.cfg)
		if err != nil {
			return response, err
		}
		rules, err := encodeRules(round.cfg, round.rules)
		if err != nil {
			return response, err
		}
		caps := round.service.Capabilities()
		m.mu.Lock()
		available := round.engine != nil
		m.mu.Unlock()
		timer := timerRuntimeState(len(round.schedules) > 0, available, caps)
		response.Snapshot = &model.ManagementSnapshot{Generation: round.generation, Configuration: cfg, Rules: rules, RulesWritable: m.settingsWriter != nil, EngineAvailable: available, Capabilities: caps, Timer: timer}
	case model.ManagementPreviewStart:
		var data model.ManagementPreviewStartData
		if err := managementData(request.Data, &data); err != nil {
			return response, err
		}
		if request.SessionID != "" || request.CommitID != "" || request.Confirmed {
			return response, managementError("data", "预览启动仅接受已保存目标请求")
		}
		// 纯配置规划复用队列的保存映射规则，不持管理锁、不入队或等待硬件。
		initial, err := round.queue.PlanPreview(data.Request)
		if err != nil {
			return response, err
		}
		sessionID, err := runtimeID("session-")
		if err != nil {
			return response, err
		}
		session := &managedSession{view: model.CalibrationSession{ID: sessionID, Kind: "input_preview", Generation: round.generation, ExpiresAt: time.Now().Add(managementLifetime)}, preview: &initial}
		m.mu.Lock()
		// 上面的严格解码和目标检查不持锁。此处必须重新核对全部受理条件，
		// 再与活动会话及Add一并登记；仅在入口初检会留下关闭后Add的竞态。
		if m.current != round || m.applying || round.closing || round.life.Err() != nil || request.ExpectedGeneration != round.generation {
			m.mu.Unlock()
			return response, model.NewError(model.CodeBackendUnavailable, "运行代正在关停或已更换，未接受预览", nil)
		}
		m.expireSessions()
		if round.activePreview != "" {
			m.mu.Unlock()
			return response, model.NewError(model.CodeBackendUnavailable, "已有目标预览尚未完成恢复，请查询当前会话", nil)
		}
		if len(m.sessions) >= 32 {
			m.mu.Unlock()
			return response, managementError("session_id", "管理会话已达32个上限，请等待过期")
		}
		m.sessions[sessionID] = session
		round.activePreview, round.latestPreview = sessionID, sessionID
		round.previewWG.Add(1)
		view := session.view
		response.Session = &view
		response.Data, err = json.Marshal(initial.Clone())
		m.mu.Unlock()
		// 启动应答的context只拥有短管理请求，不拥有五秒保持及恢复责任。
		// 完整作业由本轮次生命context及专用WaitGroup持有，不延长operations。
		go m.runPreview(round, sessionID, data.Request)
		if err != nil {
			return response, err
		}
	case model.ManagementPreviewStatus:
		if len(request.Data) != 0 {
			return response, managementError("data", "预览查询不接受负载")
		}
		m.mu.Lock()
		m.expireSessions()
		id := request.SessionID
		if id == "" {
			id = round.latestPreview
		}
		session := m.sessions[id]
		if session == nil {
			m.mu.Unlock()
			if request.SessionID != "" {
				return response, managementError("session_id", "预览会话不存在或已过期")
			}
			return response, nil
		}
		if session.preview == nil || session.view.Generation != round.generation {
			m.mu.Unlock()
			return response, managementError("session_id", "预览会话不属于当前运行代")
		}
		view := session.view
		status := session.preview.Clone()
		m.mu.Unlock()
		response.Session = &view
		var err error
		response.Data, err = json.Marshal(status)
		if err != nil {
			return response, err
		}
	case model.ManagementSchedulesState:
		if len(request.Data) != 0 {
			return response, managementError("data", "日程读取不接受负载")
		}
		document, err := encodeSchedules(round.schedules)
		if err != nil {
			return response, err
		}
		m.mu.Lock()
		engine := round.engine
		m.mu.Unlock()
		statuses := []scheduler.ScheduleStatus{}
		// Status内部有调度锁，不能持管理锁调用；operations屏障保证读取结束前
		// 当前轮次不会被提交事务替换。无引擎时保留真实空状态，仍允许保存首条日程。
		if engine != nil {
			statuses = engine.Status()
		}
		// 严格解码入口只接受对象，核心原始列表必须放入统一的items对象中；
		// 无实例时仍发送显式空列表，不能用null替代“确实没有运行状态”的证据。
		encoded, err := json.Marshal(struct {
			Items []scheduler.ScheduleStatus `json:"items"`
		}{statuses})
		if err != nil {
			return response, err
		}
		caps := round.service.Capabilities()
		response.Data, err = json.Marshal(model.ManagementSchedulesData{Document: document, Writable: m.settingsWriter != nil, Statuses: encoded, Timer: timerRuntimeState(len(round.schedules) > 0, engine != nil, caps)})
		if err != nil {
			return response, err
		}
	case model.ManagementDiagnose:
		var data model.ManagementDiagnoseData
		if err := managementData(request.Data, &data); err != nil {
			return response, err
		}
		var value any
		var err error
		if data.DisplayID == "" {
			if data.SchemeID != "" {
				return response, managementError("scheme_id", "方案诊断必须指定显示器")
			}
			value, err = round.diagnostic.InspectBackend(ctx)
		} else {
			value, err = round.diagnostic.Inspect(ctx, data.DisplayID, data.SchemeID)
		}
		if err != nil {
			return response, err
		}
		response.Data, err = json.Marshal(value)
		if err != nil {
			return response, err
		}
	case model.ManagementCalibrationBegin, model.ManagementTrialBegin:
		sessionID, err := runtimeID("session-")
		if err != nil {
			return response, err
		}
		session := &managedSession{view: model.CalibrationSession{ID: sessionID, Generation: round.generation, ExpiresAt: time.Now().Add(managementLifetime)}}
		if request.Action == model.ManagementCalibrationBegin {
			var data model.ManagementCalibrationBeginData
			if err := managementData(request.Data, &data); err != nil {
				return response, err
			}
			session.calibration, err = round.diagnostic.BeginCalibration(ctx, diagnostics.CalibrationRequest{DisplayID: data.DisplayID, TargetID: data.TargetID, PortName: data.PortName, Scheme: data.Scheme})
			if err != nil {
				return response, err
			}
			session.view.Kind = "calibration"
			session.view.Evidence, err = json.Marshal(session.calibration.Evidence())
		} else {
			var data model.ManagementTrialBeginData
			if err := managementData(request.Data, &data); err != nil {
				return response, err
			}
			session.trial, err = round.diagnostic.PlanTrial(ctx, data.Requests)
			if err != nil {
				return response, err
			}
			session.view.Kind = "trial"
			session.view.Progress, err = json.Marshal(session.trial.Progress())
		}
		if err != nil {
			return response, err
		}
		m.mu.Lock()
		m.expireSessions()
		if len(m.sessions) >= 32 {
			m.mu.Unlock()
			return response, managementError("session_id", "管理会话已达32个上限，请停止或等待过期")
		}
		m.sessions[sessionID] = session
		view := session.view
		response.Session = &view
		m.mu.Unlock()
	case model.ManagementTrialStep, model.ManagementTrialStop:
		if len(request.Data) != 0 {
			return response, managementError("data", "试切步骤与停止不接受负载")
		}
		m.mu.Lock()
		m.expireSessions()
		session := m.sessions[request.SessionID]
		m.mu.Unlock()
		if session == nil || session.view.Generation != round.generation || session.trial == nil {
			return response, managementError("session_id", "试切会话不存在或已过期")
		}
		var progress diagnostics.TrialProgress
		var err error
		if request.Action == model.ManagementTrialStop {
			session.trial.Stop()
			progress = session.trial.Progress()
		} else {
			progress, err = session.trial.Step(ctx, request.Confirmed)
		}
		if err != nil {
			return response, err
		}
		view := session.view
		view.Progress, err = json.Marshal(progress)
		if err != nil {
			return response, err
		}
		response.Session = &view
	case model.ManagementPrepareConfig, model.ManagementPrepareRules, model.ManagementPrepareCalibration, model.ManagementPrepareSchedules:
		commit := &managedCommit{status: model.CommitStatus{ID: request.CommitID, Kind: request.Action, Phase: model.CommitAccepted, PreparedGeneration: round.generation, Saved: known(false), Effective: known(false)}, expires: time.Now().Add(managementLifetime), round: round}
		var err error
		switch request.Action {
		case model.ManagementPrepareConfig:
			commit.cfg, err = config.Load(bytes.NewReader(request.Data))
			if err == nil {
				err = automation.ValidateRules(commit.cfg, round.rules)
			}
		case model.ManagementPrepareRules:
			if m.settingsWriter == nil {
				return response, managementError("rules", "完整配置写者不可用，不能保存规则")
			}
			commit.rules, err = automation.LoadRules(bytes.NewReader(request.Data))
			if err == nil {
				err = automation.ValidateRules(round.cfg, commit.rules)
			}
		case model.ManagementPrepareCalibration:
			if !request.Confirmed || len(request.Data) != 0 {
				return response, managementError("confirmed", "校准保存必须明确确认且不能替换依据")
			}
			m.mu.Lock()
			m.expireSessions()
			session := m.sessions[request.SessionID]
			m.mu.Unlock()
			if session == nil || session.view.Generation != round.generation || session.calibration == nil {
				return response, managementError("session_id", "校准会话不存在或已过期")
			}
			commit.calibration = session.calibration
		case model.ManagementPrepareSchedules:
			if m.settingsWriter == nil {
				return response, managementError("schedules", "完整配置写者不可用，不能保存日程")
			}
			loaded, loadErr := scheduler.LoadSchedules(bytes.NewReader(request.Data))
			err = loadErr
			if err == nil {
				commit.schedules = append([]scheduler.Schedule{}, loaded...)
			}
		}
		if err != nil {
			return response, err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.applying || m.current != round {
			return response, managementError("expected_generation", "校验期间运行代已改变")
		}
		if _, exists := m.commits[request.CommitID]; exists {
			return response, managementError("commit_id", "提交ID已登记，重用被拒绝；请查询原ID")
		}
		if len(m.commits) >= 256 {
			return response, managementError("commit_id", "本进程提交记录已达256个上限")
		}
		m.commits[request.CommitID] = commit
		status := commit.status
		response.Commit = &status
	case model.ManagementPreviewRules, model.ManagementRuleRecords:
		m.mu.Lock()
		engine := round.engine
		m.mu.Unlock()
		if engine == nil {
			return response, model.NewError(model.CodeBackendUnavailable, "当前运行代没有真实规则引擎", nil)
		}
		var value any
		var err error
		if request.Action == model.ManagementPreviewRules {
			var data model.ManagementPreviewRulesData
			if err := managementData(request.Data, &data); err != nil {
				return response, err
			}
			value, err = engine.Preview(data.Event)
		} else {
			if len(request.Data) != 0 {
				return response, managementError("data", "记录查询不接受负载")
			}
			value = engine.Records()
		}
		if err != nil {
			return response, err
		}
		response.Data, err = json.Marshal(value)
		if err != nil {
			return response, err
		}
	default:
		return response, model.NewError(model.CodeUnsupportedOperation, "不支持的管理动作", nil)
	}
	return response, nil
}

// 调用方持管理锁；Stop只取消本会话，不回滚任何设备写入。
func (m *runtimeManager) expireSessions() {
	for id, session := range m.sessions {
		// 尚未结束的预览可能已经发写；过期清理不能抹掉恢复责任及查询依据。
		// worker先发布最终progress、runPreview随后才清active；中间即使已有
		// FinishedAt也仍由轮次持有，不能删除会话使最终落地丢失或留下失效active。
		active := m.current != nil && m.current.activePreview == id
		if session.preview != nil && (session.preview.FinishedAt == nil || active) {
			continue
		}
		if !time.Now().Before(session.view.ExpiresAt) {
			if session.trial != nil {
				session.trial.Stop()
			}
			delete(m.sessions, id)
		}
	}
}

func (m *runtimeManager) runPreview(round *runtimeRound, id string, request model.SwitchRequest) {
	defer round.previewWG.Done()
	status, err := round.queue.Preview(round.life, request, func(status model.InputPreviewStatus) {
		m.mu.Lock()
		if session := m.sessions[id]; session != nil {
			copy := status.Clone()
			session.preview = &copy
		}
		m.mu.Unlock()
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	if session := m.sessions[id]; session != nil {
		if err != nil {
			// 尚未入队的语法/映射拒绝也要终结已应答会话，避免永远停在queued。
			status = session.preview.Clone()
			status.State, status.Phase = "failed", "finished"
			if code := model.CodeOf(err); code == model.CodeCanceled || code == model.CodeTimeout {
				status.State = "cancelled"
			}
			now := time.Now()
			status.FinishedAt, status.HoldUntil, status.ActiveDisplayID = &now, nil, ""
			for i := range status.Displays {
				status.Displays[i].SwitchStatus, status.Displays[i].RestoreStatus, status.Displays[i].Detail = "skipped", "not_needed", "预览未执行："+err.Error()
			}
		}
		copy := status.Clone()
		session.preview = &copy
		session.view.ExpiresAt = time.Now().Add(managementLifetime)
	}
	if round.activePreview == id {
		round.activePreview = ""
	}
}

func (m *runtimeManager) expireCommit(commit *managedCommit) {
	if commit.status.Phase == model.CommitAccepted && (!time.Now().Before(commit.expires) || m.current == nil || commit.round != m.current) {
		commit.status.Phase = model.CommitFailed
		commit.status.Error = managementError("expected_generation", "准备提交已过期或所属运行代已退出，未保存未生效")
	}
}
