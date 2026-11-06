package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	coreclient "github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/ipc"
	"github.com/orzlee/displayKvm/internal/model"
)

// 此夹具仅提供后台应答边界；Service、发现配置、核心客户端、帧校验和连接
// 失效均使用生产路径，不在页面或测试中重写桌面连接/门禁逻辑。
type managementServer struct {
	t           *testing.T
	root        string
	instance    *ipc.Instance
	cfg         config.Config
	mu          sync.Mutex
	generation  model.RuntimeGeneration
	requests    []model.ManagementRequest
	stepEntered chan struct{}
	stepRelease chan struct{}
	cancel      context.CancelFunc
	done        chan error
}

func newManagementServer(t *testing.T) *managementServer {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(strings.NewReader(`{"version":2,"displays":[{"id":"left","name":"左屏","identity":{"manufacturer":"ABC","serial":"one"}}],"input_schemes":[{"id":"hdmi","protocol":"vcp","vcp_code":96,"value":17}],"targets":[{"id":"work","name":"工作机","inputs":[{"display_id":"left","scheme_id":"hdmi"}]}],"groups":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	instance, err := ipc.Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	server := &managementServer{t: t, root: root, instance: instance, cfg: cfg}
	server.start("run-one")
	t.Cleanup(func() {
		server.stop()
		if err := instance.Close(); err != nil {
			t.Error(err)
		}
	})
	return server
}

func (s *managementServer) stop() {
	if s.cancel != nil {
		s.cancel()
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
			s.t.Error("协议夹具未停止")
		}
		s.cancel = nil
	}
}
func (s *managementServer) start(generation model.RuntimeGeneration) {
	s.generation = generation
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	endpoint, err := s.instance.PublishRuntime(listener.Addr().String(), s.cfg)
	if err != nil {
		listener.Close()
		s.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan error, 1)
	go func() { s.done <- ipc.Serve(ctx, listener, endpoint.Token, s.handle) }()
}
func (s *managementServer) handle(ctx context.Context, request model.Request) model.Response {
	response := model.Response{Version: model.ProtocolVersion, ID: request.ID, Operation: request.Operation}
	switch request.Operation {
	case model.OperationEnumerate:
		response.Devices = []model.Device{{BackendID: "physical", Name: "左屏", Identity: model.IdentityHints{Manufacturer: "ABC", Serial: "one"}}}
	case model.OperationManagementCapabilities:
		response.Accepted = true
	case model.OperationManagement:
		s.mu.Lock()
		s.requests = append(s.requests, *request.Management)
		generation := s.generation
		entered, release := s.stepEntered, s.stepRelease
		s.mu.Unlock()
		management := model.ManagementResponse{Generation: generation}
		switch request.Management.Action {
		case model.ManagementSnapshotAction:
			var cfg bytes.Buffer
			if err := config.Encode(&cfg, s.cfg); err != nil {
				response.Error = ipc.PublicError(err)
				return response
			}
			management.Snapshot = &model.ManagementSnapshot{Generation: generation, Configuration: cfg.Bytes(), Rules: json.RawMessage(`{"version":1,"rules":[]}`)}
		case model.ManagementApplyCommit:
			management.Commit = &model.CommitStatus{ID: request.Management.CommitID, Kind: model.ManagementPrepareConfig, Phase: model.CommitApplying, PreparedGeneration: "run-one"}
		case model.ManagementCommitStatus:
			value := true
			management.Commit = &model.CommitStatus{ID: request.Management.CommitID, Kind: model.ManagementPrepareConfig, Phase: model.CommitApplied, PreparedGeneration: "run-one", EffectiveGeneration: generation, Saved: &value, Effective: &value}
		case model.ManagementTrialStep:
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				response.Error = model.NewError(model.CodeCanceled, "试切等待已取消", nil)
				return response
			}
		case model.ManagementTrialStop:
			close(release)
		}
		response.Management = &management
	default:
		response.Error = model.NewError(model.CodeUnsupportedOperation, "夹具未提供此操作", nil)
	}
	return response
}

func TestManagementServiceReconnectAndQueryOriginalCommitAcrossGeneration(t *testing.T) {
	server := newManagementServer(t)
	connects := 0
	service := New(func() (Client, error) { connects++; return coreclient.Connect(server.root) })
	defer service.Close()
	if err := service.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.ManagementSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	before := service.generation
	service.mu.Unlock()
	id, err := coreclient.NewCommitID(snapshot.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Management(context.Background(), model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: snapshot.Generation, CommitID: id}); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	connected := service.connected
	service.mu.Unlock()
	if connected {
		t.Fatal("Apply后应要求只读重连")
	}
	if _, err := service.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: id}); err == nil {
		t.Fatal("断开状态不应暗中新建客户端")
	}
	if connects != 1 {
		t.Fatal("Apply不能自动重连或重发")
	}
	server.stop()
	server.start("run-two")
	if err := service.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := service.Management(context.Background(), model.ManagementRequest{Action: model.ManagementCommitStatus, ExpectedGeneration: snapshot.Generation, CommitID: id})
	if err != nil || result.Commit == nil || result.Commit.ID != id || result.Generation != "run-two" || result.Commit.Phase != model.CommitApplied {
		t.Fatalf("旧代原ID应可查询: %+v %v", result, err)
	}
	service.mu.Lock()
	connected, after, managementGeneration := service.connected, service.generation, service.managementGeneration
	service.mu.Unlock()
	if !connected || after <= before || managementGeneration != "run-two" || connects != 2 {
		t.Fatalf("同Service未更新共享代: connected=%v before=%d after=%d generation=%s connects=%d", connected, before, after, managementGeneration, connects)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	applies, prepares := 0, 0
	for _, request := range server.requests {
		if request.Action == model.ManagementApplyCommit {
			applies++
		}
		if request.Action == model.ManagementPrepareConfig {
			prepares++
		}
	}
	if applies != 1 || prepares != 0 {
		t.Fatalf("重连查询不能Prepare/Apply重放: %d/%d", prepares, applies)
	}
}

func TestManagementServiceSharedBusyAndSameSessionStop(t *testing.T) {
	server := newManagementServer(t)
	server.stepEntered, server.stepRelease = make(chan struct{}), make(chan struct{})
	service := New(func() (Client, error) { return coreclient.Connect(server.root) })
	defer service.Close()
	if err := service.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.Management(context.Background(), model.ManagementRequest{Action: model.ManagementTrialStep, ExpectedGeneration: "run-one", SessionID: "session-one", Confirmed: true})
		done <- err
	}()
	select {
	case <-server.stepEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("未进入真实IPC调用")
	}
	if err := service.Reconnect(context.Background()); err == nil {
		t.Fatal("共享busy期间不得重连")
	}
	if err := service.BeginSwitch(Selection{}); err == nil {
		t.Fatal("管理试切与概览切换必须共用门禁")
	}
	if _, err := service.Management(context.Background(), model.ManagementRequest{Action: model.ManagementTrialStop, ExpectedGeneration: "run-one", SessionID: "session-other"}); err == nil {
		t.Fatal("其他会话不能穿透busy门禁")
	}
	if _, err := service.Management(context.Background(), model.ManagementRequest{Action: model.ManagementTrialStop, ExpectedGeneration: "run-one", SessionID: "session-one"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop后Step未退出")
	}
	service.mu.Lock()
	busy := service.busy
	service.mu.Unlock()
	if busy {
		t.Fatal("Step结束后共享门禁未释放")
	}
}
