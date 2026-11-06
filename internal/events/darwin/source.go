// Package darwin接入macOS的USB及完整唤醒通知；不实现规则筛选或显示器操作。
package darwin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/events"
	"github.com/orzlee/displayKvm/internal/model"
)

// Snapshot使用匿名结构体别名，保证各平台的同形启动快照可直接赋值。
// USBKnown为false表示未知而非没有设备；本来源不能可靠识别新登录，会话值始终未知。
type Snapshot = struct {
	ObservedAt     time.Time
	USBDevices     []model.USBDevice
	USBKnown       bool
	SessionPresent *bool
}

const deliveryCapacity = 64

type nativeDevice struct {
	ID  uint64
	USB model.USBDevice
}

type notification struct {
	Kind       model.EventKind
	Device     nativeDevice
	ObservedAt time.Time
}

// subscription只由持有原生事件循环的线程调用。原生回调不持有Go指针，
// 扫描期间的通知在原生侧有界缓存，poll在扫描完成后取出它们进行启动对账。
type subscription interface {
	capabilities() []model.EventCapability
	scan(context.Context) ([]nativeDevice, error)
	poll(context.Context, time.Duration) ([]notification, error)
	close() error
}

type runState struct {
	active   bool
	ready    bool
	changed  chan struct{}
	baseline Snapshot
	err      error
}

// Source每轮Run拥有独立原生订阅及启动基线，允许结束后重新运行，不允许并发Run。
// Snapshot和Run之间仅通过锁保护的本轮状态交接，不能复用已经结束的旧设备快照。
type Source struct {
	mu      sync.Mutex
	changed chan struct{}
	current *runState
	open    func(context.Context) (subscription, error)
}

var _ events.EventSource = (*Source)(nil)

func New() *Source { return &Source{open: openNative, changed: make(chan struct{})} }

func (s *Source) Name() string { return "darwin" }

func (s *Source) factory() func(context.Context) (subscription, error) {
	if s.open != nil {
		return s.open
	}
	return openNative
}

func (s *Source) Capabilities(ctx context.Context) (capabilities []model.EventCapability, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 能力探测也按原生线程所有权创建并注销临时订阅，不留下持续回调。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	listener, err := s.factory()(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, listener.close()) }()
	capabilities = listener.capabilities()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return capabilities, nil
}

func (s *Source) Snapshot(ctx context.Context) (Snapshot, error) {
	var attempt *runState
	for {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		s.mu.Lock()
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		wait := s.changed
		if attempt == nil {
			attempt = s.current
		}
		// 已经等待某一轮的调用必须收到该轮结果，不能在快速重启后悄悄跳到下一轮。
		if state := attempt; state != nil {
			if !state.active {
				err := state.err
				s.mu.Unlock()
				return Snapshot{}, err
			}
			if state.ready {
				baseline := cloneSnapshot(state.baseline)
				s.mu.Unlock()
				return baseline, nil
			}
			wait = state.changed
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		case <-wait:
		}
	}
}

func cloneSnapshot(value Snapshot) Snapshot {
	value.USBDevices = append([]model.USBDevice(nil), value.USBDevices...)
	if value.SessionPresent != nil {
		present := *value.SessionPresent
		value.SessionPresent = &present
	}
	return value
}

func (s *Source) Run(ctx context.Context, out chan<- model.Event) (err error) {
	s.mu.Lock()
	if s.current != nil && s.current.active {
		s.mu.Unlock()
		return model.NewError(model.CodeInvalidRequest, "macOS事件来源不能并发运行", nil)
	}
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	state := &runState{active: true, changed: make(chan struct{})}
	s.current = state
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()

	workerCtx, cancel := context.WithCancel(ctx)
	queue := make(chan model.Event, deliveryCapacity)
	done := make(chan struct{})
	var workerErr error
	go func() {
		workerErr = s.listen(workerCtx, state, queue)
		close(done)
	}()
	defer func() {
		cancel()
		// 即使消费者不再读取，也必须等原生线程注销所有回调后才结束本轮。
		<-done
		if workerErr != nil && !errors.Is(err, workerErr) {
			err = errors.Join(err, workerErr)
		}
		if err == nil {
			err = model.NewError(model.CodeBackendUnavailable, "macOS事件监听已经结束", nil)
		}
		s.mu.Lock()
		state.active, state.err = false, err
		state.baseline = Snapshot{}
		if !state.ready {
			close(state.changed)
		}
		s.mu.Unlock()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return workerErr
		case event := <-queue:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				return workerErr
			case out <- event:
			}
		}
	}
}

func (s *Source) listen(ctx context.Context, state *runState, out chan<- model.Event) (err error) {
	// IOKit回调在此线程的RunLoop中串行执行；Go迁移协程到其他线程会使
	// 回调、迭代器和注销不再归同一所有者，故从注册到清理全程固定线程。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ctx.Err(); err != nil {
		return err
	}
	listener, err := s.factory()(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, listener.close()) }()
	capabilities := listener.capabilities()
	usb, resume := false, false
	for _, capability := range capabilities {
		if err := capability.Validate(); err != nil {
			return err
		}
		usb = usb || capability.Kind == model.EventUSBAdded && capability.Available
		resume = resume || capability.Kind == model.EventResume && capability.Available
	}
	if !usb && !resume {
		return model.NewError(model.CodeBackendUnavailable, "macOS没有可用的USB或电源事件来源，请检查能力原因", nil)
	}
	devices := make(map[uint64]model.USBDevice)
	if usb {
		scanned, err := listener.scan(ctx)
		if err != nil {
			return err
		}
		for _, device := range scanned {
			if device.ID == 0 {
				return model.NewError(model.CodeTransport, "USB初扫缺少注册表实例编号", nil)
			}
			devices[device.ID] = device.USB
		}
	}
	initial, err := listener.poll(ctx, 0)
	if err != nil {
		return err
	}
	// 初扫并非硬件事务：订阅已启用，缓存的USB变化用于校准基线，不能再当作
	// 初始插入事件发送。期间真正发生的完整唤醒保留原观测时间，基线后照常交付。
	var resumes []notification
	for _, change := range initial {
		if _, _, err := normalize(devices, change); err != nil {
			return err
		}
		if change.Kind == model.EventResume {
			resumes = append(resumes, change)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	baseline := Snapshot{ObservedAt: time.Now().UTC(), USBKnown: usb}
	ids := make([]uint64, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		baseline.USBDevices = append(baseline.USBDevices, devices[id])
	}
	s.mu.Lock()
	state.baseline, state.ready = baseline, true
	close(state.changed)
	s.mu.Unlock()
	changes := resumes
	for {
		for _, change := range changes {
			event, publish, err := normalize(devices, change)
			if err != nil {
				return err
			}
			if !publish {
				continue
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case out <- event:
			default:
				// 消费者阻塞时绝不能连带阻塞电源确认。有限缓冲耗尽后失败并注销，
				// 让调用者明确重新订阅补扫，而不是静默丢失事实或无限占用内存。
				return model.NewError(model.CodeTransport, "macOS事件交付队列已满，需重新订阅并读取启动快照", nil)
			}
		}
		changes, err = listener.poll(ctx, 50*time.Millisecond)
		if err != nil {
			return err
		}
	}
}

func normalize(devices map[uint64]model.USBDevice, change notification) (model.Event, bool, error) {
	if change.ObservedAt.IsZero() {
		return model.Event{}, false, model.NewError(model.CodeTransport, "原生通知缺少观测时间", nil)
	}
	event := model.Event{Kind: change.Kind, Source: "darwin", ObservedAt: change.ObservedAt}
	switch change.Kind {
	case model.EventUSBAdded, model.EventUSBRemoved:
		if change.Device.ID == 0 {
			return model.Event{}, false, model.NewError(model.CodeTransport, "原生USB通知缺少实例编号", nil)
		}
		cached, exists := devices[change.Device.ID]
		if change.Kind == model.EventUSBAdded {
			if exists {
				return model.Event{}, false, nil
			}
			cached = change.Device.USB
			devices[change.Device.ID] = cached
		} else {
			if !exists {
				return model.Event{}, false, nil
			}
			delete(devices, change.Device.ID)
		}
		// 终止时设备属性可能已无法读取，因此沿用本轮实例缓存；空或重复序列号
		// 均不能作为合并设备的依据。该缓存不承担规则层的防抖和跨设备身份识别。
		event.USB = &cached
	case model.EventResume:
	default:
		return model.Event{}, false, model.NewError(model.CodeUnsupportedOperation, "macOS来源收到契约外通知", nil)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return model.Event{}, false, model.NewError(model.CodeInternal, "生成macOS事件编号失败", err)
	}
	event.ID = "darwin-" + hex.EncodeToString(id[:])
	if err := event.Validate(); err != nil {
		return model.Event{}, false, err
	}
	return event, true, nil
}

func eventCapabilities(usb, resume bool, usbReason, resumeReason string) []model.EventCapability {
	usbFields := []string{"source", "usb.vendor_id", "usb.product_id", "usb.serial", "usb.path"}
	return []model.EventCapability{
		{Kind: model.EventUSBAdded, Available: usb, Reason: usbReason, FilterFields: append([]string(nil), usbFields...)},
		{Kind: model.EventUSBRemoved, Available: usb, Reason: usbReason, FilterFields: append([]string(nil), usbFields...)},
		{Kind: model.EventResume, Available: resume, Reason: resumeReason, FilterFields: []string{"source"}},
		{Kind: model.EventSessionStarted, Reason: "本来源没有可靠的新登录通知，控制台切换、解锁和程序启动不能冒充会话开始"},
		{Kind: model.EventCustom, Reason: "macOS系统来源不接收自定义入口事件"},
		{Kind: model.EventTimer, Reason: "macOS系统来源不承担定时调度"},
	}
}
