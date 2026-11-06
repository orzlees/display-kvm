// Package ipc实现已批准的单请求回环协议及私有端点，不拥有显示器后端。
package ipc

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

func ReadFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, model.NewError(model.CodeTransport, "读取IPC帧头失败", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	// 长度来自未认证对端，必须在分配正文前检查；0也不是合法JSON帧。
	if size == 0 || size > model.MaxMessageBytes {
		return nil, model.NewError(model.CodeMessageTooLarge, "IPC帧长度必须为1至1048576字节", nil)
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, model.NewError(model.CodeTransport, "读取IPC帧正文失败", err)
	}
	return body, nil
}

func WriteFrame(writer io.Writer, body []byte) error {
	if len(body) == 0 || len(body) > model.MaxMessageBytes {
		return model.NewError(model.CodeMessageTooLarge, "IPC帧长度必须为1至1048576字节", nil)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	for _, part := range [][]byte{header[:], body} {
		for len(part) > 0 {
			written, err := writer.Write(part)
			if err != nil {
				return model.NewError(model.CodeTransport, "写入IPC帧失败", err)
			}
			if written <= 0 || written > len(part) {
				return model.NewError(model.CodeTransport, "写入IPC帧未取得进展", io.ErrShortWrite)
			}
			part = part[written:]
		}
	}
	return nil
}

// Deadline保持每个阶段的硬上限，不能因持续收到零碎字节而刷新期限。
func Deadline(ctx context.Context, maximum time.Duration) time.Time {
	deadline := time.Now().Add(maximum)
	if earlier, ok := ctx.Deadline(); ok && earlier.Before(deadline) {
		return earlier
	}
	return deadline
}

// PublicError仅传递契约错误；未知底层错误不向对端暴露路径、输出或认证材料。
func PublicError(err error) *model.Error {
	if err == nil {
		return nil
	}
	var classified *model.Error
	if errors.As(err, &classified) && classified != nil && classified.Validate() == nil {
		return &model.Error{Code: model.CodeOf(err), Message: classified.Message, Field: classified.Field, Retryable: classified.Retryable}
	}
	switch model.CodeOf(err) {
	case model.CodeCanceled:
		return model.NewError(model.CodeCanceled, "操作已取消", err)
	case model.CodeTimeout:
		return model.NewError(model.CodeTimeout, "操作已超时", err)
	default:
		return model.NewError(model.CodeInternal, "后台操作失败", err)
	}
}
