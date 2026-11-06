// Package events定义系统事件来源边界，不实现平台监听或规则调度。
package events

import (
	"context"

	"github.com/orzlee/displayKvm/internal/model"
)

// EventSource公开平台真实能力，不能把接口存在解释成支持任意系统事件。
// Run在调用者协程内阻塞运行，out由调用者拥有，来源不得关闭它；发送时必须同时
// 选择ctx.Done，避免消费者停止后资源无法释放。取消后返回可被errors.Is识别的
// context错误，并在返回前结束本次监听/子进程；不能遗留后台发送协程。
type EventSource interface {
	Name() string
	Capabilities(ctx context.Context) ([]model.EventCapability, error)
	Run(ctx context.Context, out chan<- model.Event) error
}
