package windows

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

type testPlatform struct {
	sub      *testSubscription
	probeErr error
	openErr  error
}

func (p *testPlatform) probe(context.Context) error { return p.probeErr }
func (p *testPlatform) open(context.Context) (subscription, error) {
	if p.openErr != nil {
		return nil, p.openErr
	}
	return p.sub, nil
}

type testSubscription struct {
	mu       sync.Mutex
	notices  chan notification
	done     chan struct{}
	once     sync.Once
	scan     func(context.Context) (Snapshot, error)
	failure  error
	closeErr error
	closed   bool
}

func testSub(snapshot Snapshot) *testSubscription {
	return &testSubscription{
		notices: make(chan notification, notificationLimit), done: make(chan struct{}),
		scan: func(context.Context) (Snapshot, error) { return cloneSnapshot(snapshot), nil },
	}
}

func (s *testSubscription) Snapshot(ctx context.Context) (Snapshot, error) { return s.scan(ctx) }
func (s *testSubscription) Notifications() <-chan notification             { return s.notices }
func (s *testSubscription) Done() <-chan struct{}                          { return s.done }
func (s *testSubscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}
func (s *testSubscription) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
	return s.closeErr
}
func (s *testSubscription) push(n notification) {
	if err := enqueueNotification(s.notices, n); err != nil {
		s.mu.Lock()
		s.failure = err
		s.mu.Unlock()
		s.once.Do(func() { close(s.done) })
	}
}

func baseline(devices ...model.USBDevice) Snapshot {
	present := true
	return Snapshot{ObservedAt: time.Unix(100, 0), USBDevices: devices, USBKnown: true, SessionPresent: &present}
}

func startTestSource(t *testing.T, sub *testSubscription) (*Source, context.CancelFunc, <-chan error, chan model.Event) {
	t.Helper()
	s := newSource(&testPlatform{sub: sub}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan model.Event, 16)
	result := make(chan error, 1)
	go func() { result <- s.Run(ctx, out) }()
	t.Cleanup(cancel)
	return s, cancel, result, out
}

func awaitSnapshot(t *testing.T, s *Source) Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("等待启动快照: %v", err)
	}
	return snapshot
}

func awaitRun(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Run未在资源回收后退出")
		return nil
	}
}

func nextEvent(t *testing.T, out <-chan model.Event) model.Event {
	t.Helper()
	select {
	case event := <-out:
		if err := event.Validate(); err != nil {
			t.Fatalf("未产生03规范事件: %v", err)
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("未收到增量事件")
		return model.Event{}
	}
}

func TestStartupSnapshotReconcilesWithoutSyntheticEvents(t *testing.T) {
	a := model.USBDevice{VendorID: 0x1234, ProductID: 0x5678, Path: "opaque-A"}
	b := model.USBDevice{VendorID: 0x1234, ProductID: 0x5678, Path: "opaque-B"}
	sub := testSub(baseline())
	var calls int
	sub.scan = func(context.Context) (Snapshot, error) {
		calls++
		if calls == 1 {
			// 模拟订阅已启用、初扫尚未结束时的设备变化，必须并入基线。
			sub.push(notification{kind: noticeUSB, observedAt: time.Unix(101, 0)})
			return baseline(a), nil
		}
		return baseline(b), nil
	}
	s, cancel, result, out := startTestSource(t, sub)
	snap := awaitSnapshot(t, s)
	if len(snap.USBDevices) != 1 || snap.USBDevices[0] != b || calls != 2 {
		t.Fatalf("未对账初扫期间的通知: %+v，扫描次数=%d", snap, calls)
	}
	snap.USBDevices[0].Path = "mutated"
	*snap.SessionPresent = false
	copy := awaitSnapshot(t, s)
	if copy.USBDevices[0] != b || !*copy.SessionPresent {
		t.Fatal("Snapshot泄漏了可变切片或会话指针")
	}
	select {
	case e := <-out:
		t.Fatalf("启动基线被伪造成事件: %+v", e)
	default:
	}
	cancel()
	if err := awaitRun(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消原因丢失: %v", err)
	}
}

func TestSnapshotPreservesUnknownAndKnownEmpty(t *testing.T) {
	absent := false
	for _, snapshot := range []Snapshot{
		{ObservedAt: time.Unix(1, 0)},
		{ObservedAt: time.Unix(1, 0), USBKnown: true, USBDevices: []model.USBDevice{}, SessionPresent: &absent},
	} {
		sub := testSub(snapshot)
		s, cancel, result, _ := startTestSource(t, sub)
		got := awaitSnapshot(t, s)
		if !reflect.DeepEqual(got, snapshot) {
			t.Fatalf("未知和已知空被混淆: got=%+v want=%+v", got, snapshot)
		}
		cancel()
		awaitRun(t, result)
	}
}

func TestUSBIncrementalIdentityAndRepeatedRemoval(t *testing.T) {
	a := model.USBDevice{VendorID: 1, ProductID: 2, Path: "opaque-A"}
	b := model.USBDevice{VendorID: 1, ProductID: 2, Path: "opaque-B"}
	sub := testSub(baseline())
	scans := make(chan Snapshot, 4)
	scans <- baseline()
	scans <- baseline(a, b)
	scans <- baseline(b)
	scans <- baseline(b)
	sub.scan = func(ctx context.Context) (Snapshot, error) {
		select {
		case snapshot := <-scans:
			return snapshot, nil
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
	}
	s, cancel, result, out := startTestSource(t, sub)
	original := awaitSnapshot(t, s)
	sub.push(notification{kind: noticeUSB, observedAt: time.Unix(101, 0)})
	first, second := nextEvent(t, out), nextEvent(t, out)
	if first.Kind != model.EventUSBAdded || second.Kind != model.EventUSBAdded || first.USB.Path == second.USB.Path {
		t.Fatalf("相同VID/PID的不同设备被合并: %+v %+v", first, second)
	}
	sub.push(notification{kind: noticeUSB, observedAt: time.Unix(102, 0)})
	removed := nextEvent(t, out)
	if removed.Kind != model.EventUSBRemoved || *removed.USB != a {
		t.Fatalf("移除身份不准确: %+v", removed)
	}
	sub.push(notification{kind: noticeUSB, observedAt: time.Unix(103, 0)})
	sub.push(notification{kind: noticeResume, observedAt: time.Unix(104, 0)})
	if next := nextEvent(t, out); next.Kind != model.EventResume {
		t.Fatalf("重复移除产生了多余事件: %+v", next)
	}
	if after := awaitSnapshot(t, s); !reflect.DeepEqual(after, original) {
		t.Fatal("后续增量改变了固定启动基线")
	}
	cancel()
	awaitRun(t, result)
}

func TestHardwareIdentityAndUSBSelection(t *testing.T) {
	for _, text := range []string{`USB\VID_1234&PID_aBcD`, `usb\vid_1234&pid_ABCD&REV_0200`} {
		vid, pid, ok := parseHardwareID(text)
		if !ok || vid != 0x1234 || pid != 0xabcd {
			t.Fatalf("标准硬件ID解析失败: %s", text)
		}
	}
	for _, text := range []string{`USB\VID_12345&PID_0001`, `USB\VID_ZZZZ&PID_0001`, `USB\VID_0001`, `PCI\VID_1234&PID_ABCD`, `USB\VID_1234&VID_4321&PID_ABCD`, `\\?\usb#vid_1234&pid_abcd#port-number`} {
		if _, _, ok := parseHardwareID(text); ok {
			t.Fatalf("无效属性或不透明设备路径被误当硬件ID: %s", text)
		}
	}
	vid, pid := uint16(0x1234), uint16(0xabcd)
	filter := USBFilter{VendorID: &vid, ProductID: &pid, Path: "OPAQUE-PATH"}
	device := model.USBDevice{VendorID: vid, ProductID: pid, Path: "opaque-path"}
	if !matchesUSB(device, []USBFilter{filter}) {
		t.Fatal("精确字段筛选未匹配")
	}
	device.Path += "-other"
	if matchesUSB(device, []USBFilter{filter}) {
		t.Fatal("路径筛选错误使用了子串匹配")
	}
}

func TestSessionAndPowerMessageClassification(t *testing.T) {
	for _, code := range []uint32{1, 2, 3, 4, 7, 8, 9, 10} {
		if _, ok := classifySession(code, 4, 4); ok {
			t.Fatalf("锁屏、解锁或连接状态被当成登录: %d", code)
		}
	}
	if kind, ok := classifySession(5, 4, 4); !ok || kind != noticeLogin {
		t.Fatal("真实当前会话登录未识别")
	}
	if _, ok := classifySession(5, 8, 4); ok {
		t.Fatal("其他会话登录泄漏到当前会话")
	}
	if kind, ok := classifySession(6, 4, 4); !ok || kind != noticeLogoff {
		t.Fatal("当前会话注销未识别")
	}
	if !isAutomaticResume(0x12) || isAutomaticResume(7) || isAutomaticResume(6) {
		t.Fatal("同一次恢复的追加通知可能重复输出")
	}
}

func TestRealLoginAfterBaselineIsNotSuppressedByPresentSession(t *testing.T) {
	sub := testSub(baseline())
	s, cancel, result, out := startTestSource(t, sub)
	awaitSnapshot(t, s)
	sub.push(notification{kind: noticeLogin, observedAt: time.Unix(110, 0)})
	if event := nextEvent(t, out); event.Kind != model.EventSessionStarted {
		t.Fatalf("真实登录被当前会话存在状态抑制: %+v", event)
	}
	cancel()
	awaitRun(t, result)
}

func TestSourceFilterCopiesConfigurationAndAppliesToSnapshot(t *testing.T) {
	vid := uint16(1)
	filters := []USBFilter{{VendorID: &vid}}
	sub := testSub(baseline(model.USBDevice{VendorID: 1, Path: "chosen"}, model.USBDevice{VendorID: 2, Path: "other"}))
	s := newSource(&testPlatform{sub: sub}, filters)
	vid = 2
	filters[0].Path = "other"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- s.Run(ctx, make(chan model.Event)) }()
	snapshot := awaitSnapshot(t, s)
	if len(snapshot.USBDevices) != 1 || snapshot.USBDevices[0].Path != "chosen" {
		t.Fatalf("来源筛选未生效或引用了外部可变配置: %+v", snapshot)
	}
	cancel()
	awaitRun(t, result)
}

func TestCancelBlockedOutputWaitsForCleanupAndKeepsChannelOwnedByCaller(t *testing.T) {
	sub := testSub(baseline())
	sub.notices = make(chan notification)
	s := newSource(&testPlatform{sub: sub}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan model.Event)
	result := make(chan error, 1)
	go func() { result <- s.Run(ctx, out) }()
	awaitSnapshot(t, s)
	// 无缓冲通知交接证明Run已领取该通知；输出也无缓冲且无读者，随后取消
	// 必须解除发送等待，不能仅验证取消发生在尚未读取通知的分支。
	select {
	case sub.notices <- notification{kind: noticeResume, observedAt: time.Now()}:
	case <-time.After(2 * time.Second):
		t.Fatal("Run未领取恢复通知")
	}
	cancel()
	if err := awaitRun(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("阻塞发送不能取消: %v", err)
	}
	sub.mu.Lock()
	closed := sub.closed
	sub.mu.Unlock()
	if !closed {
		t.Fatal("Run先于资源释放返回")
	}
	close(out)
}

func TestInitialScanAndCleanupFailureWakeSnapshotWaiters(t *testing.T) {
	scanErr, cleanupErr := errors.New("initial scan failed"), errors.New("cleanup failed")
	entered, release := make(chan struct{}), make(chan struct{})
	sub := testSub(baseline())
	sub.closeErr = cleanupErr
	sub.scan = func(context.Context) (Snapshot, error) { close(entered); <-release; return Snapshot{}, scanErr }
	s, _, result, _ := startTestSource(t, sub)
	<-entered
	waiting := make(chan error, 1)
	go func() { _, err := s.Snapshot(context.Background()); waiting <- err }()
	close(release)
	if err := awaitRun(t, result); !errors.Is(err, scanErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("初扫或清理根因被吞掉: %v", err)
	}
	if err := awaitRun(t, waiting); !errors.Is(err, scanErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("快照等待者未获知启动失败: %v", err)
	}
}

func TestSnapshotCancellationAndRestartDoNotReturnOldBaseline(t *testing.T) {
	sub := testSub(baseline(model.USBDevice{Path: "old"}))
	s, cancel, firstRun, _ := startTestSource(t, sub)
	awaitSnapshot(t, s)
	cancel()
	awaitRun(t, firstRun)
	if _, err := s.Snapshot(context.Background()); err == nil {
		t.Fatal("结束的Run仍返回旧基线")
	}
	entered := make(chan struct{})
	second := testSub(baseline())
	second.scan = func(ctx context.Context) (Snapshot, error) {
		close(entered)
		<-ctx.Done()
		return Snapshot{}, ctx.Err()
	}
	s.platform = &testPlatform{sub: second}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	result := make(chan error, 1)
	go func() { result <- s.Run(ctx, make(chan model.Event)) }()
	<-entered
	waitCtx, abortWait := context.WithCancel(context.Background())
	abortWait()
	if _, err := s.Snapshot(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("新一轮未就绪时未响应取消，或返回旧基线: %v", err)
	}
	stop()
	awaitRun(t, result)
	neverStarted := newSource(&testPlatform{sub: testSub(baseline())}, nil)
	if _, err := neverStarted.Snapshot(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("首次Run前等待不支持取消: %v", err)
	}
}

func TestOpenFailureAndConcurrentRun(t *testing.T) {
	openErr := errors.New("subscription failed")
	s := newSource(&testPlatform{openErr: openErr}, nil)
	if err := s.Run(context.Background(), make(chan model.Event)); !errors.Is(err, openErr) {
		t.Fatalf("订阅失败被改为静默成功: %v", err)
	}
	if _, err := s.Snapshot(context.Background()); !errors.Is(err, openErr) {
		t.Fatalf("订阅失败未唤醒快照: %v", err)
	}
	s, cancel, result, _ := startTestSource(t, testSub(baseline()))
	awaitSnapshot(t, s)
	if err := s.Run(context.Background(), make(chan model.Event)); model.CodeOf(err) != model.CodeInvalidRequest {
		t.Fatalf("同一个来源允许并发Run: %v", err)
	}
	cancel()
	awaitRun(t, result)
}

func TestNotificationOverflowFailsInsteadOfDropping(t *testing.T) {
	sub := testSub(baseline())
	for i := 0; i <= notificationLimit; i++ {
		sub.push(notification{kind: noticeUSB})
	}
	if model.CodeOf(sub.Err()) != model.CodeIO {
		t.Fatal("通知缓冲区溢出未显式报错")
	}
	s, _, result, _ := startTestSource(t, sub)
	if err := awaitRun(t, result); model.CodeOf(err) != model.CodeIO {
		t.Fatalf("溢出后来源未终止并回收: %v", err)
	}
	if _, err := s.Snapshot(context.Background()); err == nil {
		t.Fatal("溢出后仍发布可信快照")
	}
}

func TestContinuousChangesCannotKeepInitialReconciliationUnbounded(t *testing.T) {
	sub := testSub(baseline())
	sub.scan = func(context.Context) (Snapshot, error) {
		sub.push(notification{kind: noticeUSB, observedAt: time.Now()})
		return baseline(), nil
	}
	s, _, result, _ := startTestSource(t, sub)
	if err := awaitRun(t, result); model.CodeOf(err) != model.CodeIO {
		t.Fatalf("持续变化未在初始对账上限处明确失败: %v", err)
	}
	if _, err := s.Snapshot(context.Background()); err == nil {
		t.Fatal("无法对账的启动轮仍返回了快照")
	}
}

func TestCapabilitiesAndUnsupportedPlatform(t *testing.T) {
	s := newSource(&testPlatform{sub: testSub(baseline())}, nil)
	caps, err := s.Capabilities(context.Background())
	if err != nil || len(caps) != 6 {
		t.Fatalf("能力目录错误: %+v %v", caps, err)
	}
	for _, cap := range caps {
		if err := cap.Validate(); err != nil {
			t.Fatal(err)
		}
		if cap.Kind == model.EventCustom || cap.Kind == model.EventTimer {
			if cap.Available || cap.Reason == "" {
				t.Fatal("其他来源能力被虚报")
			}
		}
		for _, field := range cap.FilterFields {
			if field == "usb.serial" {
				t.Fatal("无法可靠读取序列号却宣称支持")
			}
		}
	}
	caps[0].FilterFields[0] = "mutated"
	again, err := s.Capabilities(context.Background())
	if err != nil || again[0].FilterFields[0] != "usb.vendor_id" {
		t.Fatal("能力目录泄漏了共享切片")
	}
	probeErr := model.NewError(model.CodePermissionDenied, "denied", nil)
	failed := newSource(&testPlatform{probeErr: probeErr}, nil)
	unavailable, err := failed.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cap := range unavailable {
		if cap.Available || cap.Reason == "" {
			t.Fatalf("系统探测失败后虚报可用: %+v", cap)
		}
	}
	if runtime.GOOS != "windows" {
		s = New()
		caps, err = s.Capabilities(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, cap := range caps {
			if cap.Available || cap.Reason == "" {
				t.Fatalf("非Windows平台虚报能力: %+v", cap)
			}
		}
		if err := s.Run(context.Background(), make(chan model.Event)); model.CodeOf(err) != model.CodeBackendUnavailable {
			t.Fatalf("非Windows缺少明确错误: %v", err)
		}
	}
}
