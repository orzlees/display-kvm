// Package client供CLI与桌面共用四操作协议；不导入GUI或任何显示器后端。
package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/ipc"
	"github.com/orzlee/displayKvm/internal/model"
)

type DialFunc func(context.Context, string, string) (net.Conn, error)

type Client struct {
	endpoint        model.Endpoint
	dial            DialFunc
	configDirectory string
}

// Connect只读取私有发现文件，不自动拉起后台；空目录使用os.UserConfigDir。
func Connect(configDirectory string) (*Client, error) {
	if configDirectory == "" {
		var err error
		configDirectory, err = os.UserConfigDir()
		if err != nil || configDirectory == "" {
			return nil, model.NewError(model.CodePermissionDenied, "无法确定当前用户配置目录", err)
		}
	}
	endpoint, err := ipc.LoadEndpoint(configDirectory)
	if err != nil {
		if model.CodeOf(err) == model.CodePermissionDenied || model.CodeOf(err) == model.CodeUnsupportedVersion {
			return nil, err
		}
		return nil, unavailable(err)
	}
	client, err := New(endpoint)
	if err != nil {
		return nil, err
	}
	client.configDirectory = configDirectory
	return client, nil
}

// Configuration只读取Connect发现的同一后台固定快照；每次解码得到独立值，
// 调用者修改分组或目标不会影响后台。New/NewWithTransport没有私有发现目录，
// 因而不能猜测系统目录、重读原配置或用空配置伪造可选项目。
func (c *Client) Configuration() (config.Config, error) {
	if c.configDirectory == "" {
		return config.Config{}, model.NewError(model.CodeInvalidRequest, "读取配置快照必须先使用client.Connect完成私有发现", nil)
	}
	return ipc.LoadConfiguration(c.configDirectory, c.endpoint)
}

// New供已完成私有发现的嵌入调用者使用；Endpoint.Validate仅检查格式，调用者仍须
// 通过ipc.LoadEndpoint保证磁盘发现文件私有，不能从不可信位置取得认证材料。
func New(endpoint model.Endpoint) (*Client, error) {
	dialer := &net.Dialer{Timeout: model.ConnectTimeout}
	return NewWithTransport(endpoint, dialer.DialContext)
}

func NewWithTransport(endpoint model.Endpoint, dial DialFunc) (*Client, error) {
	if err := endpoint.Validate(); err != nil {
		return nil, err
	}
	if dial == nil {
		return nil, model.NewError(model.CodeInvalidRequest, "客户端连接函数不能为空", nil)
	}
	return &Client{endpoint: endpoint, dial: dial}, nil
}

func (c *Client) Switch(ctx context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
	response, err := c.call(ctx, model.Request{ID: request.ID, Operation: model.OperationSwitch, Switch: &request})
	if err != nil {
		return model.SwitchResult{}, err
	}
	return *response.SwitchResult, nil
}

func (c *Client) Enumerate(ctx context.Context) ([]model.Device, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	response, err := c.call(ctx, model.Request{ID: id, Operation: model.OperationEnumerate})
	return response.Devices, err
}

func (c *Client) EventCapabilities(ctx context.Context) ([]model.EventCapability, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	response, err := c.call(ctx, model.Request{ID: id, Operation: model.OperationEventCapabilities})
	return response.EventCapabilities, err
}

func (c *Client) EmitEvent(ctx context.Context, event model.Event) error {
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = c.call(ctx, model.Request{ID: id, Operation: model.OperationEmitEvent, Event: &event})
	return err
}

// 先用旧协议也能严格解码的无负载操作探测。旧后台可明确拒绝，不能把未知字段
// 导致的断连解释为支持。两次调用均不自动重试，尤其Apply结果未知时只能查询ID。
func (c *Client) Management(ctx context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
	if err := request.Validate(); err != nil {
		return model.ManagementResponse{}, err
	}
	id, err := newID()
	if err != nil {
		return model.ManagementResponse{}, err
	}
	if _, err = c.call(ctx, model.Request{ID: id, Operation: model.OperationManagementCapabilities}); err != nil {
		return model.ManagementResponse{}, err
	}
	id, err = newID()
	if err != nil {
		return model.ManagementResponse{}, err
	}
	response, err := c.call(ctx, model.Request{ID: id, Operation: model.OperationManagement, Management: &request})
	if err != nil {
		return model.ManagementResponse{}, err
	}
	return *response.Management, nil
}

func NewCommitID(generation model.RuntimeGeneration) (model.RequestID, error) {
	if !model.ValidID(string(generation)) {
		return "", model.NewError(model.CodeInvalidRequest, "提交ID需要有效运行代", nil)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", model.NewError(model.CodeInternal, "无法生成提交ID", err)
	}
	id := model.RequestID("commit-" + string(generation) + "-" + hex.EncodeToString(random[:]))
	if !model.ValidID(string(id)) {
		return "", model.NewError(model.CodeInvalidRequest, "运行代过长，无法生成提交ID", nil)
	}
	return id, nil
}

func (c *Client) call(ctx context.Context, request model.Request) (model.Response, error) {
	request.Version, request.Token = model.ProtocolVersion, c.endpoint.Token
	if err := request.Validate(); err != nil {
		return model.Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Response{}, ipc.PublicError(err)
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > model.MaxMessageBytes {
		return model.Response{}, model.NewError(model.CodeMessageTooLarge, "请求超过协议大小限制", err)
	}
	connect, cancelConnect := context.WithTimeout(ctx, model.ConnectTimeout)
	conn, err := c.dial(connect, c.endpoint.Network, c.endpoint.Address)
	cancelConnect()
	if err != nil {
		if ctx.Err() != nil {
			return model.Response{}, ipc.PublicError(ctx.Err())
		}
		return model.Response{}, unavailable(err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := conn.SetWriteDeadline(ipc.Deadline(ctx, model.ResponseTimeout)); err != nil {
		return model.Response{}, ipc.TransportError(ctx, err)
	}
	if err := ipc.WriteFrame(conn, data); err != nil {
		return model.Response{}, ipc.TransportError(ctx, err)
	}
	if err := conn.SetReadDeadline(ipc.Deadline(ctx, model.ResponseTimeout)); err != nil {
		return model.Response{}, ipc.TransportError(ctx, err)
	}
	data, err = ipc.ReadFrame(conn)
	if err != nil {
		return model.Response{}, ipc.TransportError(ctx, err)
	}
	response, err := model.DecodeResponse(data)
	if err != nil {
		return model.Response{}, err
	}
	if response.ID != request.ID || response.Operation != request.Operation {
		return model.Response{}, model.NewError(model.CodeInvalidRequest, "后台响应关联ID或操作不一致", nil)
	}
	if response.Error != nil {
		return response, response.Error
	}
	// 成功收到受理结果后不再用ctx.Err替换它；逐屏失败与部分发送是调用者必须保留
	// 的执行证据。任何网络错误都直接返回，绝不自动重连重发可能已执行的切换。
	return response, nil
}

func newID() (model.RequestID, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", model.NewError(model.CodeInternal, "无法生成请求关联ID", err)
	}
	return model.RequestID("client-" + hex.EncodeToString(random[:])), nil
}

func unavailable(cause error) error {
	return model.NewError(model.CodeDaemonUnavailable, "后台未启动或不可用，请手动启动DisplayKVM后台后重试", cause)
}
