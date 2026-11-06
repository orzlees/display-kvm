package devices

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/orzlee/displayKvm/desktop/i18n"
	coreclient "github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/identity"
	"github.com/orzlee/displayKvm/internal/model"
)

func (h *handler) action(r *http.Request) error {
	path := strings.TrimPrefix(r.URL.Path, "/devices/")
	if path == "reconnect" {
		return h.service.Reconnect(r.Context())
	}
	if path == "save/query" {
		return h.queryCommit(r)
	}
	if path == "trial/confirm" {
		return h.confirmObservation(r)
	}
	if path == "calibration/cancel" {
		h.mu.Lock()
		h.state.calibration = nil
		h.state.notice = i18n.Message{Key: "devices.notice.calibration_cancelled"}
		h.state.error = nil
		h.mu.Unlock()
		return nil
	}
	h.mu.Lock()
	generation := h.state.generation
	selected := h.state.selected
	pending := h.state.hasPendingCommit()
	trialActive := h.state.hasOpenTrial()
	h.mu.Unlock()
	if r.PostForm.Get("generation") != string(generation) || generation == "" {
		return pageError("devices.error.page_expired", nil)
	}
	if pending && path != "save/apply" && path != "trial/stop" {
		return pageError("devices.error.previous_commit", nil)
	}
	if trialActive && path != "trial/step" && path != "trial/stop" {
		return pageError("devices.error.trial_before_edit", nil)
	}
	if value := r.PostForm.Get("display_id"); value != "" && value != string(selected) {
		return pageError("devices.error.form_display_changed", nil)
	}
	switch path {
	case "scan":
		if err := h.load(r.Context()); err != nil {
			return err
		}
		h.mu.Lock()
		unchanged := h.state.generation == generation
		h.mu.Unlock()
		if !unchanged {
			return pageError("devices.error.scan_generation", nil)
		}
		return h.scan(r.Context())
	case "inspect":
		return h.inspect(r.Context())
	case "bind":
		return h.bind(r)
	case "calibration/prepare":
		return h.beginCalibration(r)
	case "calibration/save":
		if r.PostForm.Get("confirmed") != "true" {
			return pageError("devices.error.calibration_confirm", nil)
		}
		h.mu.Lock()
		session := h.state.calibration
		h.mu.Unlock()
		if err := validSession(session, generation); err != nil {
			return err
		}
		return h.prepareCommit(r.Context(), model.ManagementPrepareCalibration, session.ID, nil)
	case "save/apply":
		return h.applyCommit(r)
	case "trial/start":
		return h.beginTrial(r)
	case "trial/step", "trial/stop":
		return h.trialAction(r, path == "trial/stop")
	default:
		return pageError("devices.error.action_missing", nil)
	}
}

func (h *handler) bind(r *http.Request) error {
	if r.PostForm.Get("confirmed") != "true" {
		return pageError("devices.error.bind_confirm", nil)
	}
	name, backendID := strings.TrimSpace(r.PostForm.Get("name")), r.PostForm.Get("backend_id")
	if name == "" || len(name) > 256 || backendID == "" {
		return pageError("devices.error.bind_fields", nil)
	}
	h.mu.Lock()
	cfg := h.state.cfg
	cfg.Displays = append([]model.Display(nil), cfg.Displays...)
	devices := append([]model.Device(nil), h.state.devices...)
	id := h.state.selected
	h.state.draft.Name, h.state.draft.BackendID = name, backendID
	h.mu.Unlock()
	if !model.ValidID(string(id)) {
		return pageError("devices.error.bind_selection", nil)
	}
	if !configuredDisplay(cfg.Displays, id) {
		cfg.Displays = append(cfg.Displays, model.Display{ID: id, Name: name})
	}
	displays, err := identity.Bind(cfg.Displays, devices, id, backendID)
	if err != nil {
		return err
	}
	for i := range displays {
		if displays[i].ID == id {
			displays[i].Name = name
		}
	}
	cfg.Displays = displays
	if err := cfg.Validate(); err != nil {
		return err
	}
	return h.prepareCommit(r.Context(), model.ManagementPrepareConfig, "", cfg)
}

func (h *handler) beginCalibration(r *http.Request) error {
	h.mu.Lock()
	draft := h.state.draft
	draft.TargetID, draft.PortName = r.PostForm.Get("target_id"), strings.TrimSpace(r.PostForm.Get("port_name"))
	draft.Protocol, draft.Value = r.PostForm.Get("protocol"), strings.TrimSpace(r.PostForm.Get("value"))
	h.state.draft = draft
	h.state.calibration = nil
	diagnostic, generation, id := h.state.diagnostic, h.state.generation, h.state.selected
	targets := h.state.cfg.Targets
	h.mu.Unlock()
	if diagnostic == nil || diagnostic.Display.ID != id || !diagnostic.CanCalibrate || diagnostic.IdentityIssue != nil || diagnostic.BackendIssue != nil {
		return pageError("devices.error.calibration_identity", nil)
	}
	if draft.PortName == "" || len(draft.PortName) > 256 {
		return pageError("devices.error.port_name", nil)
	}
	if draft.TargetID == "" {
		newID, err := freshID("target-")
		if err != nil {
			return err
		}
		draft.TargetID = newID
	} else {
		found := false
		for _, target := range targets {
			if string(target.ID) == draft.TargetID {
				found = true
			}
		}
		if !found {
			return pageError("devices.error.target_missing", nil)
		}
	}
	// 新校准使用独立输入方案，避免人工编辑方案编号时误改其他目标共用的参数。
	// 逻辑编号由页面生成；页面只要求选择目标、命名并核对真实协议参数。
	schemeID, err := freshID("input-")
	if err != nil {
		return err
	}
	draft.SchemeID = schemeID
	h.mu.Lock()
	h.state.draft = draft
	h.mu.Unlock()
	base, raw := 10, draft.Value
	if strings.HasPrefix(strings.ToLower(raw), "0x") {
		base, raw = 16, raw[2:]
	}
	value, err := strconv.ParseUint(raw, base, 16)
	if err != nil {
		return pageError("devices.error.input_value", nil)
	}
	scheme := model.InputScheme{ID: model.SchemeID(draft.SchemeID), Protocol: model.InputProtocol(draft.Protocol), Value: uint16(value)}
	if scheme.Protocol == model.ProtocolVCP {
		scheme.VCPCode = 0x60
	}
	if err := scheme.Validate(); err != nil {
		return err
	}
	data := model.ManagementCalibrationBeginData{DisplayID: id, TargetID: model.TargetID(draft.TargetID), PortName: draft.PortName, Scheme: scheme}
	response, err := h.manage(r.Context(), model.ManagementRequest{Action: model.ManagementCalibrationBegin, ExpectedGeneration: generation}, data)
	if err != nil {
		return err
	}
	if err := validSession(response.Session, generation); err != nil {
		return err
	}
	var evidence diagnostics.CalibrationEvidence
	if err := json.Unmarshal(response.Session.Evidence, &evidence); err != nil || evidence.DisplayID != id || evidence.TargetID != data.TargetID || evidence.PortName != data.PortName || evidence.Scheme != scheme || evidence.Basis != diagnostics.BasisManual && evidence.Basis != diagnostics.BasisReadback {
		return pageError("devices.error.calibration_mismatch", nil)
	}
	h.mu.Lock()
	h.state.calibration = response.Session
	h.state.error = nil
	h.state.notice = i18n.Message{Key: "devices.notice.calibration_prepared"}
	h.mu.Unlock()
	return nil
}

func (h *handler) prepareCommit(ctx context.Context, action model.ManagementAction, sessionID string, data any) error {
	h.mu.Lock()
	generation := h.state.generation
	h.mu.Unlock()
	id, err := coreclient.NewCommitID(generation)
	if err != nil {
		return err
	}
	// 先记录ID再发Prepare。即使应答丢失，后台也可能已经接收；该页面只允许查询
	// 原编号，不能让重按按钮产生第二次保存。只有完整accepted应答才允许显式Apply。
	h.mu.Lock()
	h.state.commit = &model.CommitStatus{ID: id, Kind: action, Phase: model.CommitUnknown, PreparedGeneration: generation}
	h.state.commitKnown = false
	h.state.applyAttempted = false
	h.mu.Unlock()
	response, err := h.manage(ctx, model.ManagementRequest{Action: action, ExpectedGeneration: generation, CommitID: id, SessionID: sessionID, Confirmed: true}, data)
	if err != nil {
		return pageError("devices.error.prepare_unknown", err)
	}
	if response.Commit == nil || response.Commit.ID != id || response.Commit.Phase != model.CommitAccepted || response.Commit.PreparedGeneration != generation {
		return pageError("devices.error.prepare_missing", nil)
	}
	h.mu.Lock()
	h.state.commit, h.state.commitKnown = response.Commit, true
	h.state.error = nil
	h.state.notice = i18n.Message{Key: "devices.notice.commit_prepared"}
	h.mu.Unlock()
	return nil
}

func (h *handler) applyCommit(r *http.Request) error {
	h.mu.Lock()
	commit, known := h.state.commit, h.state.commitKnown
	generation := h.state.generation
	h.mu.Unlock()
	if r.PostForm.Get("confirmed") != "true" || commit == nil || !known || commit.Phase != model.CommitAccepted || commit.PreparedGeneration != generation || r.PostForm.Get("commit_id") != string(commit.ID) {
		return pageError("devices.error.apply_confirm", nil)
	}
	h.mu.Lock()
	h.state.commitKnown = false
	h.state.applyAttempted = true
	h.state.commit = &model.CommitStatus{ID: commit.ID, Kind: commit.Kind, Phase: model.CommitUnknown, PreparedGeneration: generation}
	h.mu.Unlock()
	response, err := h.manage(r.Context(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: generation, CommitID: commit.ID, Confirmed: true}, nil)
	if err != nil {
		return pageError("devices.error.apply_unknown", err)
	}
	if response.Commit == nil || response.Commit.ID != commit.ID {
		return pageError("devices.error.apply_missing", nil)
	}
	h.mu.Lock()
	h.state.commit = response.Commit
	h.state.notice = i18n.Message{Key: "devices.notice.applied"}
	h.state.error = nil
	h.mu.Unlock()
	return nil
}

func (h *handler) queryCommit(r *http.Request) error {
	h.mu.Lock()
	commit := h.state.commit
	h.mu.Unlock()
	if commit == nil || r.PostForm.Get("commit_id") != string(commit.ID) {
		return pageError("devices.error.query_id", nil)
	}
	response, err := h.manage(r.Context(), model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: commit.ID}, nil)
	if err != nil {
		return err
	}
	if response.Commit == nil || response.Commit.ID != commit.ID {
		return pageError("devices.error.query_missing", nil)
	}
	h.mu.Lock()
	h.state.commit = response.Commit
	// 查询可以恢复Prepare丢失的accepted应答，但Apply后绝不能重新开放Apply。
	h.state.commitKnown = response.Commit.Phase == model.CommitAccepted && !h.state.applyAttempted
	h.state.error = nil
	h.state.notice = i18n.Message{Key: "devices.notice.queried"}
	h.mu.Unlock()
	return nil
}
