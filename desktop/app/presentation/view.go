// Package presentation定义纯Go桌面视图及逐屏证据投影，不依赖原生窗口。
package presentation

import (
	"errors"
	"time"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/model"
)

type Selection struct {
	Kind       string          `json:"kind"`
	DisplayID  model.DisplayID `json:"displayID"`
	GroupID    model.GroupID   `json:"groupID"`
	TargetID   model.TargetID  `json:"targetID"`
	Generation uint64          `json:"generation"`
}

type Option struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type DisplayView struct {
	ID         model.DisplayID `json:"id"`
	Name       string          `json:"name"`
	Available  bool            `json:"available"`
	Reason     string          `json:"reason"`
	Selected   bool            `json:"selected"`
	Diagnostic *Diagnostic     `json:"diagnostic,omitempty"`
}

type ResultView struct {
	DisplayID       model.DisplayID         `json:"displayID"`
	Name            string                  `json:"name"`
	State           string                  `json:"state"`
	Label           string                  `json:"label"`
	Detail          string                  `json:"detail"`
	Tone            string                  `json:"tone"`
	ManualConfirmed bool                    `json:"manualConfirmed"`
	SentAt          *time.Time              `json:"sentAt,omitempty"`
	Observation     *model.InputObservation `json:"observation,omitempty"`
	Diagnostic      *Diagnostic             `json:"diagnostic,omitempty"`
}

// Diagnostic保留原错误的结构字段与包装链文字。语言只改变提示，不能覆盖后台
// 的诊断证据，也不能把未知结果改写为未发送或已确认。
type Diagnostic struct {
	Code      model.ErrorCode `json:"code"`
	Message   string          `json:"message"`
	Field     string          `json:"field,omitempty"`
	Retryable bool            `json:"retryable,omitempty"`
	Detail    string          `json:"detail"`
}

func DiagnosticFromError(err error) *Diagnostic {
	if err == nil {
		return nil
	}
	diagnostic := &Diagnostic{Code: model.CodeOf(err), Message: err.Error(), Detail: err.Error()}
	var classified *model.Error
	if errors.As(err, &classified) && classified != nil {
		diagnostic.Message, diagnostic.Field, diagnostic.Retryable = classified.Message, classified.Field, classified.Retryable
	}
	return diagnostic
}

// MessageForError按协议代码选择消息键，不对后台原文做逐字替换。原文由Diagnostic
// 独立承载，词典缺键时仍显示可追踪的error.<code>，不会静默丢失错误。
func MessageForError(err error) i18n.Message {
	if err == nil {
		return i18n.Message{}
	}
	return i18n.Message{Key: "error." + string(model.CodeOf(err))}
}

type View struct {
	Generation           uint64          `json:"generation"`
	Revision             uint64          `json:"revision"`
	Connected            bool            `json:"connected"`
	Connecting           bool            `json:"connecting"`
	Busy                 bool            `json:"busy"`
	ConnectionMessage    string          `json:"connectionMessage"`
	Message              string          `json:"message"`
	Selection            Selection       `json:"selection"`
	CanSwitch            bool            `json:"canSwitch"`
	BlockedReason        string          `json:"blockedReason"`
	Displays             []DisplayView   `json:"displays"`
	Groups               []Option        `json:"groups"`
	Targets              []Option        `json:"targets"`
	Results              []ResultView    `json:"results"`
	RequestID            model.RequestID `json:"requestID"`
	AttemptTarget        string          `json:"attemptTarget"`
	Summary              string          `json:"summary"`
	Appearance           string          `json:"appearance"`
	TrayMessage          string          `json:"trayMessage"`
	UnconfiguredCount    int             `json:"unconfiguredCount"`
	ConnectionDiagnostic *Diagnostic     `json:"connectionDiagnostic,omitempty"`
	MessageDiagnostic    *Diagnostic     `json:"messageDiagnostic,omitempty"`
	BlockedDiagnostic    *Diagnostic     `json:"blockedDiagnostic,omitempty"`
}

// ApplyResult只接受已通过契约校验的结果；发送、读回、画面确认始终是独立证据。
func ApplyResult(row *ResultView, result model.DisplayResult) {
	// 旧调用入口保留中文结果契约；生产页面显式传共享Catalog，避免另作语言决策。
	manager, err := i18n.New("zh-CN", nil)
	if err != nil {
		panic(err)
	}
	ApplyResultWithCatalog(row, result, manager.Snapshot())
	if result.Error != nil {
		row.Detail = result.Error.Message
	}
}

func ApplyResultWithCatalog(row *ResultView, result model.DisplayResult, catalog i18n.Catalog) {
	row.State, row.SentAt, row.Observation = string(result.State), result.SentAt, result.Observation
	row.Diagnostic = nil
	if result.Error != nil {
		row.Diagnostic = DiagnosticFromError(result.Error)
	}
	if row.SentAt != nil && row.SentAt.IsZero() {
		row.SentAt = nil
	}
	// 未知/失败允许携带历史观测，但空值或零时间不构成观测证据，不能呈现成
	// “刚刚读取”。有效旧观测保留原时间，且始终不提升本次切换的结果状态。
	if row.Observation != nil && (row.Observation.Value == nil || row.Observation.ObservedAt.IsZero()) {
		row.Observation = nil
	}
	switch result.State {
	case model.StateAccepted:
		row.Label, row.Detail, row.Tone = catalog.Text("app.result.accepted", nil), catalog.Text("app.result.accepted_detail", nil), "notice"
	case model.StateSent:
		row.Label, row.Detail, row.Tone = catalog.Text("app.result.sent", nil), catalog.Text("app.result.sent_detail", nil), "notice"
	case model.StateConfirmed:
		row.Label, row.Detail, row.Tone = catalog.Text("app.result.confirmed", nil), catalog.Text("app.result.confirmed_detail", nil), "success"
	case model.StateUnknown:
		row.Label, row.Detail, row.Tone = catalog.Text("app.result.unknown", nil), catalog.Text("app.result.unknown_detail", nil), "notice"
	case model.StateFailed:
		row.Label, row.Detail, row.Tone = catalog.Text("app.result.failed", nil), catalog.Text("app.result.unknown_detail", nil), "error"
	}
	if result.Error != nil {
		message := MessageForError(result.Error)
		row.Detail = catalog.Text(message.Key, message.Values)
	}
}
