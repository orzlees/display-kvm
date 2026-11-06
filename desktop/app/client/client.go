// Package client定义桌面公共客户端契约，不导入原生宿主或显示器后端。
package client

import (
	"context"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

type ConfigProvider interface{ Configuration() (config.Config, error) }

// Client只消费唯一后台客户端；Configuration必须来自该连接对应的后台快照。
// 测试替身实现同一接口，不能另建绕过配置检查、请求门禁或结果呈现的测试入口。
type Client interface {
	ConfigProvider
	Enumerate(context.Context) ([]model.Device, error)
	Switch(context.Context, model.SwitchRequest) (model.SwitchResult, error)
}

type Connector func() (Client, error)

// 管理是可探测能力；旧客户端替身和只读控制入口仍可实现原Client契约。
type ManagementClient interface {
	Management(context.Context, model.ManagementRequest) (model.ManagementResponse, error)
}
