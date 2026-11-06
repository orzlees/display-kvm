package devices

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/model"
)

func (h *handler) load(ctx context.Context) error {
	snapshot, err := h.service.ManagementSnapshot(ctx)
	if err != nil {
		return err
	}
	cfg, err := config.Load(bytes.NewReader(snapshot.Configuration))
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state.generation != "" && h.state.generation != snapshot.Generation {
		// 后台重建后，枚举编号、诊断和后台会话均不可继续使用；草稿和提交ID保留，
		// 使未知保存仍可核对原提交，而不是根据新运行代重新提交旧操作。
		h.state.devices = nil
		h.state.diagnostic = nil
		h.state.calibration = nil
		if h.state.trial != nil {
			h.state.trialUncertain = true
		}
		h.state.notice = i18n.Message{Key: "devices.notice.generation_changed"}
	}
	h.state.generation, h.state.cfg = snapshot.Generation, cfg
	return nil
}

func (h *handler) choose(query url.Values) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &h.state
	if s.hasPendingCommit() && (query.Get("candidate") != "" || query.Get("display") != "" && query.Get("display") != string(s.selected)) {
		return pageError("devices.error.commit_pending", nil)
	}
	if s.hasOpenTrial() && (query.Get("candidate") != "" || query.Get("display") != "" && query.Get("display") != string(s.selected)) {
		return pageError("devices.error.trial_selection", nil)
	}
	if id := query.Get("display"); id != "" {
		if !configuredDisplay(s.cfg.Displays, model.DisplayID(id)) {
			return pageError("devices.error.display_missing", nil)
		}
		if s.selected != model.DisplayID(id) {
			h.selectLocked(model.DisplayID(id), "")
		}
	} else if id := query.Get("candidate"); id != "" {
		for _, candidate := range s.devices {
			if candidate.BackendID == id {
				if s.draft.BackendID == id && !configuredDisplay(s.cfg.Displays, s.selected) {
					return nil
				}
				newID, err := freshID("display-")
				if err != nil {
					return err
				}
				h.selectLocked(model.DisplayID(newID), id)
				s.draft.Name = candidate.Name
				return nil
			}
		}
		return pageError("devices.error.candidate_expired", nil)
	} else if s.selected == "" && len(s.cfg.Displays) > 0 {
		h.selectLocked(s.cfg.Displays[0].ID, "")
	}
	return nil
}

func (h *handler) selectLocked(id model.DisplayID, backendID string) {
	h.state.selected = id
	h.state.draft = draftView{DisplayID: string(id), BackendID: backendID, Protocol: string(model.ProtocolVCP)}
	h.state.diagnostic = nil
	h.state.calibration = nil
	for _, display := range h.state.cfg.Displays {
		if display.ID == id {
			h.state.draft.Name = display.Name
			break
		}
	}
}

func (h *handler) scan(ctx context.Context) error {
	h.mu.Lock()
	generation := h.state.generation
	h.mu.Unlock()
	response, err := h.manage(ctx, model.ManagementRequest{Action: model.ManagementDiagnose, ExpectedGeneration: generation}, model.ManagementDiagnoseData{})
	if err != nil {
		return err
	}
	var backend diagnostics.BackendDiagnostic
	if err := json.Unmarshal(response.Data, &backend); err != nil {
		return pageError("devices.error.scan_invalid", nil)
	}
	if !backend.Available {
		if backend.Issue != nil {
			return backend.Issue
		}
		return pageError("devices.error.scan_unavailable", nil)
	}
	h.mu.Lock()
	h.state.devices = append([]model.Device(nil), backend.Devices...)
	h.state.diagnostic = nil
	h.state.calibration = nil
	h.state.error = nil
	h.state.notice = i18n.Message{Key: "devices.notice.scanned", Values: map[string]string{"count": strconv.Itoa(len(backend.Devices))}}
	h.mu.Unlock()
	return nil
}

func (h *handler) inspect(ctx context.Context) error {
	h.mu.Lock()
	id, generation := h.state.selected, h.state.generation
	known := configuredDisplay(h.state.cfg.Displays, id)
	h.mu.Unlock()
	if !known {
		return pageError("devices.error.bind_before_inspect", nil)
	}
	response, err := h.manage(ctx, model.ManagementRequest{Action: model.ManagementDiagnose, ExpectedGeneration: generation}, model.ManagementDiagnoseData{DisplayID: id})
	if err != nil {
		return err
	}
	var diagnostic diagnostics.Diagnostic
	if err := json.Unmarshal(response.Data, &diagnostic); err != nil || diagnostic.Display.ID != id {
		return pageError("devices.error.diagnostic_mismatch", nil)
	}
	h.mu.Lock()
	h.state.diagnostic = &diagnostic
	h.state.error = nil
	h.state.notice = i18n.Message{Key: "devices.notice.inspected"}
	h.mu.Unlock()
	return nil
}

func (h *handler) manage(ctx context.Context, request model.ManagementRequest, data any) (model.ManagementResponse, error) {
	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			return model.ManagementResponse{}, err
		}
		request.Data = encoded
	}
	response, err := h.service.Management(ctx, request)
	if err == nil && request.ExpectedGeneration != "" && request.Action != model.ManagementApplyCommit && request.Action != model.ManagementCommitStatus && response.Generation != request.ExpectedGeneration {
		return model.ManagementResponse{}, pageError("devices.error.generation_changed", nil)
	}
	return response, err
}

func validSession(session *model.CalibrationSession, generation model.RuntimeGeneration) error {
	if session == nil || session.Generation != generation || !session.ExpiresAt.After(time.Now()) {
		return pageError("devices.error.session_expired", nil)
	}
	return nil
}
