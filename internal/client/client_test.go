package client

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/ipc"
	"github.com/orzlee/displayKvm/internal/model"
)

func TestManagementLegacyProbeAndNoApplyRetry(t *testing.T) {
	for _, old := range []bool{true, false} {
		t.Run(map[bool]string{true: "old-backend", false: "apply-disconnect"}[old], func(t *testing.T) {
			serverCtx, stopServer := context.WithCancel(context.Background())
			t.Cleanup(stopServer)
			var dials atomic.Int32
			var applied atomic.Int32
			endpoint := testEndpoint()
			client, err := NewWithTransport(endpoint, func(ctx context.Context, network, address string) (net.Conn, error) {
				attempt := dials.Add(1)
				local, remote := net.Pipe()
				if !old && attempt == 2 {
					go func() {
						defer remote.Close()
						body, err := ipc.ReadFrame(remote)
						request, decodeErr := model.DecodeRequest(body)
						if err == nil && decodeErr == nil && request.Token == endpoint.Token && request.Operation == model.OperationManagement && request.Management != nil && request.Management.Action == model.ManagementApplyCommit && request.Management.CommitID == "commit-run-one-a" {
							applied.Add(1)
						}
					}()
					return local, nil
				}
				go func() {
					ipc.ServeConn(serverCtx, remote, endpoint.Token, func(_ context.Context, request model.Request) model.Response {
						response := model.Response{Version: model.ProtocolVersion, ID: request.ID, Operation: request.Operation}
						if old {
							response.Error = model.NewError(model.CodeUnsupportedOperation, "旧后台不支持管理", nil)
						} else {
							response.Accepted = true
						}
						return response
					})
				}()
				return local, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Management(context.Background(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: "run-one", CommitID: "commit-run-one-a"})
			if err == nil {
				t.Fatal("旧协议或Apply断连须报错误，由上层标记unknown")
			}
			if old && (model.CodeOf(err) != model.CodeUnsupportedOperation || dials.Load() != 1) {
				t.Fatalf("旧后台应明确不支持且不发新payload: %v dials=%d", err, dials.Load())
			}
			if !old && (dials.Load() != 2 || applied.Load() != 1) {
				t.Fatalf("Apply不得自动重发: dials=%d applied=%d", dials.Load(), applied.Load())
			}
		})
	}
	id, err := NewCommitID("run-one")
	if err != nil || !strings.HasPrefix(string(id), "commit-run-one-") {
		t.Fatalf("提交ID预生成失败: %q %v", id, err)
	}
}

func testEndpoint() model.Endpoint {
	return model.Endpoint{Version: 1, ProtocolVersion: 1, Network: "tcp4", Address: "127.0.0.1:12345", Token: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", PID: 1, StartedAt: time.Now()}
}

func TestFourOperationsUseSameAuthenticatedTransport(t *testing.T) {
	c, err := NewWithTransport(testEndpoint(), func(ctx context.Context, network, address string) (net.Conn, error) {
		server, peer := net.Pipe()
		go ipc.ServeConn(context.Background(), server, testEndpoint().Token, func(_ context.Context, r model.Request) model.Response {
			response := model.Response{Version: 1, ID: r.ID, Operation: r.Operation}
			switch r.Operation {
			case model.OperationSwitch:
				response.SwitchResult = &model.SwitchResult{RequestID: r.ID, Error: model.NewError(model.CodeNotFound, "逐屏请求失败证据", nil)}
			case model.OperationEmitEvent:
				response.Accepted = true
			}
			return response
		})
		return peer, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Switch(context.Background(), model.SwitchRequest{ID: "switch-1", TargetID: "target-1"})
	if err != nil || result.RequestID != "switch-1" || result.Error == nil {
		t.Fatalf("覆盖受理结果: %+v %v", result, err)
	}
	if _, err := c.Enumerate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EventCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.EmitEvent(context.Background(), model.Event{ID: "custom-1", Kind: model.EventCustom, Source: "cli", Name: "desk", ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestDisconnectNeverRetriesAndCorrelationIsChecked(t *testing.T) {
	for _, wrongID := range []bool{false, true} {
		var dials atomic.Int32
		c, _ := NewWithTransport(testEndpoint(), func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			server, peer := net.Pipe()
			go func() {
				defer server.Close()
				data, _ := ipc.ReadFrame(server)
				var request model.Request
				_ = json.Unmarshal(data, &request)
				if wrongID {
					response, _ := json.Marshal(model.Response{Version: 1, ID: "wrong-id", Operation: request.Operation})
					_ = ipc.WriteFrame(server, response)
				}
			}()
			return peer, nil
		})
		if _, err := c.Enumerate(context.Background()); err == nil {
			t.Fatal("断连/错误关联未拒绝")
		}
		if dials.Load() != 1 {
			t.Fatal("自动重发了请求")
		}
	}
}

func TestCancellationClosesConnectionAndUnavailableIsExplicit(t *testing.T) {
	closed := make(chan struct{})
	c, _ := NewWithTransport(testEndpoint(), func(context.Context, string, string) (net.Conn, error) {
		server, peer := net.Pipe()
		go func() {
			defer server.Close()
			_, _ = ipc.ReadFrame(server)
			var b [1]byte
			_, _ = server.Read(b[:])
			close(closed)
		}()
		return peer, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Enumerate(ctx); model.CodeOf(err) != model.CodeTimeout {
		t.Fatalf("丢失超时: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("取消后连接未释放")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Connect(root); runtime.GOOS != "windows" && model.CodeOf(err) != model.CodeDaemonUnavailable {
		t.Fatalf("后台不可达解释: %v", err)
	}
}

func TestOversizedAcceptedResultIsNeverReportedAsRequestRejection(t *testing.T) {
	c, err := NewWithTransport(testEndpoint(), func(context.Context, string, string) (net.Conn, error) {
		server, peer := net.Pipe()
		go ipc.ServeConn(context.Background(), server, testEndpoint().Token, func(_ context.Context, request model.Request) model.Response {
			return model.Response{SwitchResult: &model.SwitchResult{RequestID: request.ID, Error: model.NewError(model.CodeIO, strings.Repeat("x", model.MaxMessageBytes), nil)}}
		})
		return peer, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(context.Background(), model.SwitchRequest{ID: "large-result", TargetID: "target"}); model.CodeOf(err) != model.CodeTransport {
		t.Fatalf("受理结果被改写为请求级拒绝: %v", err)
	}
}

func TestConfigurationUsesDaemonSnapshotAndRefusesRestartOrMissingSnapshot(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("本测试限定Unix私有文件实现")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const document = `{"version":2,"displays":[{"id":"left","name":"左屏","identity":{"manufacturer":"ABC","serial":"same"}}],"input_schemes":[{"id":"hdmi","protocol":"vcp","vcp_code":96,"value":17}],"targets":[{"id":"work","name":"工作机","inputs":[{"display_id":"left","scheme_id":"hdmi"}]}],"groups":[{"id":"desk","name":"桌面","display_ids":["left"]}]}`
	original := filepath.Join(root, "original.json")
	if err := os.WriteFile(original, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	first, err := ipc.Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.PublishConfiguration(cfg); err != nil {
		t.Fatal(err)
	}
	if err := first.Publish(first.Endpoint("127.0.0.1:12345")); err != nil {
		t.Fatal(err)
	}
	c, err := Connect(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte(strings.ReplaceAll(document, "工作机", "后来修改")), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := c.Configuration()
	if err != nil || len(loaded.Groups) != 1 || loaded.Targets[0].Name != "工作机" {
		t.Fatalf("客户端偏离后台固定快照: %+v %v", loaded, err)
	}
	loaded.Groups[0].DisplayIDs[0] = "caller-mutated"
	loaded, err = c.Configuration()
	if err != nil || loaded.Groups[0].DisplayIDs[0] != "left" {
		t.Fatalf("调用方修改污染快照: %+v %v", loaded, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := ipc.Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Publish(second.Endpoint("127.0.0.1:12346")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Configuration(); model.CodeOf(err) != model.CodeDaemonUnavailable {
		t.Fatalf("旧客户端读取了新实例配置: %v", err)
	}
	current, err := Connect(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := current.Configuration(); model.CodeOf(err) != model.CodeDaemonUnavailable {
		t.Fatalf("缺少快照时静默返回默认配置: %v", err)
	}
	injected, _ := New(testEndpoint())
	if _, err := injected.Configuration(); model.CodeOf(err) != model.CodeInvalidRequest {
		t.Fatalf("手工端点客户端偷偷寻找备用配置: %v", err)
	}
}
