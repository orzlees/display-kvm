package schedules

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"time"

	"github.com/orzlee/displayKvm/desktop/app/presentation"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/localize"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/scheduler"
)

//go:embed schedules.html assets/*
var resources embed.FS
var pageTemplate = template.Must(template.New("schedules.html").Funcs(localize.Functions(i18n.Catalog{})).ParseFS(resources, "schedules.html"))

// 本页校验错误保存消息键，避免在操作时锁定语言；后台错误另保留原始诊断。
type messageError string

func (e messageError) Error() string { return string(e) }

// 解码失败既有本页上下文，也有严格解码器给出的具体原因。两者不能互相覆盖：
// 例如重复items在外层扫描时已被拒绝，丢掉cause后无法区分重复键与缺失状态。
type contextualError struct {
	messageError
	cause error
}

func (e contextualError) Error() string { return e.cause.Error() }
func (e contextualError) Unwrap() error { return e.cause }

type rowView struct {
	Index    int
	ID       string
	Time     string
	Timezone string
	Enabled  bool
	Selected bool
}
type statusView struct {
	ID       string
	Time     string
	Timezone string
	Next     string
	Instant  string
	Policy   string
	Enabled  bool
}
type pageView struct {
	Token            string
	Revision         uint64
	Theme            string
	Locale           string
	Ready            bool
	Writable         bool
	Editable         bool
	Dirty            bool
	CanCancel        bool
	CanReload        bool
	CanApply         bool
	Rows             []rowView
	Editor           *scheduler.Schedule
	Selected         int
	Statuses         []statusView
	ErrorKey         string
	ErrorSummary     string
	Diagnostic       *presentation.Diagnostic
	Notice           string
	TimerReason      string
	SkipGuaranteed   bool
	CommitID         string
	Phase            string
	Saved            string
	Effective        string
	CommitError      *model.Error
	CommitDiagnostic *presentation.Diagnostic
}

func (h *handler) render(w http.ResponseWriter, status int) {
	catalog := h.catalog()
	view := pageView{Token: h.token, Revision: h.revision, Theme: h.service.Snapshot().Appearance, Locale: catalog.Locale, Ready: h.loaded, Writable: h.writable, Editable: h.loaded && h.writable && h.commit == nil, Dirty: h.draft.dirty(), CanCancel: h.canCancel(), CanReload: h.canReload(), Selected: h.draft.selected, Notice: h.notice, TimerReason: h.timer.Reason, SkipGuaranteed: h.timer.SkipGuaranteed}
	if h.error != nil {
		var local messageError
		var contextual contextualError
		diagnosticError := h.error
		switch {
		case errors.As(h.error, &contextual):
			view.ErrorKey, diagnosticError = string(contextual.messageError), contextual.cause
		case errors.As(h.error, &local):
			view.ErrorKey = string(local)
			diagnosticError = nil
		}
		if diagnosticError != nil {
			message := presentation.MessageForError(diagnosticError)
			view.Diagnostic = presentation.DiagnosticFromError(diagnosticError)
			if view.ErrorKey == "" {
				view.ErrorKey = message.Key
			} else {
				view.ErrorSummary = message.Key
			}
		}
	}
	if view.Theme == "" {
		view.Theme = "system"
	}
	for i, item := range h.draft.items {
		view.Rows = append(view.Rows, rowView{Index: i, ID: item.ID, Time: item.Time, Timezone: item.Timezone, Enabled: item.Enabled, Selected: i == h.draft.selected})
	}
	if h.draft.selected >= 0 && h.draft.selected < len(h.draft.items) {
		item := h.draft.items[h.draft.selected]
		view.Editor = &item
	}
	for _, status := range h.statuses {
		row := statusView{ID: status.Schedule.ID, Time: status.Schedule.Time, Timezone: status.Schedule.Timezone, Enabled: status.Schedule.Enabled, Policy: status.MissedPolicy}
		if status.NextTrigger != nil {
			row.Instant = status.NextTrigger.Format(time.RFC3339Nano)
			row.Next = status.NextTrigger.Format("2006-01-02 15:04:05 -07:00")
		}
		view.Statuses = append(view.Statuses, row)
	}
	if h.commit != nil {
		view.CommitID = string(h.commit.ID)
		view.Phase = string(h.commit.Phase)
		view.CanApply = h.commit.Phase == "accepted" && !h.applySent
		view.Saved = "schedules.saved.pending"
		if h.commit.Saved != nil {
			if *h.commit.Saved {
				view.Saved = "targets.saved.yes"
			} else {
				view.Saved = "targets.saved.no"
			}
		}
		view.Effective = "schedules.effective.pending"
		if h.commit.Effective != nil {
			if *h.commit.Effective {
				view.Effective = "targets.effective.yes"
			} else {
				view.Effective = "targets.effective.no"
			}
		}
		if h.commit.Error != nil {
			view.CommitError = h.commit.Error
			view.CommitDiagnostic = presentation.DiagnosticFromError(h.commit.Error)
		}
	}
	var body bytes.Buffer
	if err := localize.Execute(pageTemplate, &body, view, catalog); err != nil {
		http.Error(w, catalog.Text("common.render_error", nil), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}
