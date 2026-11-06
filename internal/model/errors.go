package model

import (
	"context"
	"errors"
	"strings"
)

type ErrorCode string

const (
	CodeInvalidConfig        ErrorCode = "invalid_config"
	CodeInvalidRequest       ErrorCode = "invalid_request"
	CodeUnsupportedVersion   ErrorCode = "unsupported_version"
	CodeUnsupportedProtocol  ErrorCode = "unsupported_protocol"
	CodeIdentityAmbiguous    ErrorCode = "identity_ambiguous"
	CodeNotFound             ErrorCode = "not_found"
	CodeUnsupportedOperation ErrorCode = "unsupported_operation"
	CodePermissionDenied     ErrorCode = "permission_denied"
	CodeUnreachable          ErrorCode = "unreachable"
	CodeBackendUnavailable   ErrorCode = "backend_unavailable"
	CodeCanceled             ErrorCode = "canceled"
	CodeTimeout              ErrorCode = "timeout"
	CodeTransport            ErrorCode = "transport"
	CodeIO                   ErrorCode = "io"
	CodeAuthenticationFailed ErrorCode = "authentication_failed"
	CodeMessageTooLarge      ErrorCode = "message_too_large"
	CodeDaemonUnavailable    ErrorCode = "daemon_unavailable"
	CodeInternal             ErrorCode = "internal"
)

// Error是跨入口可序列化的错误。底层原因仅保留在进程内供errors.Is/As追踪，
// 不直接发送给客户端，避免平台路径、子进程输出或认证材料进入协议。
type Error struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Field     string    `json:"field,omitempty"`
	Retryable bool      `json:"retryable,omitempty"`
	cause     error
}

func NewError(code ErrorCode, message string, cause error) *Error {
	// 调用链可能把取消包装成传输失败；在构造和分类时保留取消/超时语义，
	// 防止调度器把已经取消的操作当成普通设备失败继续重试。
	if errors.Is(cause, context.Canceled) {
		code = CodeCanceled
	} else if errors.Is(cause, context.DeadlineExceeded) {
		code = CodeTimeout
	}
	return &Error{Code: code, Message: message, cause: cause}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return string(e.Code) + ": " + e.Message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func CodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return CodeCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CodeTimeout
	}
	var classified *Error
	if errors.As(err, &classified) && classified != nil {
		return classified.Code
	}
	return CodeInternal
}

func (e *Error) Validate() error {
	if e == nil || strings.TrimSpace(e.Message) == "" {
		return invalid("error.message", "错误必须说明原因")
	}
	switch e.Code {
	case CodeInvalidConfig, CodeInvalidRequest, CodeUnsupportedVersion, CodeUnsupportedProtocol,
		CodeIdentityAmbiguous, CodeNotFound, CodeUnsupportedOperation, CodePermissionDenied,
		CodeUnreachable, CodeBackendUnavailable, CodeCanceled, CodeTimeout, CodeTransport,
		CodeIO, CodeAuthenticationFailed, CodeMessageTooLarge, CodeDaemonUnavailable, CodeInternal:
		return nil
	default:
		return invalid("error.code", "未知错误类别")
	}
}

func invalid(field, message string) *Error {
	err := NewError(CodeInvalidRequest, message, nil)
	err.Field = field
	return err
}
