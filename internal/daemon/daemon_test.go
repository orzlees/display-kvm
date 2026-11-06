package daemon

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/model"
)

type fakeQueue struct {
	closed   atomic.Bool
	switches atomic.Int32
}

func (q *fakeQueue) Switch(_ context.Context, r model.SwitchRequest) (model.SwitchResult, error) {
	q.switches.Add(1)
	return model.SwitchResult{RequestID: r.ID, Error: model.NewError(model.CodeNotFound, "保留受理结果", nil)}, nil
}
func (*fakeQueue) Enumerate(ctx context.Context) ([]model.Device, error) {
	return nil, ctx.Err()
}
func (q *fakeQueue) Close() error {
	q.closed.Store(true)
	return nil
}

type fakeSource struct {
	ready   chan struct{}
	done    chan struct{}
	value   Baseline
	fail    error
	cleanup error
}

func (*fakeSource) Name() string { return "fake" }
func (*fakeSource) Capabilities(context.Context) ([]model.EventCapability, error) {
	return []model.EventCapability{{Kind: model.EventUSBAdded, Available: true}}, nil
}
func (s *fakeSource) Run(ctx context.Context, out chan<- model.Event) error {
	defer close(s.done)
	close(s.ready)
	<-ctx.Done()
	return errors.Join(ctx.Err(), s.cleanup)
}
func (s *fakeSource) Snapshot(ctx context.Context) (Baseline, error) {
	select {
	case <-ctx.Done():
		return Baseline{}, ctx.Err()
	case <-s.ready:
		return s.value, s.fail
	}
}

type automationFunc func(context.Context) error

func (f automationFunc) Run(ctx context.Context) error { return f(ctx) }

type pipeListener struct {
	incoming chan net.Conn
	closed   chan struct{}
	once     sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{incoming: make(chan net.Conn), closed: make(chan struct{})}
}
func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}
func (*pipeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
}

func TestRegistrationRescanUsesFreshRoundAndRunWaitsForOwnedResources(t *testing.T) {
	q := &fakeQueue{}
	d, err := New(q)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sources []*fakeSource
	factory := func() Source {
		mu.Lock()
		defer mu.Unlock()
		s := &fakeSource{ready: make(chan struct{}), done: make(chan struct{}), value: Baseline{ObservedAt: time.Now(), USBKnown: true, USBDevices: []model.USBDevice{{VendorID: uint16(len(sources) + 1)}}}}
		sources = append(sources, s)
		return s
	}
	if err := d.RegisterEventSource(factory); err != nil {
		t.Fatal(err)
	}
	checked := make(chan error, 1)
	if err := d.RegisterAutomation(func(rt AutomationRuntime) (Automation, error) {
		return automationFunc(func(ctx context.Context) error {
			first, err := rt.Rescan(ctx)
			if err != nil {
				checked <- err
				return err
			}
			second, err := rt.Rescan(ctx)
			if err == nil && (len(first.USBDevices) != 1 || len(second.USBDevices) != 1 || first.USBDevices[0].VendorID == second.USBDevices[0].VendorID) {
				err = errors.New("重复使用旧Snapshot")
			}
			checked <- err
			<-ctx.Done()
			return ctx.Err()
		}), nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, newPipeListener(), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") }()
	select {
	case err := <-checked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("补扫阻塞")
	}
	if err := d.RegisterEventSource(factory); err == nil {
		t.Fatal("Run后仍允许注册来源")
	}
	if err := d.RegisterAutomation(func(AutomationRuntime) (Automation, error) { return nil, nil }); err == nil {
		t.Fatal("Run后仍允许注册自动化")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("退出未回收")
	}
	if !q.closed.Load() {
		t.Fatal("队列未关闭")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sources) != 3 {
		t.Fatalf("来源轮次%d", len(sources))
	}
	for _, source := range sources {
		select {
		case <-source.done:
		default:
			t.Fatal("来源仍在运行")
		}
	}
}

func TestRescanFailurePreservesUnknownAndAcceptedSwitchResult(t *testing.T) {
	d, _ := New(&fakeQueue{})
	if err := d.RegisterEventSource(func() Source {
		return &fakeSource{ready: make(chan struct{}), done: make(chan struct{}), fail: errors.New("扫描失败"), value: Baseline{USBKnown: true}}
	}); err != nil {
		t.Fatal(err)
	}
	baseline, err := d.rescan(context.Background())
	if err == nil || baseline.USBKnown || baseline.SessionPresent != nil {
		t.Fatalf("失败被当作已知空值: %+v %v", baseline, err)
	}
	request := model.SwitchRequest{ID: "partial", TargetID: "target"}
	response := d.handle(context.Background(), model.Request{Version: 1, ID: request.ID, Operation: model.OperationSwitch, Switch: &request})
	if response.Error != nil || response.SwitchResult == nil || response.SwitchResult.Error == nil {
		t.Fatalf("覆盖受理结果: %+v", response)
	}
}

func TestCustomEventReachesAutomationAndSwitchUsesOwnedQueue(t *testing.T) {
	q := &fakeQueue{}
	d, _ := New(q)
	received := make(chan error, 1)
	err := d.RegisterAutomation(func(rt AutomationRuntime) (Automation, error) {
		return automationFunc(func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case event := <-rt.Events:
				if event.Name != "desk" {
					received <- errors.New("事件内容丢失")
					return errors.New("事件内容丢失")
				}
				result, err := rt.Switch(ctx, model.SwitchRequest{ID: "rule-switch", TargetID: "target"})
				if err == nil && (result.RequestID != "rule-switch" || result.Error == nil) {
					err = errors.New("自动化未保留队列结果")
				}
				received <- err
			}
			<-ctx.Done()
			return ctx.Err()
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	listener := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	const token = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	go func() { done <- d.Run(ctx, listener, token) }()
	c, err := client.NewWithTransport(model.Endpoint{Version: 1, ProtocolVersion: 1, Network: "tcp4", Address: "127.0.0.1:12345", Token: token, PID: 1, StartedAt: time.Now()}, func(ctx context.Context, _, _ string) (net.Conn, error) {
		server, peer := net.Pipe()
		select {
		case listener.incoming <- server:
			return peer, nil
		case <-ctx.Done():
			server.Close()
			peer.Close()
			return nil, ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EmitEvent(ctx, model.Event{ID: "custom", Kind: model.EventCustom, Source: "cli", Name: "desk", ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-received:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("自动化未收到事件")
	}
	if q.switches.Load() != 1 {
		t.Fatalf("切换未通过唯一队列: %d", q.switches.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("自动化未退出")
	}
}

func TestRescanAndShutdownKeepCleanupFailure(t *testing.T) {
	cleanupError := errors.New("原生订阅关闭失败")
	newSource := func() *fakeSource {
		return &fakeSource{ready: make(chan struct{}), done: make(chan struct{}), value: Baseline{ObservedAt: time.Now(), USBKnown: true}, cleanup: cleanupError}
	}
	d, _ := New(&fakeQueue{})
	if err := d.RegisterEventSource(func() Source { return newSource() }); err != nil {
		t.Fatal(err)
	}
	baseline, err := d.rescan(context.Background())
	if !errors.Is(err, cleanupError) || baseline.USBKnown {
		t.Fatalf("补扫掩盖清理错误: %+v %v", baseline, err)
	}
	source := newSource()
	d, _ = New(&fakeQueue{})
	if err := d.RegisterEventSource(func() Source { return source }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, newPipeListener(), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") }()
	<-source.ready
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, cleanupError) {
			t.Fatalf("退出掩盖清理错误: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("退出阻塞")
	}
}

func TestNoAutomationDoesNotClaimCustomEventsAreConsumed(t *testing.T) {
	d, _ := New(&fakeQueue{})
	for _, capability := range d.eventCapabilities() {
		if capability.Kind == model.EventCustom && (capability.Available || capability.Reason == "") {
			t.Fatalf("未接线却声称可用: %+v", capability)
		}
	}
	if err := d.emit(context.Background(), model.Event{}); model.CodeOf(err) != model.CodeBackendUnavailable {
		t.Fatalf("没有消费者仍接受事件: %v", err)
	}
}
