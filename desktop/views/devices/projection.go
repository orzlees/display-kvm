package devices

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/orzlee/displayKvm/desktop/app/presentation"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/identity"
	"github.com/orzlee/displayKvm/internal/model"
)

func (h *handler) view() pageView {
	shared := h.service.Snapshot()
	catalog := h.service.Language().Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &h.state
	view := pageView{Catalog: catalog, Token: h.token, Generation: string(s.generation), Theme: shared.Appearance,
		Connected: shared.Connected, Connection: i18n.Message{Key: "app.connection.disconnected"}, ConnectionDiagnostic: shared.ConnectionDiagnostic, Busy: shared.Busy,
		Notice: s.notice, Draft: s.draft, HasSelection: s.selected != "",
		CalibrationHint: i18n.Message{Key: "devices.calibration_hint"}}
	if shared.Connecting {
		view.Connection.Key = "app.connection.connecting"
	} else if shared.Connected {
		view.Connection.Key = "app.connection.connected"
	} else if shared.ConnectionDiagnostic != nil {
		view.Connection.Key = "app.connection.failed"
	}
	if s.error != nil {
		var localized *localizedError
		if errors.As(s.error, &localized) {
			view.Error = i18n.Message{Key: localized.key}
			view.ErrorCause = presentation.MessageForError(localized.cause)
			view.ErrorDiagnostic = presentation.DiagnosticFromError(localized.cause)
		} else {
			view.Error = presentation.MessageForError(s.error)
			view.ErrorDiagnostic = presentation.DiagnosticFromError(s.error)
		}
	}
	if view.Theme == "" {
		view.Theme = "system"
	}
	pending := s.hasPendingCommit()
	trialActive := s.hasOpenTrial()
	ready := shared.Connected && !shared.Busy && s.generation != ""
	view.CanScan = ready && !pending && !trialActive
	view.CanBind = ready && !pending && !trialActive && len(s.devices) > 0
	resolved, resolveErr := identity.Resolve(s.cfg.Displays, s.devices)
	claimed := make(map[string]bool)
	for i, display := range s.cfg.Displays {
		row := displayView{ID: string(display.ID), URL: "/devices/?display=" + url.QueryEscape(string(display.ID)), Name: display.Name,
			LogicalID: string(display.ID), State: i18n.Message{Key: "devices.identity_unconfirmed"}, Selected: display.ID == s.selected}
		if row.Name == "" {
			row.Name = string(display.ID)
		}
		if resolveErr == nil && i < len(resolved) {
			resolution := resolved[i]
			if resolution.Error != nil {
				row.State = presentation.MessageForError(resolution.Error)
				row.Diagnostic = presentation.DiagnosticFromError(resolution.Error)
			} else if resolution.Device != nil {
				row.State.Key = "devices.identity_matched"
				claimed[resolution.Device.BackendID] = true
			}
		}
		if row.Selected {
			view.SelectedName = row.Name
			if resolveErr != nil {
				view.IdentityIssue = presentation.MessageForError(resolveErr)
				view.IdentityDiagnostic = presentation.DiagnosticFromError(resolveErr)
			} else if i < len(resolved) && resolved[i].Error != nil {
				view.IdentityIssue = presentation.MessageForError(resolved[i].Error)
				view.IdentityDiagnostic = presentation.DiagnosticFromError(resolved[i].Error)
			}
		}
		view.Displays = append(view.Displays, row)
	}
	for _, device := range s.devices {
		view.Candidates = append(view.Candidates, candidateView{ID: device.BackendID, Name: device.Name,
			Detail: device.Identity.Path, Selected: device.BackendID == s.draft.BackendID})
		if !claimed[device.BackendID] {
			selected := !configuredDisplay(s.cfg.Displays, s.selected) && s.draft.BackendID == device.BackendID
			view.Displays = append(view.Displays, displayView{ID: device.BackendID, URL: "/devices/?candidate=" + url.QueryEscape(device.BackendID),
				Name: device.Name, Detail: device.Identity.Path, State: i18n.Message{Key: "devices.scan_candidate"}, Selected: selected})
			if selected {
				view.SelectedName = device.Name
				view.IdentityIssue = i18n.Message{Key: "devices.candidate_identity"}
			}
		}
	}
	for _, target := range s.cfg.Targets {
		view.CalibrationTargets = append(view.CalibrationTargets, targetView{ID: string(target.ID), Name: target.Name, Selected: string(target.ID) == s.draft.TargetID})
		for _, input := range target.Inputs {
			if input.DisplayID == s.selected {
				view.Targets = append(view.Targets, targetView{ID: string(target.ID), Name: target.Name, Selected: string(target.ID) == s.draft.TargetID})
				break
			}
		}
	}
	for _, record := range s.cfg.Calibrations {
		if record.DisplayID == s.selected {
			confirmed := ""
			if record.ConfirmedAt != nil && !record.ConfirmedAt.IsZero() {
				confirmed = record.ConfirmedAt.Local().Format("2006-01-02 15:04:05")
			}
			view.Recorded = append(view.Recorded, recordedCalibrationView{Name: record.PortName, Target: string(record.TargetID),
				Input: fmt.Sprintf("%s / %d", record.Scheme.Protocol, record.Scheme.Value), Basis: evidenceText(diagnostics.EvidenceBasis(record.Basis)), Confirmed: confirmed})
		}
	}
	if d := s.diagnostic; d != nil && d.Display.ID == s.selected {
		view.CanCalibrate = ready && !pending && !trialActive && d.CanCalibrate && d.IdentityIssue == nil && d.BackendIssue == nil
		view.Diagnostic = &diagnosticView{Backend: i18n.Message{Key: "devices.backend_queue"}, Identity: i18n.Message{Key: "devices.identity_clear"},
			Capabilities: i18n.Message{Key: "devices.capabilities_empty"}, Readback: observationText(d.Observation), ReadbackBasis: evidenceText(d.InputConfidence)}
		var protocols []string
		for _, protocol := range d.Capabilities.Protocols {
			protocols = append(protocols, string(protocol))
		}
		if len(protocols) > 0 {
			view.Diagnostic.Protocols = strings.Join(protocols, ", ")
		}
		if d.IdentityIssue != nil {
			view.Diagnostic.Identity = presentation.MessageForError(d.IdentityIssue)
			view.IdentityIssue = presentation.MessageForError(d.IdentityIssue)
			view.IdentityDiagnostic = presentation.DiagnosticFromError(d.IdentityIssue)
		}
		for _, issue := range []*model.Error{d.BackendIssue, d.CapabilityIssue, d.ObservationIssue} {
			if issue != nil {
				view.Diagnostic.Issues = append(view.Diagnostic.Issues, presentation.DiagnosticFromError(issue))
			}
		}
		if d.Capabilities.UnavailableReason != "" {
			view.Diagnostic.UnavailableReason = d.Capabilities.UnavailableReason
		}
		if view.CanCalibrate {
			view.CalibrationHint.Key = "devices.calibration_ready"
		}
	}
	view.CanTrial = ready && !pending && view.IdentityIssue.Key == "" && len(view.Targets) > 0 && !s.trialInFlight && !s.trialUncertain && (s.trial == nil || s.progress.Stopped || s.progress.Next >= s.progress.Total)
	if s.calibration != nil {
		targetName := s.draft.PortName
		for _, target := range s.cfg.Targets {
			if string(target.ID) == s.draft.TargetID {
				targetName = target.Name
			}
		}
		view.Calibration = &calibrationView{Name: s.draft.PortName, Target: targetName, Protocol: s.draft.Protocol,
			Value: s.draft.Value, Basis: i18n.Message{Key: "devices.calibration_no_evidence"}, CanSave: ready && !pending && !trialActive && s.calibration.Generation == s.generation && s.calibration.ExpiresAt.After(time.Now())}
		var evidence diagnostics.CalibrationEvidence
		if json.Unmarshal(s.calibration.Evidence, &evidence) == nil {
			view.Calibration.Basis = evidenceText(evidence.Basis)
			if evidence.ObservationIssue != nil {
				view.Calibration.Diagnostic = presentation.DiagnosticFromError(evidence.ObservationIssue)
			}
		}
	}
	if s.trial != nil {
		progress := s.progress
		trial := &trialView{Running: progress.Running || s.trialInFlight, CanStop: progress.Running || s.trialInFlight || s.trialUncertain || !progress.Stopped && progress.Next < progress.Total,
			CanStep: ready && !pending && !s.trialInFlight && !s.trialUncertain && !progress.Stopped && progress.Next < progress.Total && s.trial.Generation == s.generation && s.trial.ExpiresAt.After(time.Now()),
			Summary: i18n.Message{Key: "devices.trial_progress", Values: map[string]string{"next": strconv.Itoa(progress.Next), "total": strconv.Itoa(progress.Total)}}}
		if progress.Stopped {
			trial.Summary = i18n.Message{Key: "devices.trial_stopped"}
		}
		if s.trialUncertain {
			trial.Summary = i18n.Message{Key: "devices.trial_unknown"}
		}
		for _, result := range progress.Results {
			for _, display := range result.Displays {
				row := presentation.ResultView{}
				presentation.ApplyResultWithCatalog(&row, display, catalog)
				key := string(result.RequestID) + "/" + string(display.DisplayID)
				observation := i18n.Message{Key: "devices.picture_unconfirmed"}
				if s.observed[key] {
					observation.Key = "devices.picture_confirmed"
				}
				sent := i18n.Message{Key: "devices.not_sent"}
				hasSent := display.SentAt != nil && !display.SentAt.IsZero()
				if hasSent {
					sent = i18n.Message{Key: "devices.sent_at", Values: map[string]string{"time": display.SentAt.Local().Format("15:04:05")}}
				}
				name := string(display.DisplayID)
				for _, configured := range s.cfg.Displays {
					if configured.ID == display.DisplayID && configured.Name != "" {
						name = configured.Name
					}
				}
				issue := i18n.Message{Key: "app.result." + row.State + "_detail"}
				if row.State == string(model.StateFailed) {
					issue.Key = "app.result.unknown_detail"
				}
				if row.Diagnostic != nil {
					issue = presentation.MessageForError(display.Error)
				}
				trial.Results = append(trial.Results, trialResultView{Index: len(trial.Results), RequestID: string(result.RequestID), Name: name, Status: i18n.Message{Key: "app.result." + row.State},
					Sent: sent, Readback: observationText(display.Observation), Observation: observation, Issue: issue, Diagnostic: row.Diagnostic, CanConfirm: hasSent && !s.observed[key]})
			}
		}
		view.Trial = trial
	}
	if s.commit != nil {
		status := i18n.Message{Key: "app.result.unknown"}
		switch s.commit.Phase {
		case model.CommitAccepted:
			status.Key = "devices.commit_accepted"
		case model.CommitSaving:
			status.Key = "devices.commit_saving"
		case model.CommitSaved:
			status.Key = "devices.commit_saved"
		case model.CommitApplying:
			status.Key = "devices.commit_applying"
		case model.CommitApplied:
			status.Key = "devices.commit_applied"
		case model.CommitFailed:
			status.Key = "devices.commit_failed"
		}
		view.Save = &saveView{CommitID: string(s.commit.ID), Status: status, Saved: boolEvidence(s.commit.Saved), Effective: boolEvidence(s.commit.Effective),
			CanApply: ready && s.commitKnown && s.commit.Phase == model.CommitAccepted && s.commit.PreparedGeneration == s.generation,
			CanQuery: shared.Connected}
		if s.commit.Error != nil {
			view.Save.Diagnostic = presentation.DiagnosticFromError(s.commit.Error)
		}
	}
	return view
}

func configuredDisplay(displays []model.Display, id model.DisplayID) bool {
	for _, display := range displays {
		if display.ID == id {
			return true
		}
	}
	return false
}

func observationText(observation *model.InputObservation) i18n.Message {
	if observation == nil || observation.Value == nil || observation.ObservedAt.IsZero() {
		return i18n.Message{Key: "devices.readback_unknown"}
	}
	return i18n.Message{Key: "devices.readback_value", Values: map[string]string{"value": strconv.FormatUint(uint64(*observation.Value), 10), "time": observation.ObservedAt.Local().Format("2006-01-02 15:04:05")}}
}

func evidenceText(basis diagnostics.EvidenceBasis) i18n.Message {
	switch basis {
	case diagnostics.BasisReadback:
		return i18n.Message{Key: "devices.evidence_readback"}
	case diagnostics.BasisManual:
		return i18n.Message{Key: "devices.evidence_manual"}
	default:
		return i18n.Message{Key: "devices.evidence_unknown"}
	}
}

func boolEvidence(value *bool) i18n.Message {
	if value == nil {
		return i18n.Message{Key: "common.unknown"}
	}
	if *value {
		return i18n.Message{Key: "common.confirmed"}
	}
	return i18n.Message{Key: "common.not_completed"}
}
