// Package app维护窗口和托盘共用的桌面状态，不导入原生宿主或显示器后端。
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"reflect"
	"sync"

	appclient "github.com/orzlee/displayKvm/desktop/app/client"
	"github.com/orzlee/displayKvm/desktop/app/presentation"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/identity"
	"github.com/orzlee/displayKvm/internal/model"
)

// 公共壳仅转出稳定类型，定义分别归客户端契约与呈现包，后续页面共享同一service。
type Client = appclient.Client
type Connector = appclient.Connector
type Selection = presentation.Selection
type Option = presentation.Option
type DisplayView = presentation.DisplayView
type ResultView = presentation.ResultView
type View = presentation.View

type Service struct {
	mu                   sync.Mutex
	connect              Connector
	language             *i18n.Manager
	client               Client
	cfg                  config.Config
	resolved             []identity.Resolution
	devices              []model.Device
	selection            Selection
	connected            bool
	connecting           bool
	busy                 bool
	closed               bool
	generation           uint64
	revision             uint64
	connectionMessage    i18n.Message
	connectionError      error
	message              i18n.Message
	messageError         error
	appearance           string
	results              []switchResult
	requestID            model.RequestID
	attemptTarget        string
	summary              i18n.Message
	ctx                  context.Context
	cancel               context.CancelFunc
	changes              chan struct{}
	managementGeneration model.RuntimeGeneration
	managementSnapshot   *model.ManagementSnapshot
	managementCommit     *model.CommitStatus
	managementAction     model.ManagementAction
	managementSession    string
}

func New(connect Connector) *Service {
	return NewWithLanguage(connect, nil)
}

// Changes合并连续呈现通知；订阅者收到通知后读取完整快照，不依赖每次中间事件。
// 通道不关闭，避免原生宿主退出时把零值通知误当成无限更新；生命周期由宿主取消。
func (s *Service) Changes() <-chan struct{} { return s.changes }

func (s *Service) changedLocked() {
	s.revision++
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	// 这里只释放GUI的等待和连接资源，不向后台发送退出命令，也不承诺撤销已发送项。
	// 后台可能已经提交设备写入；重新打开窗口不得根据本地取消状态重放该操作。
	s.cancel()
}

func (s *Service) Reconnect(ctx context.Context) error { return s.load(ctx, true) }
func (s *Service) Refresh(ctx context.Context) error   { return s.load(ctx, false) }

func (s *Service) load(ctx context.Context, reconnect bool) error {
	s.mu.Lock()
	if s.closed || s.busy || s.connecting {
		s.mu.Unlock()
		return s.languageError("app.wait.operation", nil)
	}
	current := s.client
	s.connecting, s.connected = true, false
	s.generation++
	s.managementGeneration = ""
	s.managementSnapshot = nil
	s.selection = Selection{Kind: "display", Generation: s.generation}
	s.connectionMessage, s.message = i18n.Message{Key: "app.connection.connecting"}, i18n.Message{Key: "app.load.reading"}
	s.connectionError, s.messageError = nil, nil
	s.changedLocked()
	s.mu.Unlock()

	bounded, cancel := context.WithTimeout(ctx, model.DefaultRequestTimeout)
	stop := context.AfterFunc(s.ctx, cancel)
	defer cancel()
	defer stop()
	var err error
	if reconnect || current == nil {
		if s.connect == nil {
			err = s.languageError("app.connection.failed", nil)
		} else {
			current, err = s.connect()
		}
	}
	var cfg config.Config
	var devices []model.Device
	var resolved []identity.Resolution
	if err == nil && current == nil {
		err = s.languageError("app.connection.failed", nil)
	}
	if err == nil {
		cfg, err = current.Configuration()
	}
	if err == nil {
		err = cfg.Validate()
	}
	if err == nil {
		devices, err = current.Enumerate(bounded)
	}
	if err == nil {
		// 枚举前后均核对同一连接的快照，避免后台恰在枚举期间重启后，把旧配置与
		// 新设备拼成可操作列表。失败必须保留断开状态，不能回退原文件或空配置。
		var after config.Config
		after, err = current.Configuration()
		if err == nil && !reflect.DeepEqual(cfg, after) {
			err = s.languageError("app.connection.config_changed", nil)
		}
	}
	if err == nil {
		resolved, err = identity.Resolve(cfg.Displays, devices)
	}
	if err == nil {
		err = bounded.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connecting = false
	if s.closed {
		return s.languageError("app.closed", nil)
	}
	if err != nil {
		s.connectionMessage, s.connectionError = messageForError(err), err
		s.message, s.messageError = i18n.Message{Key: "app.load.failed"}, nil
		s.changedLocked()
		return err
	}
	s.client, s.cfg, s.devices, s.resolved = current, cfg, devices, resolved
	s.connected = true
	s.connectionMessage, s.message = i18n.Message{Key: "app.connection.connected"}, i18n.Message{Key: "app.load.complete"}
	s.changedLocked()
	return nil
}

func (s *Service) SetSelection(choice Selection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy || s.connecting {
		return s.languageError("app.wait.operation", nil)
	}
	if err := s.validateChoiceLocked(choice); err != nil {
		return err
	}
	s.selection, s.message, s.messageError = choice, i18n.Message{}, nil
	s.changedLocked()
	return nil
}

func (s *Service) validateChoiceLocked(choice Selection) error {
	// Generation绑定本次目录，而非持久化ID。旧托盘菜单可能在重连后仍派发点击，
	// 即使同名ID仍存在，也不能把旧意图套用到新快照或新映射，必须重新选择。
	if choice.Generation != s.generation {
		return s.languageError("app.selection.stale", nil)
	}
	if choice.Kind != "display" && choice.Kind != "group" {
		return s.languageError("app.selection.kind", nil)
	}
	if choice.Kind == "display" && choice.GroupID != "" || choice.Kind == "group" && choice.DisplayID != "" {
		return s.languageError("app.selection.mixed", nil)
	}
	if choice.DisplayID != "" {
		found := false
		for _, d := range s.cfg.Displays {
			if d.ID == choice.DisplayID {
				found = true
			}
		}
		if !found {
			return s.languageError("app.selection.display_missing", nil)
		}
	}
	if choice.GroupID != "" {
		found := false
		for _, g := range s.cfg.Groups {
			if g.ID == choice.GroupID {
				found = true
			}
		}
		if !found {
			return s.languageError("app.selection.group_missing", nil)
		}
	}
	if choice.TargetID != "" {
		found := false
		for _, target := range s.cfg.Targets {
			if target.ID == choice.TargetID {
				found = true
			}
		}
		if !found {
			return s.languageError("app.selection.target_missing", nil)
		}
	}
	return nil
}

func (s *Service) selectedIDsLocked(choice Selection) []model.DisplayID {
	if choice.Kind == "display" && choice.DisplayID != "" {
		return []model.DisplayID{choice.DisplayID}
	}
	if choice.Kind == "group" {
		for _, group := range s.cfg.Groups {
			if group.ID == choice.GroupID {
				return group.DisplayIDs
			}
		}
	}
	return nil
}

func (s *Service) blockedLocked(choice Selection) error {
	if s.closed {
		return s.languageError("app.closed", nil)
	}
	if s.connecting {
		return s.languageError("app.wait.refresh", nil)
	}
	if s.busy {
		return s.languageError("app.wait.switch", nil)
	}
	if !s.connected {
		return s.languageError("app.wait.connect", nil)
	}
	if err := s.validateChoiceLocked(choice); err != nil {
		return err
	}
	ids := s.selectedIDsLocked(choice)
	if len(ids) == 0 {
		return s.languageError("app.selection.scope", nil)
	}
	if choice.TargetID == "" {
		return s.languageError("app.selection.target", nil)
	}
	var target model.Target
	for _, item := range s.cfg.Targets {
		if item.ID == choice.TargetID {
			target = item
		}
	}
	for _, id := range ids {
		name := ""
		for _, d := range s.cfg.Displays {
			if d.ID == id {
				name = d.Name
			}
		}
		mapped := false
		for _, input := range target.Inputs {
			if input.DisplayID == id {
				mapped = true
			}
		}
		if !mapped {
			return s.languageError("app.display.mapping", map[string]string{"name": name})
		}
		available := false
		for _, resolution := range s.resolved {
			if resolution.DisplayID != id {
				continue
			}
			if resolution.Error != nil {
				return &languageError{message: i18n.Message{Key: "app.display.error", Values: map[string]string{"name": name}}, language: s.language, reason: resolution.Error}
			}
			available = resolution.Device != nil
		}
		if !available {
			return s.languageError("app.display.unavailable", map[string]string{"name": name})
		}
	}
	return nil
}

func (s *Service) Preview(choice Selection) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return renderError(s.language.Snapshot(), s.blockedLocked(choice))
}

func (s *Service) BeginSwitch(choice Selection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reason := s.blockedLocked(choice); reason != nil {
		s.message, s.messageError = messageForError(reason), reason
		s.changedLocked()
		return reason
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return model.NewError(model.CodeInternal, s.language.Snapshot().Text("app.switch.start_failed", nil), err)
	}
	request := model.SwitchRequest{ID: model.RequestID("desktop-" + hex.EncodeToString(random[:])), TargetID: choice.TargetID, GroupID: choice.GroupID}
	if choice.Kind == "display" {
		request.DisplayIDs = []model.DisplayID{choice.DisplayID}
	}
	if err := request.Validate(); err != nil {
		return err
	}
	s.selection, s.requestID, s.busy = choice, request.ID, true
	s.results = nil
	for _, id := range s.selectedIDsLocked(choice) {
		for _, d := range s.cfg.Displays {
			if d.ID == id {
				s.results = append(s.results, switchResult{ResultView: ResultView{DisplayID: id, Name: d.Name, State: "waiting", Tone: "notice"}, label: i18n.Message{Key: "app.result.waiting"}, detail: i18n.Message{Key: "app.result.waiting_detail"}})
			}
		}
	}
	for _, target := range s.cfg.Targets {
		if target.ID == choice.TargetID {
			s.attemptTarget = target.Name
		}
	}
	s.summary, s.message, s.messageError = i18n.Message{Key: "app.result.waiting"}, i18n.Message{Key: "app.switch.multi"}, nil
	s.changedLocked()
	// HTTP请求只负责启动本次动作，等待归GUI生命周期所有。关闭页面、读取超时
	// 或托盘重复点击都不能创建第二个请求；后台是否已写入只由返回证据决定。
	go s.runSwitch(s.client, s.cfg, request)
	return nil
}

func (s *Service) runSwitch(client Client, expected config.Config, request model.SwitchRequest) {
	ctx, cancel := context.WithTimeout(s.ctx, model.ResponseTimeout)
	defer cancel()
	current, err := client.Configuration()
	if err == nil && !reflect.DeepEqual(current, expected) {
		err = s.languageError("app.connection.config_changed", nil)
	}
	beforeSend := err != nil
	var result model.SwitchResult
	if err == nil {
		result, err = client.Switch(ctx, request)
	}
	if err != nil {
		log.Printf("取得桌面切换结果失败（请求 %s）：%v", request.ID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.requestID != request.ID {
		return
	}
	s.busy = false
	if beforeSend {
		s.connected = false
		s.connectionMessage, s.connectionError = i18n.Message{Key: "app.connection.config_unavailable"}, err
		for i := range s.results {
			s.results[i].State, s.results[i].Tone = "failed", "error"
			s.results[i].label, s.results[i].detail, s.results[i].reason = i18n.Message{Key: "app.result.failed"}, i18n.Message{Key: "app.switch.not_sent"}, err
		}
		s.summary, s.message, s.messageError = i18n.Message{Key: "app.summary.failed"}, i18n.Message{Key: "app.switch.reconnect"}, nil
		s.changedLocked()
		return
	}
	// 不完整、错误关联或格式无效的响应不能证明某屏成功。传输失败也不证明
	// “尚未发送”；保留有效逐屏证据，缺少证据的屏明确未知，永不自动重试。
	valid := result.RequestID == request.ID && result.Validate() == nil
	if valid {
		for _, item := range result.Displays {
			selected := false
			for _, row := range s.results {
				if row.DisplayID == item.DisplayID {
					selected = true
				}
			}
			if !selected {
				valid = false
			}
		}
	}
	for i := range s.results {
		row := &s.results[i]
		row.State, row.Tone = "unknown", "notice"
		row.label, row.detail = i18n.Message{Key: "app.result.unknown"}, i18n.Message{Key: "app.result.unknown_detail"}
		if valid {
			for _, item := range result.Displays {
				if item.DisplayID == row.DisplayID {
					value := item
					row.State, row.result = string(item.State), &value
				}
			}
		}
	}
	if err != nil || !valid {
		s.connected = false
		s.connectionMessage, s.connectionError = i18n.Message{Key: "app.connection.incomplete"}, err
	}
	failed, uncertain, confirmed := 0, 0, 0
	for _, row := range s.results {
		switch row.State {
		case "failed":
			failed++
		case "unknown", "accepted":
			uncertain++
		case "confirmed":
			confirmed++
		}
	}
	switch {
	case failed == len(s.results):
		s.summary = i18n.Message{Key: "app.summary.failed"}
	case uncertain == len(s.results):
		s.summary = i18n.Message{Key: "app.summary.unknown"}
	case failed+uncertain > 0:
		s.summary = i18n.Message{Key: "app.summary.partial"}
	case confirmed == len(s.results):
		s.summary = i18n.Message{Key: "app.summary.confirmed"}
	default:
		s.summary = i18n.Message{Key: "app.summary.sent"}
	}
	s.message, s.messageError = i18n.Message{Key: "app.switch.readback_notice"}, nil
	if result.Error != nil {
		s.message, s.messageError = messageForError(result.Error), result.Error
	}
	s.changedLocked()
}

func (s *Service) Confirm(requestID model.RequestID, displayID model.DisplayID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy || requestID != s.requestID {
		return s.languageError("app.confirm.stale", nil)
	}
	for i := range s.results {
		if s.results[i].DisplayID == displayID {
			// 人工看到画面是独立证据，仅属于本次请求；不改后台state或读回时间，
			// 不写配置、不发送事件，下次切换会创建全新的逐屏结果和确认标记。
			s.results[i].ManualConfirmed = true
			s.message, s.messageError = i18n.Message{Key: "app.confirm.recorded"}, nil
			s.changedLocked()
			return nil
		}
	}
	return s.languageError("app.confirm.missing", nil)
}

func (s *Service) SetAppearance(mode string) error {
	if mode != "system" && mode != "dark" && mode != "light" {
		return s.languageError("app.appearance.invalid", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appearance = mode
	s.changedLocked()
	return nil
}

func (s *Service) Snapshot() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 同一快照只读取一次语言，避免并发切换让标题、门禁和逐屏结果混用两种语言。
	catalog := s.language.Snapshot()
	v := View{Generation: s.generation, Revision: s.revision, Connected: s.connected, Connecting: s.connecting, Busy: s.busy,
		ConnectionMessage: catalog.Text(s.connectionMessage.Key, s.connectionMessage.Values), Message: catalog.Text(s.message.Key, s.message.Values), Selection: s.selection, RequestID: s.requestID,
		AttemptTarget: s.attemptTarget, Summary: catalog.Text(s.summary.Key, s.summary.Values), Appearance: s.appearance,
		TrayMessage:          catalog.Text("app.tray.notice", nil),
		ConnectionDiagnostic: diagnosticForError(s.connectionError), MessageDiagnostic: diagnosticForError(s.messageError),
		Displays: []DisplayView{}, Groups: []Option{}, Targets: []Option{}, Results: []ResultView{}}
	if _, local := s.messageError.(*languageError); local {
		v.Message = renderError(catalog, s.messageError)
	}
	if s.requestID != "" && s.attemptTarget == "" {
		v.AttemptTarget = catalog.Text("app.target.unnamed", nil)
	}
	blocked := s.blockedLocked(s.selection)
	v.BlockedReason, v.BlockedDiagnostic = renderError(catalog, blocked), diagnosticForError(blocked)
	v.CanSwitch = v.BlockedReason == ""
	selected := s.selectedIDsLocked(s.selection)
	matched := make(map[string]bool)
	for _, display := range s.cfg.Displays {
		row := DisplayView{ID: display.ID, Name: named(display.Name, catalog.Text("app.display.unnamed", nil)), Reason: catalog.Text("app.display.unread", nil)}
		for _, id := range selected {
			if id == display.ID {
				row.Selected = true
			}
		}
		for _, resolution := range s.resolved {
			if resolution.DisplayID != display.ID {
				continue
			}
			if resolution.Error != nil {
				row.Reason = renderError(catalog, resolution.Error)
				row.Diagnostic = diagnosticForError(resolution.Error)
			} else if resolution.Device != nil {
				row.Available, row.Reason = true, catalog.Text("app.display.matched", nil)
				matched[resolution.Device.BackendID] = true
			}
		}
		if !s.connected {
			row.Available, row.Reason = false, catalog.Text("app.display.disconnected", nil)
		}
		v.Displays = append(v.Displays, row)
	}
	for _, device := range s.devices {
		if !matched[device.BackendID] {
			v.UnconfiguredCount++
		}
	}
	for _, group := range s.cfg.Groups {
		v.Groups = append(v.Groups, Option{ID: string(group.ID), Name: named(group.Name, catalog.Text("app.group.unnamed", nil))})
	}
	for _, target := range s.cfg.Targets {
		v.Targets = append(v.Targets, Option{ID: string(target.ID), Name: named(target.Name, catalog.Text("app.target.unnamed", nil))})
	}
	for _, state := range s.results {
		row := state.ResultView
		row.Name = named(row.Name, catalog.Text("app.display.unnamed", nil))
		row.Label, row.Detail = catalog.Text(state.label.Key, state.label.Values), renderMessage(catalog, state.detail, state.reason)
		row.Diagnostic = diagnosticForError(state.reason)
		if state.result != nil {
			presentation.ApplyResultWithCatalog(&row, *state.result, catalog)
		}
		// 快照必须拥有自己的观测指针，防止呈现调用者或测试替身改写历史证据。
		if row.SentAt != nil {
			value := *row.SentAt
			row.SentAt = &value
		}
		if row.Observation != nil {
			observation := *row.Observation
			if observation.Value != nil {
				value := *observation.Value
				observation.Value = &value
			}
			row.Observation = &observation
		}
		v.Results = append(v.Results, row)
	}
	return v
}

func named(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}
