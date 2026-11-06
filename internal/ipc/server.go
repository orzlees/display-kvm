package ipc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

type Handler func(context.Context, model.Request) model.Response

type responseCompletionKey struct{}

type responseCompletion struct {
	mu       sync.Mutex
	finished bool
	callback func(error)
}

// OnResponseComplete仅供处理器登记本连接的完成屏障。业务返回不代表响应已交付；
// 受控换代必须等最终帧写入返回，才能关闭旧连接。通知也覆盖取消、非法结果和写失败，
// 不承诺对端已经读取；每连接最多登记一次，非IPC调用返回false由调用方同步清理。
func OnResponseComplete(ctx context.Context, done func(error)) bool {
	completion, ok := ctx.Value(responseCompletionKey{}).(*responseCompletion)
	if !ok || done == nil {
		return false
	}
	completion.mu.Lock()
	defer completion.mu.Unlock()
	if completion.finished || completion.callback != nil {
		return false
	}
	completion.callback = done
	return true
}

func (c *responseCompletion) complete(err error) {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	c.finished = true
	callback := c.callback
	c.callback = nil
	c.mu.Unlock()
	if callback != nil {
		callback(err)
	}
}

// ValidateListener复用端点契约，在启动来源前拒绝公开地址与无效认证材料。
func ValidateListener(listener net.Listener, token string) error {
	if listener == nil || listener.Addr() == nil {
		return model.NewError(model.CodeInvalidRequest, "IPC监听器不能为空", nil)
	}
	endpoint := model.Endpoint{Version: model.EndpointVersion, ProtocolVersion: model.ProtocolVersion, Network: "tcp4", Address: listener.Addr().String(), Token: token, PID: os.Getpid(), StartedAt: time.Now()}
	return endpoint.Validate()
}

// Serve接管listener及接收的连接。退出先停止接收并关闭连接，再等待处理器清理；
// 不通过遗留业务协程来伪装及时返回，后端真正停止由后台拥有的08队列保证。
func Serve(ctx context.Context, listener net.Listener, token string, handler Handler) error {
	if listener == nil || handler == nil {
		return model.NewError(model.CodeInvalidRequest, "IPC监听器和处理器不能为空", nil)
	}
	defer listener.Close()
	if err := ValidateListener(listener, token); err != nil {
		return err
	}
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	connections := make(map[net.Conn]struct{})
	var workers sync.WaitGroup
	stop := context.AfterFunc(life, func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for conn := range connections {
			conn.Close()
		}
	})
	defer stop()
	defer workers.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return model.NewError(model.CodeTransport, "IPC监听已停止", err)
		}
		mu.Lock()
		if life.Err() != nil || len(connections) >= 64 {
			mu.Unlock()
			conn.Close()
			continue
		}
		connections[conn] = struct{}{}
		mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				mu.Lock()
				delete(connections, conn)
				mu.Unlock()
			}()
			ServeConn(life, conn, token, handler)
		}()
	}
}

// ServeConn只处理一条消息。公开此边界供注入net.Pipe测试，不启用其他传输协议。
func ServeConn(ctx context.Context, conn net.Conn, token string, handler Handler) {
	defer conn.Close()
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	completion := &responseCompletion{}
	life = context.WithValue(life, responseCompletionKey{}, completion)
	var writeErr error = model.NewError(model.CodeTransport, "响应未完成写入", nil)
	// 此defer在处理器、监听读取协程及最终WriteFrame退出后触发；不能把handler
	// 返回或连接被关闭当成成功交付。失败路径仍须释放后台排空计数，防止永久等待。
	defer func() { completion.complete(writeErr) }()
	stop := context.AfterFunc(life, func() { conn.Close() })
	defer stop()
	if err := conn.SetReadDeadline(Deadline(life, model.ResponseTimeout)); err != nil {
		return
	}
	body, err := ReadFrame(conn)
	if err != nil {
		return
	}
	var request model.Request
	if err := model.DecodeJSON(body, &request); err != nil {
		return
	}
	// 无可信关联字段的非法消息直接关闭；认证失败仍仅回显合法关联ID及操作名。
	if !model.ValidID(string(request.ID)) || !model.ValidID(string(request.Operation)) {
		return
	}
	response := model.Response{Version: model.ProtocolVersion, ID: request.ID, Operation: request.Operation}
	if len(token) != 43 || subtle.ConstantTimeCompare([]byte(request.Token), []byte(token)) != 1 {
		response.Error = model.NewError(model.CodeAuthenticationFailed, "本地请求认证失败", nil)
	} else if err := request.Validate(); err != nil {
		response.Error = PublicError(err)
	} else {
		budget := model.DefaultRequestTimeout
		if request.Switch != nil {
			budget = request.Switch.EffectiveTimeout()
		}
		operation, finish := context.WithTimeout(life, budget)
		// 协议不允许流水线；正文后的EOF或额外数据均取消当前操作。读监听有硬期限，
		// 正常业务返回后主动打断并等待它，防止每连接遗留一个阻塞读取协程。
		watchDone := make(chan struct{})
		if err := conn.SetReadDeadline(Deadline(operation, model.ResponseTimeout)); err != nil {
			finish()
			return
		}
		go func() {
			defer close(watchDone)
			var extra [1]byte
			_, _ = conn.Read(extra[:])
			finish()
		}()
		response = handler(operation, request)
		if err := conn.SetReadDeadline(time.Now()); err != nil {
			conn.Close()
		}
		<-watchDone
		finish()
		// 受理后的取消和部分失败属于SwitchResult；这里不再以ctx.Err覆盖结果。
		response.Version, response.ID, response.Operation = model.ProtocolVersion, request.ID, request.Operation
	}
	if err := response.Validate(); err != nil {
		if response.SwitchResult != nil {
			// 已受理结果若违反契约，不能改写成“请求未执行”的外层拒绝。关闭连接让
			// 客户端明确保留执行状态未知，并要求人工核对；也不发送非法逐屏证据。
			writeErr = err
			return
		}
		response = model.Response{Version: model.ProtocolVersion, ID: request.ID, Operation: request.Operation, Error: model.NewError(model.CodeInternal, "后台响应不符合协议", err)}
	}
	body, err = json.Marshal(response)
	if err != nil || len(body) > model.MaxMessageBytes {
		if response.SwitchResult != nil {
			// 帧契约有1MiB硬上限，超限时无法完整传输所有逐屏结果；禁止删减证据或用
			// 外层错误覆盖受理结果。断连沿用客户端“可能已受理、绝不重发”的语义。
			writeErr = model.NewError(model.CodeMessageTooLarge, "受理结果无法完整写入响应帧", err)
			return
		}
		response = model.Response{Version: model.ProtocolVersion, ID: request.ID, Operation: request.Operation, Error: model.NewError(model.CodeMessageTooLarge, "后台响应超过协议大小限制", err)}
		body, _ = json.Marshal(response)
	}
	if err := conn.SetWriteDeadline(Deadline(life, model.ResponseTimeout)); err != nil {
		writeErr = err
		return
	}
	writeErr = WriteFrame(conn, body)
}

// TransportError保留本地取消及网络超时类别，不把断连解释为未受理或自动可重试。
func TransportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return PublicError(ctx.Err())
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return model.NewError(model.CodeTimeout, "本地通信阶段超时；请核对实际结果", err)
	}
	return model.NewError(model.CodeTransport, "本地连接已断开；请求可能已受理，请核对结果后使用新请求ID", err)
}
