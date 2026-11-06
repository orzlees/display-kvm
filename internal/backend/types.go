// Package backend定义唯一显示器后端边界，不选择或注册任何平台实现。
package backend

import (
	"context"

	"github.com/orzlee/displayKvm/internal/model"
)

// Backend隔离平台、协议和设备访问。所有阻塞调用必须响应ctx取消/截止时间，
// 每次调用拥有并释放本次取得的句柄、子进程和缓冲区；不得持有枚举时的裸句柄
// 等待未来切换。并发与逐屏重试由上层唯一调度者串行控制，后端不自建无限重试。
// 传输应由实现构造参数注入，因此非本平台宿主也能验证协议而不加载原生API。
type Backend interface {
	Name() string
	Enumerate(ctx context.Context) ([]model.Device, error)
	Capabilities(ctx context.Context, device model.Device) (model.DeviceCapabilities, error)
	ReadInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.InputObservation, error)
	SetInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error)
}

// ReadInput和SetInput的实现每次须在设备I/O前调用scheme.Validate；上层已经
// 校验配置也不能替代该边界校验。Enumerate和Capabilities不接收输入方案。
// 不支持协议/读回分别返回unsupported_protocol/unsupported_operation；许可不足或
// 传输未配置为backend_unavailable，权限不足为permission_denied，设备失联为unreachable。
// WriteReceipt仅在err为nil且SentAt非零时作为发送证据；失败的回执不能证明已发送，
// 也不能证明未发送。例如传输提交后等待完成时超时，设备仍可能已经接收命令。
// 错误须保留实际失败阶段与原因；上层无法证实尚未发送时，应保留原错误和已有观测，
// 使用unknown且不凭失败回执填充发送时间，不得据此盲目重试。只有确认未发送的
// 失败（如设备I/O前方案校验拒绝）才能据此认定本次未发送。
// SetInput成功仅表示传输提交；读回应由ReadInput独立完成，不承诺画面或多屏原子性。
