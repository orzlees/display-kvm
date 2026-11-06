package darwin

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

type fakeSubscription struct {
	devices     []nativeDevice
	initial     []notification
	scanErr     error
	scanStarted chan struct{}
	scanGate    <-chan struct{}
	pollStarted chan struct{}
	pollOnce    sync.Once
	changes     chan []notification
	failures    chan error
	closed      chan struct{}
	closeOnce   sync.Once
	closeCount  atomic.Int32
	usb         bool
	resume      bool
}

func newFakeSubscription() *fakeSubscription {
	return &fakeSubscription{
		usb: true, resume: true,
		scanStarted: make(chan struct{}),
		pollStarted: make(chan struct{}),
		changes:     make(chan []notification, 8), failures: make(chan error, 1),
		closed: make(chan struct{}),
	}
}

func (f *fakeSubscription) capabilities() []model.EventCapability {
	return eventCapabilities(f.usb, f.resume, "模拟USB来源不可用", "模拟电源来源不可用")
}

func (f *fakeSubscription) scan(ctx context.Context) ([]nativeDevice, error) {
	close(f.scanStarted)
	if f.scanGate != nil {
		select {
		case <-f.scanGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return append([]nativeDevice(nil), f.devices...), f.scanErr
}

func (f *fakeSubscription) poll(ctx context.Context, wait time.Duration) ([]notification, error) {
	if wait == 0 {
		initial := f.initial
		f.initial = nil
		return initial, nil
	}
	f.pollOnce.Do(func() { close(f.pollStarted) })
	select {
	case changes := <-f.changes:
		return changes, nil
	case err := <-f.failures:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeSubscription) close() error {
	f.closeCount.Add(1)
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func fakeSource(f *fakeSubscription) *Source {
	s := New()
	s.open = func(context.Context) (subscription, error) { return f, nil }
	return s
}

func startRun(t *testing.T, s *Source, out chan<- model.Event) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { result <- s.Run(ctx, out) }()
	return cancel, result
}

func runResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Run未在取消或失败后结束")
		return nil
	}
}

func getSnapshot(t *testing.T, s *Source) Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot失败: %v", err)
	}
	return snapshot
}

func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("模拟来源未进入预期阶段")
	}
}

func getEvent(t *testing.T, out <-chan model.Event) model.Event {
	t.Helper()
	select {
	case event := <-out:
		if err := event.Validate(); err != nil {
			t.Fatalf("输出不符合共享事件契约: %v", err)
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("未收到真实增量事件")
		return model.Event{}
	}
}

func usbDevice(id uint64, serial string) nativeDevice {
	return nativeDevice{ID: id, USB: model.USBDevice{VendorID: 1, ProductID: 2, Serial: serial, Path: "usb-path"}}
}

func change(kind model.EventKind, device nativeDevice) notification {
	return notification{Kind: kind, Device: device, ObservedAt: time.Now().UTC()}
}

func TestSnapshotReconcilesStartupAndReturnsFixedDeepCopy(t *testing.T) {
	f := newFakeSubscription()
	f.devices = []nativeDevice{usbDevice(1, "same"), usbDevice(2, "same"), usbDevice(3, "")}
	f.initial = []notification{
		change(model.EventUSBAdded, usbDevice(1, "same")),
		change(model.EventUSBRemoved, nativeDevice{ID: 3}),
		change(model.EventUSBAdded, usbDevice(4, "")),
	}
	s := fakeSource(f)
	out := make(chan model.Event, 8)
	cancel, result := startRun(t, s, out)
	baseline := getSnapshot(t, s)
	if !baseline.USBKnown || baseline.ObservedAt.IsZero() || baseline.SessionPresent != nil || len(baseline.USBDevices) != 3 {
		t.Fatalf("启动基线错误: %+v", baseline)
	}
	if baseline.USBDevices[0].Serial != "same" || baseline.USBDevices[1].Serial != "same" || baseline.USBDevices[2].Serial != "" {
		t.Fatalf("重复或空序列号被错误合并: %+v", baseline.USBDevices)
	}
	select {
	case event := <-out:
		t.Fatalf("启动设备被伪装成增量: %+v", event)
	default:
	}
	baseline.USBDevices[0].Path = "mutated"
	f.changes <- []notification{change(model.EventUSBAdded, usbDevice(5, "later"))}
	if event := getEvent(t, out); event.Kind != model.EventUSBAdded || event.USB.Serial != "later" {
		t.Fatalf("真实新增事件错误: %+v", event)
	}
	second := getSnapshot(t, s)
	if !second.ObservedAt.Equal(baseline.ObservedAt) || len(second.USBDevices) != 3 || second.USBDevices[0].Path != "usb-path" {
		t.Fatalf("快照被外部修改或真实增量改写: %+v", second)
	}
	cancel()
	if err := runResult(t, result); !errors.Is(err, context.Canceled) || f.closeCount.Load() != 1 {
		t.Fatalf("取消未保留根因或未释放唯一订阅: %v, %d", err, f.closeCount.Load())
	}
	if _, err := s.Snapshot(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run结束后仍返回旧基线: %v", err)
	}
}

func TestDuplicateNotificationsUseRegistryInstanceAndRemovalCache(t *testing.T) {
	f := newFakeSubscription()
	s := fakeSource(f)
	out := make(chan model.Event, 8)
	cancel, result := startRun(t, s, out)
	baseline := getSnapshot(t, s)
	if !baseline.USBKnown || len(baseline.USBDevices) != 0 {
		t.Fatalf("成功的空枚举没有表示已知无USB: %+v", baseline)
	}
	device := usbDevice(7, "cached")
	f.changes <- []notification{
		change(model.EventUSBAdded, device), change(model.EventUSBAdded, device),
		change(model.EventUSBRemoved, nativeDevice{ID: 7}), change(model.EventUSBRemoved, nativeDevice{ID: 7}),
		change(model.EventResume, nativeDevice{}),
	}
	added, removed, resumed := getEvent(t, out), getEvent(t, out), getEvent(t, out)
	if added.Kind != model.EventUSBAdded || removed.Kind != model.EventUSBRemoved || removed.USB == nil || *removed.USB != device.USB || resumed.Kind != model.EventResume || resumed.USB != nil {
		t.Fatalf("增删去重、移除缓存或唤醒错误: %+v %+v %+v", added, removed, resumed)
	}
	if added.ID == removed.ID || removed.ID == resumed.ID {
		t.Fatal("不同事实复用了事件ID")
	}
	cancel()
	if err := runResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSnapshotWaitCancellationAndRunFailureWakeWaiters(t *testing.T) {
	s := fakeSource(newFakeSubscription())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("未运行时Snapshot不能取消: %v", err)
	}
	f := newFakeSubscription()
	gate := make(chan struct{})
	f.scanGate = gate
	f.scanErr = errors.New("初扫失败")
	s = fakeSource(f)
	_, result := startRun(t, s, make(chan model.Event))
	awaitSignal(t, f.scanStarted)
	waiter := make(chan error, 1)
	go func() { _, err := s.Snapshot(context.Background()); waiter <- err }()
	close(gate)
	if err := runResult(t, result); !errors.Is(err, f.scanErr) || f.closeCount.Load() != 1 {
		t.Fatalf("初扫失败未使Run失败清理: %v", err)
	}
	if err := runResult(t, waiter); !errors.Is(err, f.scanErr) {
		t.Fatalf("等待者没有收到初扫根因: %v", err)
	}
}

func TestCancellationUnblocksConsumerAndEndsCallbacks(t *testing.T) {
	f := newFakeSubscription()
	s := fakeSource(f)
	out := make(chan model.Event)
	cancel, result := startRun(t, s, out)
	getSnapshot(t, s)
	f.changes <- []notification{change(model.EventUSBAdded, usbDevice(1, "blocked"))}
	cancel()
	if err := runResult(t, result); !errors.Is(err, context.Canceled) || f.closeCount.Load() != 1 {
		t.Fatalf("阻塞消费者阻止取消或清理: %v", err)
	}
	f.changes <- []notification{change(model.EventUSBAdded, usbDevice(2, "late"))}
	select {
	case event := <-out:
		t.Fatalf("Run返回后继续发送: %+v", event)
	default:
	}
	select {
	case _, ok := <-out:
		if !ok {
			t.Fatal("来源关闭了调用者通道")
		}
	default:
	}
}

func TestQueueOverflowFailsInsteadOfBlockingOrDroppingSilently(t *testing.T) {
	f := newFakeSubscription()
	s := fakeSource(f)
	_, result := startRun(t, s, make(chan model.Event))
	getSnapshot(t, s)
	batch := make([]notification, deliveryCapacity+2)
	for i := range batch {
		batch[i] = change(model.EventUSBAdded, usbDevice(uint64(i+1), ""))
	}
	f.changes <- batch
	if err := runResult(t, result); model.CodeOf(err) != model.CodeTransport || f.closeCount.Load() != 1 {
		t.Fatalf("队列溢出没有终止并释放来源: %v", err)
	}
}

func TestRestartReplacesBaselineAndUnsupportedSessionStaysUnknown(t *testing.T) {
	first, second := newFakeSubscription(), newFakeSubscription()
	first.devices = []nativeDevice{usbDevice(1, "old")}
	second.usb = false
	s := New()
	var opens atomic.Int32
	s.open = func(context.Context) (subscription, error) {
		if opens.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}
	cancel, result := startRun(t, s, make(chan model.Event))
	getSnapshot(t, s)
	cancel()
	runResult(t, result)
	cancel, result = startRun(t, s, make(chan model.Event))
	// 第二轮已进入原生轮询后再读取；不能误把前一轮结束状态当作新基线。
	awaitSignal(t, second.pollStarted)
	baseline := getSnapshot(t, s)
	if baseline.USBKnown || len(baseline.USBDevices) != 0 || baseline.SessionPresent != nil {
		t.Fatalf("不可用来源使用了旧轮缓存或把未知当不存在: %+v", baseline)
	}
	cancel()
	runResult(t, result)
}

func TestCapabilitiesExplainUnavailableKindsAndReleaseProbe(t *testing.T) {
	f := newFakeSubscription()
	f.usb = false
	s := fakeSource(f)
	caps, err := s.Capabilities(context.Background())
	if err != nil || len(caps) != 6 || f.closeCount.Load() != 1 {
		t.Fatalf("能力探测或资源释放错误: %+v %v", caps, err)
	}
	for _, capability := range caps {
		if err := capability.Validate(); err != nil {
			t.Fatal(err)
		}
		if capability.Kind == model.EventSessionStarted && (capability.Available || capability.Reason == "") {
			t.Fatal("不能把控制台切入或解锁当作新登录")
		}
		if capability.Kind == model.EventUSBAdded && (capability.Available || capability.Reason == "") {
			t.Fatal("USB不可用没有保留原因")
		}
	}
}

func TestSnapshotOwnDeadlineDoesNotCancelRun(t *testing.T) {
	f := newFakeSubscription()
	gate := make(chan struct{})
	f.scanGate = gate
	s := fakeSource(f)
	cancel, result := startRun(t, s, make(chan model.Event))
	awaitSignal(t, f.scanStarted)
	ctx, stopWaiting := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stopWaiting()
	if _, err := s.Snapshot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("快照等待没有响应独立截止时间: %v", err)
	}
	close(gate)
	// 编译时同时检查匿名结构体别名，供三个平台的调用方直接使用同形返回值。
	var baseline struct {
		ObservedAt     time.Time
		USBDevices     []model.USBDevice
		USBKnown       bool
		SessionPresent *bool
	} = getSnapshot(t, s)
	if !baseline.USBKnown {
		t.Fatal("快照等待超时错误地结束了Run")
	}
	cancel()
	runResult(t, result)
}

func TestInitializationCancellationAndPermissionFailureWakeSnapshot(t *testing.T) {
	f := newFakeSubscription()
	f.scanGate = make(chan struct{})
	s := fakeSource(f)
	cancel, result := startRun(t, s, make(chan model.Event))
	awaitSignal(t, f.scanStarted)
	cancel()
	if err := runResult(t, result); !errors.Is(err, context.Canceled) || f.closeCount.Load() != 1 {
		t.Fatalf("初始化取消没有清理: %v", err)
	}
	s = New()
	denied := model.NewError(model.CodePermissionDenied, "模拟注册权限不足", nil)
	s.open = func(context.Context) (subscription, error) { return nil, denied }
	_, result = startRun(t, s, make(chan model.Event))
	if err := runResult(t, result); !errors.Is(err, denied) {
		t.Fatalf("原生注册错误被吞掉: %v", err)
	}
	if _, err := s.Snapshot(context.Background()); !errors.Is(err, denied) {
		t.Fatalf("快照没有保留本轮初始化错误: %v", err)
	}
}

func TestStartupResumeRemainsRealEventAndConcurrentRunIsRejected(t *testing.T) {
	f := newFakeSubscription()
	resumed := change(model.EventResume, nativeDevice{})
	f.initial = []notification{resumed}
	s := fakeSource(f)
	out := make(chan model.Event, 1)
	cancel, result := startRun(t, s, out)
	getSnapshot(t, s)
	event := getEvent(t, out)
	if event.Kind != model.EventResume || !event.ObservedAt.Equal(resumed.ObservedAt) {
		t.Fatalf("启动对账丢失或伪造了实际唤醒事实: %+v", event)
	}
	if err := s.Run(context.Background(), out); model.CodeOf(err) != model.CodeInvalidRequest {
		t.Fatalf("并发Run没有被拒绝: %v", err)
	}
	cancel()
	runResult(t, result)
}

func TestUnavailableSourcesFailAndWakeSnapshotWithoutBaseline(t *testing.T) {
	f := newFakeSubscription()
	f.usb, f.resume = false, false
	s := fakeSource(f)
	_, result := startRun(t, s, make(chan model.Event))
	if err := runResult(t, result); model.CodeOf(err) != model.CodeBackendUnavailable || f.closeCount.Load() != 1 {
		t.Fatalf("完全不可用时没有明确失败清理: %v", err)
	}
	if snapshot, err := s.Snapshot(context.Background()); err == nil || snapshot.USBKnown {
		t.Fatalf("没有可用来源却生成了已知基线: %+v %v", snapshot, err)
	}
}

func TestNativePollFailureInvalidatesCurrentSnapshot(t *testing.T) {
	f := newFakeSubscription()
	s := fakeSource(f)
	_, result := startRun(t, s, make(chan model.Event))
	getSnapshot(t, s)
	failure := model.NewError(model.CodeTransport, "模拟原生队列溢出", nil)
	f.failures <- failure
	if err := runResult(t, result); !errors.Is(err, failure) || f.closeCount.Load() != 1 {
		t.Fatalf("原生故障未保留或释放: %v", err)
	}
	if _, err := s.Snapshot(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("原生故障后仍泄漏旧基线: %v", err)
	}
}
