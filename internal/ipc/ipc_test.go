package ipc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

func TestManagementRuntimePublicationKeepsLockAndRejectsOldSnapshot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(strings.NewReader(`{"version":2,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	instance, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	first, err := instance.PublishRuntime("127.0.0.1:12345", cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := instance.PublishRuntime("127.0.0.1:12346", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token || first.StartedAt != second.StartedAt || first.PID != second.PID {
		t.Fatalf("换轮须换凭据而保持单例身份: %+v %+v", first, second)
	}
	if _, err := LoadConfiguration(root, first); model.CodeOf(err) != model.CodeDaemonUnavailable {
		t.Fatalf("旧轮快照必须拒绝: %v", err)
	}
	if _, err := LoadConfiguration(root, second); err != nil {
		t.Fatal(err)
	}
	if other, err := Acquire(root); err == nil {
		other.Close()
		t.Fatal("换轮间系统锁不得释放")
	}
	bad := cfg
	bad.Version = 999
	if _, err := instance.PublishRuntime("127.0.0.1:12347", bad); err == nil {
		t.Fatal("错误配置不得发布新端点")
	}
	published, err := LoadEndpoint(root)
	if err != nil || published.Token != second.Token {
		t.Fatalf("失败发布破坏原端点: %+v %v", published, err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfiguration(root, second); err == nil {
		t.Fatal("关闭必须回收新轮快照")
	}
}

const testToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// net.Pipe没有内核发送缓冲；第一次Write开始后，在peer读取前必然仍未完成帧。
// 用真实写入边界建立时序，避免以handler返回或任意等待时长冒充发送完成。
type managementWriteBarrierConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *managementWriteBarrierConn) Write(data []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(data)
}

func TestManagementResponseCompletionWaitsForFinalFrame(t *testing.T) {
	if OnResponseComplete(context.Background(), func(error) {}) {
		t.Fatal("非IPC context不能登记网络完成回调")
	}
	for _, mode := range []string{"read", "disconnect", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			server, peer := net.Pipe()
			defer peer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writing, done, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			completed := make(chan error, 2)
			var count atomic.Int32
			go func() {
				defer close(done)
				ServeConn(ctx, &managementWriteBarrierConn{Conn: server, started: writing}, testToken, func(ctx context.Context, request model.Request) model.Response {
					if !OnResponseComplete(ctx, func(err error) { count.Add(1); completed <- err }) {
						t.Error("真实处理器未获得写帧通知")
					}
					if OnResponseComplete(ctx, func(error) { t.Error("重复登记被调用") }) {
						t.Error("每连接只能登记一个完成通知")
					}
					close(returned)
					return model.Response{Version: model.ProtocolVersion, ID: request.ID, Operation: request.Operation, Devices: []model.Device{}}
				})
			}()
			_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
			data, _ := json.Marshal(model.Request{Version: model.ProtocolVersion, ID: "completion-one", Token: testToken, Operation: model.OperationEnumerate})
			if err := WriteFrame(peer, data); err != nil {
				t.Fatal(err)
			}
			select {
			case <-writing:
			case <-time.After(3 * time.Second):
				t.Fatal("未进入真实写帧")
			}
			<-returned
			select {
			case err := <-completed:
				t.Fatalf("未读取帧就提前完成: %v", err)
			default:
			}
			if mode == "read" {
				data, err := ReadFrame(peer)
				if err != nil {
					t.Fatal(err)
				}
				response, err := model.DecodeResponse(data)
				if err != nil || response.ID != "completion-one" || response.Operation != model.OperationEnumerate {
					t.Fatalf("关联响应不完整: %+v %v", response, err)
				}
			} else if mode == "disconnect" {
				peer.Close()
			} else {
				cancel()
			}
			select {
			case err := <-completed:
				if (err == nil) != (mode == "read") {
					t.Fatalf("写帧状态与真实传输不符: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("写帧结束未释放屏障")
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("ServeConn未退出")
			}
			if count.Load() != 1 {
				t.Fatalf("完成通知次数=%d", count.Load())
			}
		})
	}
}

func TestFrameRejectsLengthBeforeReadingBody(t *testing.T) {
	for _, size := range []uint32{0, model.MaxMessageBytes + 1, ^uint32(0)} {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], size)
		_, err := ReadFrame(bytes.NewReader(header[:]))
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("长度%d未在读正文前拒绝: %v", size, err)
		}
	}
	_, err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 3, '{'}))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("部分帧: %v", err)
	}
}

func TestAuthenticationAndVersionAreBoundedAndCorrelated(t *testing.T) {
	var calls atomic.Int32
	for _, tc := range []struct {
		token   string
		version int
		code    model.ErrorCode
	}{
		{strings.Repeat("B", 43), 1, model.CodeAuthenticationFailed},
		{testToken, 99, model.CodeUnsupportedVersion},
	} {
		server, peer := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			ServeConn(context.Background(), server, testToken, func(context.Context, model.Request) model.Response {
				calls.Add(1)
				return model.Response{}
			})
		}()
		request := model.Request{Version: tc.version, ID: "auth-1", Token: tc.token, Operation: model.OperationEnumerate}
		data, _ := json.Marshal(request)
		_ = peer.SetDeadline(time.Now().Add(time.Second))
		if err := WriteFrame(peer, data); err != nil {
			t.Fatal(err)
		}
		data, err := ReadFrame(peer)
		if err != nil {
			t.Fatal(err)
		}
		response, err := model.DecodeResponse(data)
		if err != nil || response.ID != request.ID || response.Operation != request.Operation || response.Error == nil || response.Error.Code != tc.code {
			t.Fatalf("拒绝响应: %+v %v", response, err)
		}
		peer.Close()
		<-done
	}
	if calls.Load() != 0 {
		t.Fatal("拒绝前调用了业务处理器")
	}
}

func TestDisconnectCancelsAcceptedHandlerAndWaitsForCleanup(t *testing.T) {
	server, peer := net.Pipe()
	started, cleaned, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ServeConn(context.Background(), server, testToken, func(ctx context.Context, request model.Request) model.Response {
			close(started)
			<-ctx.Done()
			close(cleaned)
			return model.Response{Version: 1, ID: request.ID, Operation: request.Operation}
		})
	}()
	data, _ := json.Marshal(model.Request{Version: 1, ID: "disconnect", Token: testToken, Operation: model.OperationEnumerate})
	if err := WriteFrame(peer, data); err != nil {
		t.Fatal(err)
	}
	<-started
	peer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("断连后未停止")
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("返回时业务清理尚未结束")
	}
}

func TestPrivateEndpointLockAndOwnedCleanup(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Acquire(root); err == nil {
		second.Close()
		t.Fatal("允许第二实例")
	}
	endpoint := first.Endpoint("127.0.0.1:12345")
	if err := first.Publish(endpoint); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadEndpoint(root)
	if err != nil || loaded.Token != endpoint.Token {
		t.Fatalf("端点往返: %v", err)
	}
	// 替换文件模拟其他发布者发布；旧实例退出不得删除新inode。
	// 使用平台私有创建边界，Windows提升令牌的默认owner可能并非用户SID。
	replacement, err := createPrivateTemp(filepath.Dir(model.EndpointPath(root)), ".replacement-*")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(endpoint)
	if _, err := replacement.Write(data); err != nil {
		replacement.Close()
		t.Fatal(err)
	}
	replacementInfo, statError := replacement.Stat()
	if err := errors.Join(statError, replacement.Close()); err != nil {
		t.Fatal(err)
	}
	if err := replacePrivate(replacement.Name(), model.EndpointPath(root), replacementInfo); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(model.EndpointPath(root)); err != nil {
		t.Fatalf("误删替换端点: %v", err)
	}
	third, err := Acquire(root)
	if err != nil {
		t.Fatalf("退出后锁未释放: %v", err)
	}
	third.Close()
}

func TestPrivateEndpointRejectsPermissionsAndSymlink(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("本测试使用POSIX权限位；Windows的DACL和重解析点由专用用例验证")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	instance, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if err := instance.Publish(instance.Endpoint("127.0.0.1:12345")); err != nil {
		t.Fatal(err)
	}
	path := model.EndpointPath(root)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEndpoint(root); model.CodeOf(err) != model.CodePermissionDenied {
		t.Fatalf("公开端点未拒绝: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "other"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEndpoint(root); model.CodeOf(err) != model.CodePermissionDenied {
		t.Fatalf("符号链接未拒绝: %v", err)
	}
}

type memoryListener struct {
	conn     net.Conn
	accepted bool
	closed   chan struct{}
	once     sync.Once
}

func (l *memoryListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *memoryListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}
func (*memoryListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
}

func TestGracefulStopClosesIdleConnectionAndListener(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	listener := &memoryListener{conn: server, closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, testToken, func(context.Context, model.Request) model.Response {
			t.Error("空闲连接不应受理请求")
			return model.Response{}
		})
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("停止未释放空闲连接")
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := peer.Read(b[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("连接未关闭: %v", err)
	}
}

func TestNewConfigDirectoryAndUnsafeRuntimeAreHandledExplicitly(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("本测试限定Unix私有目录实现")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configDirectory := filepath.Join(root, "new-config")
	instance, err := Acquire(configDirectory)
	if err != nil {
		t.Fatalf("首次启动未创建私有配置目录: %v", err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(configDirectory, "DisplayKVM", "runtime"), 0755); err != nil {
		t.Fatal(err)
	}
	if instance, err := Acquire(configDirectory); model.CodeOf(err) != model.CodePermissionDenied {
		if instance != nil {
			instance.Close()
		}
		t.Fatalf("宽松目录未拒绝: %v", err)
	}
}

func TestListenerRejectsPublicAddressAndInvalidToken(t *testing.T) {
	if err := ValidateListener(&addressListener{address: &net.TCPAddr{IP: net.IPv4zero, Port: 12345}}, testToken); err == nil {
		t.Fatal("允许非回环监听")
	}
	if err := ValidateListener(&addressListener{address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}}, "short"); err == nil {
		t.Fatal("允许无效认证材料")
	}
}

type addressListener struct{ address net.Addr }

func (l *addressListener) Addr() net.Addr          { return l.address }
func (*addressListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (*addressListener) Close() error              { return nil }

func TestConfigurationSnapshotIsImmutablePrivateAndBoundToInstance(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const document = `{"version":2,"displays":[{"id":"left","name":"左屏","identity":{"manufacturer":"ABC","serial":"same"}}],"input_schemes":[{"id":"hdmi","protocol":"vcp","vcp_code":96,"value":17}],"targets":[{"id":"work","name":"工作机","inputs":[{"display_id":"left","scheme_id":"hdmi"}]}],"groups":[{"id":"desk","name":"桌面","display_ids":["left"]}]}`
	cfg, err := config.Load(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	instance, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	endpoint := instance.Endpoint("127.0.0.1:12345")
	if err := instance.PublishConfiguration(cfg); err != nil {
		t.Fatal(err)
	}
	if err := instance.Publish(endpoint); err != nil {
		t.Fatal(err)
	}
	cfg.Targets[0].Name = "后来修改"
	if err := instance.PublishConfiguration(cfg); err == nil {
		t.Fatal("允许修改本轮已发布配置")
	}
	loaded, err := LoadConfiguration(root, endpoint)
	if err != nil || loaded.Targets[0].Name != "工作机" || loaded.Groups[0].ID != "desk" {
		t.Fatalf("固定配置快照发生偏离: %+v %v", loaded, err)
	}
	wrongInstance := endpoint
	wrongInstance.Token = testToken
	if _, err := LoadConfiguration(root, wrongInstance); model.CodeOf(err) != model.CodeDaemonUnavailable {
		t.Fatalf("允许跨实例读取配置: %v", err)
	}
	path := configurationPath(filepath.Dir(model.EndpointPath(root)), endpoint.Token)
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		// Windows权限由DACL而非POSIX模式位控制；只隔离权限位断言，
		// 固定快照、实例绑定及关闭清理仍必须在Windows真实执行。
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfiguration(root, endpoint); model.CodeOf(err) != model.CodePermissionDenied {
			t.Fatalf("公开配置未拒绝: %v", err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("退出未回收本轮配置: %v", err)
	}
	if _, err := LoadConfiguration(root, endpoint); err == nil {
		t.Fatal("退出后仍提供配置")
	}
}
