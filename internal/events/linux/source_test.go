package linux

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

type fakeSubscription struct {
	lines chan string
	ready chan error
	done  chan struct{}
	once  sync.Once
	mu    sync.Mutex
	err   error
}

func newFakeSubscription() *fakeSubscription {
	f := &fakeSubscription{lines: make(chan string, 32), ready: make(chan error, 1), done: make(chan struct{})}
	f.ready <- nil
	return f
}
func (f *fakeSubscription) Lines() <-chan string  { return f.lines }
func (f *fakeSubscription) Ready() <-chan error   { return f.ready }
func (f *fakeSubscription) Done() <-chan struct{} { return f.done }
func (f *fakeSubscription) Err() error            { f.mu.Lock(); defer f.mu.Unlock(); return f.err }
func (f *fakeSubscription) Close() error          { f.once.Do(func() { close(f.done) }); return nil }

type fakeRunner struct {
	runFn   func(context.Context, string, ...string) ([]byte, error)
	startFn func(context.Context, string, []string, readyMode) (subscription, error)
}

func (f fakeRunner) run(c context.Context, p string, a ...string) ([]byte, error) {
	return f.runFn(c, p, a...)
}
func (f fakeRunner) start(c context.Context, p string, a []string, m readyMode) (subscription, error) {
	return f.startFn(c, p, a, m)
}

func testSource(t *testing.T, r commandRunner) *Source {
	t.Helper()
	// 模拟Linux不改变宿主路径规则，合法工具夹具须通过真实绝对路径校验才能进入替身回调。
	s := NewSource(Config{UdevadmPath: filepath.Join(t.TempDir(), "udevadm")})
	s.runner, s.platform = r, "linux"
	s.infrastructure = func() error { return nil }
	s.pause = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return s
}

func usbRunner(f *fakeSubscription, scan func(context.Context) ([]byte, error)) fakeRunner {
	return fakeRunner{
		runFn: func(ctx context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) == 1 && args[0] == "--version" {
				return []byte("258\n"), nil
			}
			return scan(ctx)
		},
		startFn: func(context.Context, string, []string, readyMode) (subscription, error) { return f, nil },
	}
}

const usbDatabase = "P: /devices/usb1/1-2\nE: DEVPATH=/devices/usb1/1-2\nE: SUBSYSTEM=usb\nE: DEVTYPE=usb_device\nE: PRODUCT=1234/5678/100\nE: ID_SERIAL_SHORT=original\n\n"

func awaitSnapshot(t *testing.T, s *Source) Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var snap Snapshot
	for {
		var err error
		snap, err = s.Snapshot(ctx)
		if err == nil {
			return snap
		}
		if model.CodeOf(err) != model.CodeBackendUnavailable {
			t.Fatalf("快照失败: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("等待启动超时")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCapabilitiesMissingPermissionAndUnsupported(t *testing.T) {
	denied := model.NewError(model.CodePermissionDenied, "监控权限不足", nil)
	f := newFakeSubscription()
	r := usbRunner(f, func(context.Context) ([]byte, error) { return nil, nil })
	r.startFn = func(context.Context, string, []string, readyMode) (subscription, error) { return nil, denied }
	s := testSource(t, r)
	caps, err := s.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		if c.Available || c.Reason == "" {
			t.Fatalf("不可用能力没有明确原因: %+v", c)
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if c.Kind == model.EventUSBAdded && !strings.Contains(c.Reason, "permission_denied") {
			t.Fatal(c.Reason)
		}
	}
	s.infrastructure = func() error { return model.NewError(model.CodeDaemonUnavailable, "udev服务缺席", nil) }
	caps, _ = s.Capabilities(context.Background())
	if caps[0].Available || !strings.Contains(caps[0].Reason, "udev服务缺席") {
		t.Fatal(caps)
	}
}

func TestSnapshotFixedBaselineAndCancellation(t *testing.T) {
	f := newFakeSubscription()
	s := testSource(t, usbRunner(f, func(context.Context) ([]byte, error) { return []byte(usbDatabase), nil }))
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan model.Event, 2)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, out) }()
	snap := awaitSnapshot(t, s)
	if !snap.USBKnown || len(snap.USBDevices) != 1 || snap.SessionPresent != nil {
		t.Fatalf("初始状态错误: %+v", snap)
	}
	select {
	case e := <-out:
		t.Fatalf("伪造初始事件: %+v", e)
	default:
	}
	snap.USBDevices[0].Serial = "changed"
	f.lines <- "UDEV  [12.3] remove /devices/usb1/1-2 (usb)"
	f.lines <- "ACTION=remove"
	f.lines <- "DEVPATH=/devices/usb1/1-2"
	f.lines <- "SUBSYSTEM=usb"
	f.lines <- "DEVTYPE=usb_device"
	f.lines <- ""
	select {
	case e := <-out:
		if e.Kind != model.EventUSBRemoved || e.USB.Serial != "original" {
			t.Fatalf("USB移除未复用已知身份: %+v", e)
		}
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到真实增量")
	}
	again, err := s.Snapshot(context.Background())
	if err != nil || len(again.USBDevices) != 1 || again.USBDevices[0].Serial != "original" || !again.ObservedAt.Equal(snap.ObservedAt) {
		t.Fatalf("快照不是固定深副本: %+v %v", again, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("取消原因丢失: %v", err)
	}
	select {
	case <-f.done:
	default:
		t.Fatal("订阅未关闭")
	}
	if _, err := s.Snapshot(context.Background()); err == nil {
		t.Fatal("退出后仍暴露旧轮就绪")
	}
}

func TestSnapshotWaitScanFailureAndContext(t *testing.T) {
	f := newFakeSubscription()
	entered, release := make(chan struct{}), make(chan struct{})
	scanErr := model.NewError(model.CodeIO, "数据库读取失败", nil)
	s := testSource(t, usbRunner(f, func(ctx context.Context) ([]byte, error) {
		close(entered)
		select {
		case <-release:
			return nil, scanErr
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), make(chan model.Event)) }()
	<-entered
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.Snapshot(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("快照等待不响应ctx: %v", err)
	}
	waiting := make(chan error, 1)
	go func() { _, err := s.Snapshot(context.Background()); waiting <- err }()
	close(release)
	if err := <-done; model.CodeOf(err) != model.CodeIO {
		t.Fatalf("初扫错误丢失: %v", err)
	}
	if err := <-waiting; err == nil {
		t.Fatal("初扫失败仍给快照")
	}
	select {
	case <-f.done:
	default:
		t.Fatal("初扫失败未清理")
	}
}

func TestSnapshotReconcilesBufferedUSBAndNoSyntheticEvent(t *testing.T) {
	f := newFakeSubscription()
	s := testSource(t, usbRunner(f, func(context.Context) ([]byte, error) {
		for _, line := range []string{"UDEV [1] add /devices/usb1/1-3 (usb)", "ACTION=add", "DEVPATH=/devices/usb1/1-3", "SUBSYSTEM=usb", "DEVTYPE=usb_device", "PRODUCT=abcd/ef01/1", ""} {
			f.lines <- line
		}
		return []byte(usbDatabase), nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	out, done := make(chan model.Event, 4), make(chan error, 1)
	go func() { done <- s.Run(ctx, out) }()
	snap := awaitSnapshot(t, s)
	// 扫描结束的读取屏障必须消化已经排队的完整记录，纳入基线而非补造新增。
	if !snap.USBKnown || len(snap.USBDevices) != 2 {
		t.Fatal(snap)
	}
	select {
	case event := <-out:
		t.Fatalf("扫描期间记录伪造成新增: %+v", event)
	default:
	}
	cancel()
	<-done
}

func TestScanWaitsForRealSubscriptionReadyAndPermissionFailure(t *testing.T) {
	f := newFakeSubscription()
	f.ready = make(chan error, 1)
	started := make(chan struct{})
	var scans atomic.Int32
	r := usbRunner(f, func(context.Context) ([]byte, error) { scans.Add(1); return nil, nil })
	r.startFn = func(context.Context, string, []string, readyMode) (subscription, error) {
		close(started)
		return f, nil
	}
	s := testSource(t, r)
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), make(chan model.Event)) }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.Snapshot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("未确认就绪便交付快照: %v", err)
	}
	if scans.Load() != 0 {
		t.Fatal("真实订阅就绪前执行了初扫")
	}
	f.ready <- model.NewError(model.CodePermissionDenied, "BecomeMonitor拒绝", nil)
	if err := <-done; model.CodeOf(err) != model.CodePermissionDenied {
		t.Fatalf("握手权限错误丢失: %v", err)
	}
	select {
	case <-f.done:
	default:
		t.Fatal("握手失败未清理")
	}
}

func TestConfirmedEmptySnapshot(t *testing.T) {
	f := newFakeSubscription()
	s := testSource(t, usbRunner(f, func(context.Context) ([]byte, error) { return nil, nil }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, make(chan model.Event)) }()
	snapshot := awaitSnapshot(t, s)
	if !snapshot.USBKnown || len(snapshot.USBDevices) != 0 || snapshot.SessionPresent != nil {
		t.Fatalf("空与未知语义混淆: %+v", snapshot)
	}
	cancel()
	<-done
}

func TestLogindUsesOnlySystemBusAndVerifiesTargetSession(t *testing.T) {
	f := newFakeSubscription()
	r := fakeRunner{
		runFn: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) == 1 && args[0] == "--version" {
				return []byte("systemd 258 (258.1)\n"), nil
			}
			if !strings.Contains(strings.Join(args, " "), "--system") {
				t.Error("logind查询未限定系统总线")
			}
			switch args[len(args)-1] {
			case loginService:
				return []byte(`{"type":"s","data":[":1.9"]}`), nil
			case "ListSessions":
				return []byte(`{"type":"a(susso)","data":[[["c1",1000,"alice","seat0","/org/freedesktop/login1/session/c1"]]]}`), nil
			case "User":
				return []byte(`{"type":"(uo)","data":[[1000,"/org/freedesktop/login1/user/_1000"]]}`), nil
			case "Id":
				return []byte(`{"type":"s","data":["c1"]}`), nil
			default:
				return nil, errors.New("未预期的查询")
			}
		},
		startFn: func(_ context.Context, _ string, args []string, mode readyMode) (subscription, error) {
			if mode != readyNotify || !strings.Contains(strings.Join(args, " "), "--system") {
				t.Error("订阅未使用系统总线与通知握手")
			}
			return f, nil
		},
	}
	s := NewSource(Config{BusctlPath: filepath.Join(t.TempDir(), "busctl"), SessionID: "c1"})
	s.runner, s.platform, s.uid = r, "linux", 1000
	ctx, cancel := context.WithCancel(context.Background())
	out, done := make(chan model.Event, 4), make(chan error, 1)
	go func() { done <- s.Run(ctx, out) }()
	snapshot := awaitSnapshot(t, s)
	if snapshot.USBKnown || snapshot.SessionPresent == nil || !*snapshot.SessionPresent {
		t.Fatal(snapshot)
	}
	*snapshot.SessionPresent = false
	f.lines <- `{"type":"signal","sender":":1.9","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Manager","member":"SessionNew","payload":{"type":"so","data":["other","/org/freedesktop/login1/session/other"]}}`
	f.lines <- `{"type":"signal","sender":":1.9","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Manager","member":"SessionNew","payload":{"type":"so","data":["c1","/org/freedesktop/login1/session/c1"]}}`
	f.lines <- `{"type":"signal","sender":":1.9","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Manager","member":"PrepareForSleep","payload":{"type":"b","data":[true]}}`
	f.lines <- `{"type":"signal","sender":":1.9","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Manager","member":"PrepareForSleep","payload":{"type":"b","data":[false]}}`
	for _, kind := range []model.EventKind{model.EventSessionStarted, model.EventResume} {
		select {
		case event := <-out:
			if event.Kind != kind || event.USB != nil {
				t.Fatal(event)
			}
			if err := event.Validate(); err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("未收到核验后的会话/恢复事件")
		}
	}
	again, err := s.Snapshot(context.Background())
	if err != nil || !*again.SessionPresent {
		t.Fatalf("会话快照未深拷贝: %+v %v", again, err)
	}
	cancel()
	<-done
}

func (f *fakeSubscription) terminate(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
	_ = f.Close()
}

func TestReconnectCleansOldRoundAndWaitsNewReady(t *testing.T) {
	first, second := newFakeSubscription(), newFakeSubscription()
	second.ready = make(chan error, 1)
	var starts, scans atomic.Int32
	paused, continueReconnect, secondStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r := usbRunner(first, func(context.Context) ([]byte, error) {
		n := scans.Add(1)
		if n == 1 {
			return []byte(usbDatabase), nil
		}
		return []byte(strings.ReplaceAll(usbDatabase, "original", "new-round")), nil
	})
	r.startFn = func(context.Context, string, []string, readyMode) (subscription, error) {
		if starts.Add(1) == 1 {
			return first, nil
		}
		close(secondStarted)
		return second, nil
	}
	s := testSource(t, r)
	s.pause = func(ctx context.Context, _ time.Duration) error {
		close(paused)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-continueReconnect:
			return nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, make(chan model.Event)) }()
	original := awaitSnapshot(t, s)
	first.terminate(model.NewError(model.CodeTransport, "连接断开", nil))
	<-paused
	select {
	case <-first.done:
	default:
		t.Fatal("重连前未清理旧订阅")
	}
	close(continueReconnect)
	<-secondStarted
	waitCtx, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if _, err := s.Snapshot(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("重连误用旧轮就绪: %v", err)
	}
	if scans.Load() != 1 {
		t.Fatal("新轮就绪前已扫描")
	}
	second.ready <- nil
	again := awaitSnapshot(t, s)
	if !again.ObservedAt.Equal(original.ObservedAt) || again.USBDevices[0].Serial != "original" {
		t.Fatal("重连改写本Run固定启动快照")
	}
	cancel()
	<-done
	select {
	case <-second.done:
	default:
		t.Fatal("重连后取消未清理")
	}
}

func TestReconnectBudgetIsFinite(t *testing.T) {
	started := make(chan *fakeSubscription, 4)
	var count atomic.Int32
	r := fakeRunner{
		runFn: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) == 1 && args[0] == "--version" {
				return []byte("258\n"), nil
			}
			return nil, nil
		},
		startFn: func(context.Context, string, []string, readyMode) (subscription, error) {
			f := newFakeSubscription()
			count.Add(1)
			started <- f
			return f, nil
		},
	}
	s := testSource(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, make(chan model.Event)) }()
	for i := 0; i < 3; i++ {
		var f *fakeSubscription
		select {
		case f = <-started:
		case <-time.After(time.Second):
			t.Fatal("没有建立预期重连")
		}
		awaitSnapshot(t, s)
		f.terminate(model.NewError(model.CodeTransport, "连接断开", nil))
	}
	select {
	case err := <-done:
		if model.CodeOf(err) != model.CodeTransport || count.Load() != 3 {
			t.Fatalf("重连预算或错误分类不符: %d %v", count.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("重连没有按预算停止")
	}
}

func TestExplicitMissingSessionSnapshotIsFalseRatherThanUnknown(t *testing.T) {
	f := newFakeSubscription()
	r := fakeRunner{
		runFn: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			switch args[len(args)-1] {
			case "--version":
				return []byte("systemd 258\n"), nil
			case loginService:
				return []byte(`{"type":"s","data":[":1.9"]}`), nil
			case "ListSessions":
				return []byte(`{"type":"a(susso)","data":[[]]}`), nil
			default:
				return nil, errors.New("未预期查询")
			}
		},
		startFn: func(context.Context, string, []string, readyMode) (subscription, error) { return f, nil },
	}
	s := NewSource(Config{BusctlPath: filepath.Join(t.TempDir(), "busctl"), SessionID: "not-present"})
	s.runner, s.platform, s.uid = r, "linux", 1000
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, make(chan model.Event)) }()
	snapshot := awaitSnapshot(t, s)
	if snapshot.SessionPresent == nil || *snapshot.SessionPresent || snapshot.USBKnown {
		t.Fatalf("明确缺席被改成未知或USB已知: %+v", snapshot)
	}
	cancel()
	<-done
}
