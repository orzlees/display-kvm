// Package windows提供登录用户会话内的Windows事件来源。
package windows

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orzlee/displayKvm/internal/events"
	"github.com/orzlee/displayKvm/internal/model"
)

// Snapshot使用匿名结构体别名，与其他平台的同形启动快照具有相同实际类型。
// USBKnown为false以及SessionPresent为nil表示未知，不能据此推断设备或会话不存在。
type Snapshot = struct {
	ObservedAt     time.Time
	USBDevices     []model.USBDevice
	USBKnown       bool
	SessionPresent *bool
}

const notificationLimit = 128

type noticeKind uint8

const (
	noticeUSB noticeKind = iota + 1
	noticeLogin
	noticeLogoff
	noticeResume
)

type notification struct {
	kind       noticeKind
	observedAt time.Time
	path       string
}

// 该局部边界只隔离系统调用，生产构造器始终选择真实平台实现；
// 测试替换订阅后仍运行同一套基线对账、归一化和退出逻辑。
type platform interface {
	probe(context.Context) error
	open(context.Context) (subscription, error)
}

type subscription interface {
	Snapshot(context.Context) (Snapshot, error)
	Notifications() <-chan notification
	Done() <-chan struct{}
	Err() error
	Close() error
}

type runState struct {
	ready    chan struct{}
	ended    chan struct{}
	active   bool
	baseline Snapshot
	err      error
}

type Source struct {
	mu       sync.Mutex
	platform platform
	filters  []USBFilter
	started  chan struct{}
	current  *runState
}

var _ events.EventSource = (*Source)(nil)
var eventSequence atomic.Uint64

// New不传筛选条件时观察所有USB设备。条件之间取“或”，单个条件的字段取“且”；
// 这里只选择来源设备，不执行自动化规则、规则防抖或动作。
func New(filters ...USBFilter) *Source {
	return newSource(newPlatform(), filters)
}

func newSource(p platform, filters []USBFilter) *Source {
	return &Source{platform: p, filters: cloneFilters(filters), started: make(chan struct{})}
}

func (s *Source) Name() string { return "windows" }

func (s *Source) initializeLocked() {
	if s.platform == nil {
		s.platform = newPlatform()
	}
	if s.started == nil && s.current == nil {
		s.started = make(chan struct{})
	}
}

func (s *Source) Capabilities(ctx context.Context) ([]model.EventCapability, error) {
	if err := ctx.Err(); err != nil {
		return nil, model.NewError(model.CodeCanceled, "Windows能力查询已取消", err)
	}
	s.mu.Lock()
	s.initializeLocked()
	p := s.platform
	s.mu.Unlock()
	err := p.probe(ctx)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	available := err == nil
	reason := func(boundary string) string {
		if err != nil {
			return err.Error()
		}
		return boundary
	}
	return []model.EventCapability{
		{Kind: model.EventUSBAdded, Available: available, Reason: reason("提供VID/PID和不透明路径，序列号未知；启动状态由Snapshot提供，极短暂插拔可能在存在状态扫描间合并"), FilterFields: []string{"usb.vendor_id", "usb.product_id", "usb.path"}},
		{Kind: model.EventUSBRemoved, Available: available, Reason: reason("按设备路径对账移除；不依赖非唯一VID/PID或缺失序列号"), FilterFields: []string{"usb.vendor_id", "usb.product_id", "usb.path"}},
		{Kind: model.EventSessionStarted, Available: available, Reason: reason("仅当前登录用户会话的真实登录；不将解锁、控制台或远程连接变化当作登录，不覆盖登录前运行")},
		{Kind: model.EventResume, Available: available, Reason: reason("仅PBT_APMRESUMEAUTOMATIC低功耗恢复；不提供显示变化事件；实际注册失败由Run返回")},
		{Kind: model.EventCustom, Reason: "Windows系统订阅不提供自定义事件，应使用独立自定义来源"},
		{Kind: model.EventTimer, Reason: "Windows系统订阅不提供定时事件，应使用独立定时来源"},
	}, nil
}

// Snapshot等待当前Run建立基线。首次Run之前允许等待，以免启动协程的调度先后
// 影响调用方；一旦绑定某轮Run，就不能跨轮等待或返回上一轮的缓存。
func (s *Source) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, model.NewError(model.CodeCanceled, "等待Windows启动快照已取消", err)
	}
	s.mu.Lock()
	s.initializeLocked()
	r, started := s.current, s.started
	s.mu.Unlock()
	if r == nil {
		select {
		case <-ctx.Done():
			return Snapshot{}, model.NewError(model.CodeCanceled, "等待Windows运行已取消", ctx.Err())
		case <-started:
		}
		s.mu.Lock()
		r = s.current
		s.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		return Snapshot{}, model.NewError(model.CodeCanceled, "等待Windows启动快照已取消", ctx.Err())
	case <-r.ended:
	case <-r.ready:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !r.active || s.current != r {
		if r.err != nil {
			return Snapshot{}, r.err
		}
		return Snapshot{}, model.NewError(model.CodeBackendUnavailable, "Windows事件来源本轮运行已结束", nil)
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, model.NewError(model.CodeCanceled, "读取Windows启动快照已取消", err)
	}
	return cloneSnapshot(r.baseline), nil
}

func (s *Source) Run(ctx context.Context, out chan<- model.Event) (retErr error) {
	s.mu.Lock()
	s.initializeLocked()
	if s.current != nil && s.current.active {
		s.mu.Unlock()
		return model.NewError(model.CodeInvalidRequest, "同一Windows来源不能并发运行", nil)
	}
	r := &runState{ready: make(chan struct{}), ended: make(chan struct{}), active: true}
	s.current = r
	if s.started != nil {
		close(s.started)
		s.started = nil
	}
	p := s.platform
	s.mu.Unlock()
	// 必须在订阅Close之后唤醒等待者，保证失败快照也不先于资源释放返回。
	defer func() {
		s.mu.Lock()
		r.err, r.active, r.baseline = retErr, false, Snapshot{}
		close(r.ended)
		s.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return model.NewError(model.CodeCanceled, "Windows事件来源已取消", err)
	}
	watch, err := p.open(ctx)
	if watch != nil {
		defer func() { retErr = errors.Join(retErr, watch.Close()) }()
	}
	if err != nil {
		return err
	}
	if watch == nil {
		return model.NewError(model.CodeInternal, "Windows订阅未返回监听实例", nil)
	}
	state, pending, err := s.initialBaseline(ctx, watch)
	if err != nil {
		return err
	}
	s.mu.Lock()
	r.baseline = cloneSnapshot(state)
	close(r.ready)
	s.mu.Unlock()
	for _, n := range pending {
		if err := s.apply(ctx, watch, out, &state, n); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return model.NewError(model.CodeCanceled, "Windows事件来源已取消", ctx.Err())
		case <-watch.Done():
			return subscriptionError(watch)
		case n, ok := <-watch.Notifications():
			if !ok {
				return subscriptionError(watch)
			}
			if err := s.apply(ctx, watch, out, &state, n); err != nil {
				return err
			}
		}
	}
}

func (s *Source) initialBaseline(ctx context.Context, watch subscription) (Snapshot, []notification, error) {
	select {
	case <-watch.Done():
		return Snapshot{}, nil, subscriptionError(watch)
	default:
	}
	snapshot, err := watch.Snapshot(ctx)
	if err != nil {
		return Snapshot{}, nil, err
	}
	pending := make([]notification, 0)
	processed := 0
	for {
		dirty := false
	drain:
		for {
			select {
			case <-ctx.Done():
				return Snapshot{}, nil, ctx.Err()
			case <-watch.Done():
				return Snapshot{}, nil, subscriptionError(watch)
			case n, ok := <-watch.Notifications():
				if !ok {
					return Snapshot{}, nil, subscriptionError(watch)
				}
				processed++
				// 持续变化不能无限延长初扫或无限积压；无法建立可信基线时明确失败。
				if processed > notificationLimit {
					return Snapshot{}, nil, notificationOverflow()
				}
				switch n.kind {
				case noticeUSB:
					dirty = true
				case noticeLogoff:
					return Snapshot{}, nil, sessionEnded()
				case noticeLogin:
					present := true
					snapshot.SessionPresent = &present
				case noticeResume:
					// 恢复没有可对账的存在状态，保留真实通知，在基线发布后输出。
					pending = append(pending, n)
				default:
					return Snapshot{}, nil, model.NewError(model.CodeIO, "Windows订阅返回未知通知", nil)
				}
			default:
				break drain
			}
		}
		if !dirty {
			selected, err := s.selectSnapshot(snapshot)
			return selected, pending, err
		}
		// 先启用订阅再扫描，初扫期间USB通知只触发重扫，不伪造usb_added。
		// 这里保证采样与通知对账，不声称Windows提供硬件事务式原子快照。
		snapshot, err = watch.Snapshot(ctx)
		if err != nil {
			return Snapshot{}, nil, err
		}
	}
}

func (s *Source) apply(ctx context.Context, watch subscription, out chan<- model.Event, state *Snapshot, n notification) error {
	switch n.kind {
	case noticeUSB:
		next, err := watch.Snapshot(ctx)
		if err != nil {
			return err
		}
		next, err = s.selectSnapshot(next)
		if err != nil {
			return err
		}
		if state.USBKnown && next.USBKnown {
			for _, change := range usbChanges(state.USBDevices, next.USBDevices) {
				device := change.device
				if err := s.send(ctx, watch, out, change.kind, &device, n.observedAt); err != nil {
					return err
				}
			}
		}
		*state = next
	case noticeLogin:
		// 用户名查询可能先于WTS登录通知变为“已登录”，不能用已知存在状态
		// 吞掉基线之后收到的真实LOGON；解锁等伪登录已在消息分类处排除。
		present := true
		state.SessionPresent = &present
		return s.send(ctx, watch, out, model.EventSessionStarted, nil, n.observedAt)
	case noticeLogoff:
		return sessionEnded()
	case noticeResume:
		return s.send(ctx, watch, out, model.EventResume, nil, n.observedAt)
	default:
		return model.NewError(model.CodeIO, "Windows订阅返回未知通知", nil)
	}
	return nil
}

func (s *Source) send(ctx context.Context, watch subscription, out chan<- model.Event, kind model.EventKind, usb *model.USBDevice, observedAt time.Time) error {
	event := model.Event{ID: fmt.Sprintf("windows:%d:%d", time.Now().UnixNano(), eventSequence.Add(1)), Source: s.Name(), Kind: kind, ObservedAt: observedAt, USB: usb}
	if err := event.Validate(); err != nil {
		return err
	}
	// 通道归调用者所有，来源永不关闭；慢消费者不能阻止取消或系统监听退出。
	select {
	case out <- event:
		return nil
	case <-ctx.Done():
		return model.NewError(model.CodeCanceled, "Windows事件发送已取消", ctx.Err())
	case <-watch.Done():
		return subscriptionError(watch)
	}
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	if snapshot.USBDevices != nil {
		devices := make([]model.USBDevice, len(snapshot.USBDevices))
		copy(devices, snapshot.USBDevices)
		snapshot.USBDevices = devices
	}
	if snapshot.SessionPresent != nil {
		present := *snapshot.SessionPresent
		snapshot.SessionPresent = &present
	}
	return snapshot
}

func enqueueNotification(out chan<- notification, n notification) error {
	select {
	case out <- n:
		return nil
	default:
		return notificationOverflow()
	}
}

func notificationOverflow() error {
	return model.NewError(model.CodeIO, "Windows事件通知缓冲区已满，停止监听以避免静默丢失", nil)
}

func subscriptionError(watch subscription) error {
	if err := watch.Err(); err != nil {
		return err
	}
	return model.NewError(model.CodeBackendUnavailable, "Windows系统监听已结束", nil)
}

func sessionEnded() error {
	return model.NewError(model.CodeBackendUnavailable, "当前Windows用户会话已注销，监听已结束", nil)
}

func classifySession(code, session, current uint32) (noticeKind, bool) {
	if session != current {
		return 0, false
	}
	switch code {
	case 5: // WTS_SESSION_LOGON，解锁、控制台连接和远程连接均不代表新登录。
		return noticeLogin, true
	case 6: // WTS_SESSION_LOGOFF，不把注销伪造为共享模型中不存在的事件。
		return noticeLogoff, true
	default:
		return 0, false
	}
}

func isAutomaticResume(code uint32) bool { return code == 0x12 }
