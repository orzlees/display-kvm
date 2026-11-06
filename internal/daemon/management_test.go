package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/ipc"
	"github.com/orzlee/displayKvm/internal/model"
)

type managementPendingWrite struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *managementPendingWrite) Write(data []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(data)
}

func TestManagementDrainWaitsForResponseAndRestoresAdmissionOnFailure(t *testing.T) {
	for _, mode := range []string{"delivered", "write-failed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			queue := &fakeQueue{}
			service, err := New(queue)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.RegisterManagement(func(context.Context, model.ManagementRequest) (model.ManagementResponse, error) {
				return model.ManagementResponse{Generation: "run-test"}, nil
			}, nil); err != nil {
				t.Fatal(err)
			}
			service.life = context.Background()
			probe := model.Request{Version: model.ProtocolVersion, ID: "pending-probe", Token: strings.Repeat("A", 43), Operation: model.OperationManagementCapabilities}
			if response := service.handle(context.Background(), probe); response.Error != nil || !response.Accepted {
				t.Fatalf("同步处理失败: %+v", response)
			}
			if service.responses != 0 {
				t.Fatal("直接调用无callback泄漏响应计数")
			}
			server, peer := net.Pipe()
			defer peer.Close()
			writing, served := make(chan struct{}), make(chan struct{})
			life, stop := context.WithCancel(context.Background())
			defer stop()
			go func() {
				defer close(served)
				ipc.ServeConn(life, &managementPendingWrite{Conn: server, started: writing}, probe.Token, service.handle)
			}()
			_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
			data, _ := json.Marshal(probe)
			if err := ipc.WriteFrame(peer, data); err != nil {
				t.Fatal(err)
			}
			select {
			case <-writing:
			case <-time.After(3 * time.Second):
				t.Fatal("未进入阻塞写帧")
			}
			drainCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			drained := make(chan error, 1)
			go func() { drained <- service.Drain(drainCtx) }()
			// 仅等待可观测的准入状态，排空完成仍由真实帧通知决定。
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			for {
				service.mu.Lock()
				gated := service.draining
				service.mu.Unlock()
				if gated {
					break
				}
				select {
				case <-tick.C:
				case <-deadline.C:
					t.Fatal("排空门禁未建立")
				}
			}
			if response := service.handle(context.Background(), probe); response.Error != nil || !response.Accepted {
				t.Fatal("能力探测必须保持可用")
			}
			status := model.Request{Operation: model.OperationManagement, Management: &model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: "commit-run-test-a"}}
			if response := service.handle(context.Background(), status); response.Error != nil {
				t.Fatalf("同ID查询被门禁阻止: %v", response.Error)
			}
			request := model.SwitchRequest{ID: "gated-switch", TargetID: "work"}
			if response := service.handle(context.Background(), model.Request{Operation: model.OperationSwitch, Switch: &request}); response.Error == nil || response.Error.Code != model.CodeBackendUnavailable {
				t.Fatal("排空期新Switch没有明确拒绝")
			}
			if _, err := service.switchRequest(context.Background(), request); model.CodeOf(err) != model.CodeBackendUnavailable {
				t.Fatal("排空期自动化仍能新发硬件操作")
			}
			if queue.switches.Load() != 0 {
				t.Fatal("拒绝请求触及队列")
			}
			select {
			case err := <-drained:
				t.Fatalf("帧尚未读出就完成Drain: %v", err)
			default:
			}
			if mode == "cancelled" {
				cancel()
				select {
				case err := <-drained:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("排空取消结果: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("取消未解除等待")
				}
			}
			if mode == "write-failed" {
				peer.Close()
			} else {
				body, err := ipc.ReadFrame(peer)
				if err != nil {
					t.Fatal(err)
				}
				response, err := model.DecodeResponse(body)
				if err != nil || !response.Accepted || response.ID != probe.ID {
					t.Fatalf("最终应答不完整: %+v %v", response, err)
				}
			}
			if mode != "cancelled" {
				select {
				case err := <-drained:
					if (err == nil) != (mode == "delivered") {
						t.Fatalf("Drain未对应写帧结果: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("最终帧未释放Drain")
				}
			}
			select {
			case <-served:
			case <-time.After(3 * time.Second):
				t.Fatal("真实ServeConn未退出")
			}
			service.mu.Lock()
			pending, gated := service.responses, service.draining
			service.mu.Unlock()
			if pending != 0 || gated != (mode == "delivered") {
				t.Fatalf("计数或门禁泄漏: pending=%d gated=%v", pending, gated)
			}
			if mode != "delivered" {
				if _, err := service.switchRequest(context.Background(), request); err != nil || queue.switches.Load() != 1 || queue.closed.Load() {
					t.Fatalf("失败后旧队列未恢复: %v", err)
				}
			}
		})
	}
}

func TestManagementDaemonDispatchAndCapabilityProjection(t *testing.T) {
	queue := &fakeQueue{}
	service, err := New(queue)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	if err := service.RegisterManagement(func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		calls.Add(1)
		return model.ManagementResponse{Generation: "run-real", Data: []byte(`{"action":"` + string(request.Action) + `"}`)}, nil
	}, func(caps []model.EventCapability) []model.EventCapability {
		// 回调不能持Daemon内部锁调用，否则这里的状态读取会死锁。
		service.mu.Lock()
		service.mu.Unlock()
		for i := range caps {
			if caps[i].Kind == model.EventTimer {
				caps[i].Available = true
				caps[i].Reason = "实际装配日程"
			}
		}
		return caps
	}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	token := strings.Repeat("A", 43)
	go func() { done <- service.Run(ctx, listener, token) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("后台未结束")
		}
	}()
	endpoint := model.Endpoint{Version: model.EndpointVersion, ProtocolVersion: model.ProtocolVersion, Network: "tcp4", Address: listener.Addr().String(), Token: token, PID: 1, StartedAt: time.Now().UTC()}
	connection, err := client.NewWithTransport(endpoint, (&net.Dialer{}).DialContext)
	if err != nil {
		t.Fatal(err)
	}
	response, err := connection.Management(context.Background(), model.ManagementRequest{Action: model.ManagementSnapshotAction})
	if err != nil || response.Generation != "run-real" || calls.Load() != 1 {
		t.Fatalf("真实协议未分派: %+v %v", response, err)
	}
	caps, err := connection.EventCapabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	available := false
	for _, capability := range caps {
		if capability.Kind == model.EventTimer {
			available = capability.Available
		}
	}
	if !available {
		t.Fatal("旧能力接口必须得到同一运行实例投影")
	}
	if queue.switches.Load() != 0 {
		t.Fatal("管理查询不能切换设备")
	}
	if err := service.RegisterManagement(nil, nil); err == nil {
		t.Fatal("运行中不得改管理入口")
	}
}
