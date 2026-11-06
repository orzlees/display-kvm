package devices

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/identity"
	"github.com/orzlee/displayKvm/internal/model"
)

func (h *handler) beginTrial(r *http.Request) error {
	if r.PostForm.Get("confirmed") != "true" {
		return pageError("devices.error.trial_confirm", nil)
	}
	h.mu.Lock()
	s := &h.state
	id, generation, cfg := s.selected, s.generation, s.cfg
	devices := append([]model.Device(nil), s.devices...)
	blocked := s.hasOpenTrial()
	h.mu.Unlock()
	if blocked {
		return pageError("devices.error.trial_pending", nil)
	}
	resolved, err := identity.Resolve(cfg.Displays, devices)
	if err != nil {
		return err
	}
	known := false
	for _, resolution := range resolved {
		if resolution.DisplayID == id && resolution.Error == nil && resolution.Device != nil {
			known = true
		}
	}
	if !known {
		return pageError("devices.error.trial_identity", nil)
	}
	targetID, mapped := model.TargetID(r.PostForm.Get("target_id")), false
	for _, target := range cfg.Targets {
		if target.ID == targetID {
			for _, input := range target.Inputs {
				if input.DisplayID == id {
					mapped = true
				}
			}
		}
	}
	if !mapped {
		return pageError("devices.error.trial_mapping", nil)
	}
	requestID, err := freshID("trial-")
	if err != nil {
		return err
	}
	request := model.SwitchRequest{ID: model.RequestID(requestID), TargetID: targetID, DisplayIDs: []model.DisplayID{id}}
	response, err := h.manage(r.Context(), model.ManagementRequest{Action: model.ManagementTrialBegin, ExpectedGeneration: generation}, model.ManagementTrialBeginData{Requests: []model.SwitchRequest{request}})
	if err != nil {
		return err
	}
	if err := validSession(response.Session, generation); err != nil {
		return err
	}
	var progress diagnostics.TrialProgress
	if err := json.Unmarshal(response.Session.Progress, &progress); err != nil || progress.Total != 1 || progress.Next != 0 || len(progress.Results) != 0 || progress.Running || progress.Stopped {
		return pageError("devices.error.trial_plan", nil)
	}
	h.mu.Lock()
	h.state.trial, h.state.progress = response.Session, progress
	h.state.trialUncertain = false
	h.state.draft.TargetID = string(targetID)
	h.state.error = nil
	h.state.notice = i18n.Message{Key: "devices.notice.trial_prepared"}
	h.mu.Unlock()
	return nil
}

func (h *handler) trialAction(r *http.Request, stop bool) error {
	h.mu.Lock()
	session, generation := h.state.trial, h.state.generation
	if session == nil {
		h.mu.Unlock()
		return pageError("devices.error.trial_session", nil)
	}
	if !stop {
		if r.PostForm.Get("confirmed") != "true" || h.state.trialInFlight || h.state.trialUncertain || h.state.progress.Running || h.state.progress.Stopped || h.state.progress.Next >= h.state.progress.Total {
			h.mu.Unlock()
			return pageError("devices.error.trial_step_confirm", nil)
		}
		if err := validSession(session, generation); err != nil {
			h.mu.Unlock()
			return err
		}
		h.state.trialInFlight = true
	}
	h.mu.Unlock()
	action := model.ManagementTrialStep
	if stop {
		action = model.ManagementTrialStop
	}
	response, err := h.manage(r.Context(), model.ManagementRequest{Action: action, ExpectedGeneration: session.Generation, SessionID: session.ID, Confirmed: !stop}, nil)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !stop {
		h.state.trialInFlight = false
	}
	if err != nil {
		h.state.trialUncertain = true
		return pageError("devices.error.trial_unknown", err)
	}
	var progress diagnostics.TrialProgress
	if response.Session == nil || response.Session.ID != session.ID || json.Unmarshal(response.Session.Progress, &progress) != nil || progress.Total != h.state.progress.Total || progress.Next < 0 || progress.Next > progress.Total {
		h.state.trialUncertain = true
		return pageError("devices.error.trial_incomplete", nil)
	}
	for _, result := range progress.Results {
		if result.Validate() != nil {
			h.state.trialUncertain = true
			return pageError("devices.error.trial_evidence", nil)
		}
	}
	// Stop可能先于正在等待的Step返回。保留单调增加的已发结果和停止标记，
	// 防止较旧的停止快照覆盖随后回收的硬件证据，也不把Running=true说成硬件已结束。
	if progress.Next >= h.state.progress.Next && len(progress.Results) >= len(h.state.progress.Results) {
		progress.Stopped = progress.Stopped || h.state.progress.Stopped || stop
		h.state.progress = progress
	} else if stop {
		h.state.progress.Stopped = true
	}
	h.state.trial = response.Session
	h.state.trialUncertain = false
	h.state.error = nil
	if stop {
		h.state.notice = i18n.Message{Key: "devices.notice.trial_stopped"}
	} else {
		h.state.notice = i18n.Message{Key: "devices.notice.trial_returned"}
	}
	return nil
}

func (h *handler) confirmObservation(r *http.Request) error {
	if r.PostForm.Get("confirmed") != "true" {
		return pageError("devices.error.picture_confirm", nil)
	}
	index, err := strconv.Atoi(r.PostForm.Get("index"))
	if err != nil || index < 0 {
		return pageError("devices.error.picture_index", nil)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	current := 0
	for _, result := range h.state.progress.Results {
		for _, display := range result.Displays {
			if current == index {
				if r.PostForm.Get("request_id") != string(result.RequestID) || display.SentAt == nil || display.SentAt.IsZero() {
					return pageError("devices.error.picture_request", nil)
				}
				h.state.observed[string(result.RequestID)+"/"+string(display.DisplayID)] = true
				h.state.notice = i18n.Message{Key: "devices.notice.picture_recorded"}
				h.state.error = nil
				return nil
			}
			current++
		}
	}
	return pageError("devices.error.picture_missing", nil)
}
