package app

import (
	"context"
	"errors"

	"github.com/orzlee/displayKvm/desktop/app/presentation"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/model"
)

func NewWithLanguage(connect Connector, language *i18n.Manager) *Service {
	if language == nil {
		var err error
		language, err = i18n.New("system", nil)
		if err != nil {
			// 固定内嵌词典损坏是程序构建错误；不能静默换成另一套硬编码消息。
			panic(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{connect: connect, language: language, ctx: ctx, cancel: cancel, changes: make(chan struct{}, 1),
		selection: Selection{Kind: "display"}, appearance: "system", connectionMessage: i18n.Message{Key: "app.connection.disconnected"}}
}

func (s *Service) Language() *i18n.Manager { return s.language }

// languageError保存本地门禁的语义。后台错误仍原样返回，不能因翻译破坏errors.Is/As。
// reason仅用于带显示器名称等上下文的嵌套诊断，读取快照时才生成其当前语言文字。
type languageError struct {
	message  i18n.Message
	language *i18n.Manager
	reason   error
}

func (e *languageError) Error() string {
	return renderMessage(e.language.Snapshot(), e.message, e.reason)
}

func (e *languageError) Unwrap() error { return e.reason }

func (s *Service) languageError(key string, values map[string]string) error {
	return &languageError{message: i18n.Message{Key: key, Values: values}, language: s.language}
}

func messageForError(err error) i18n.Message {
	if err == nil {
		return i18n.Message{}
	}
	var local *languageError
	if errors.As(err, &local) && local != nil {
		return local.message
	}
	var classified *model.Error
	if errors.As(err, &classified) && classified != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return presentation.MessageForError(err)
	}
	return i18n.Message{Key: "app.connection.failed"}
}

func renderError(catalog i18n.Catalog, err error) string {
	var local *languageError
	if errors.As(err, &local) && local != nil {
		return renderMessage(catalog, local.message, local.reason)
	}
	message := messageForError(err)
	return catalog.Text(message.Key, message.Values)
}

func renderMessage(catalog i18n.Catalog, message i18n.Message, reason error) string {
	name, hasName := message.Values["name"]
	if reason == nil && (!hasName || name != "") {
		return catalog.Text(message.Key, message.Values)
	}
	values := make(map[string]string, len(message.Values)+1)
	for key, value := range message.Values {
		values[key] = value
	}
	if hasName && name == "" {
		values["name"] = catalog.Text("app.display.unnamed", nil)
	}
	if reason != nil {
		values["reason"] = renderError(catalog, reason)
	}
	return catalog.Text(message.Key, values)
}

func diagnosticForError(err error) *presentation.Diagnostic {
	var local *languageError
	if errors.As(err, &local) && local != nil {
		return diagnosticForError(local.reason)
	}
	return presentation.DiagnosticFromError(err)
}

// switchResult只保存请求证据和消息语义，语言切换不重放动作、不更改逐屏状态。
type switchResult struct {
	ResultView
	label  i18n.Message
	detail i18n.Message
	reason error
	result *model.DisplayResult
}
