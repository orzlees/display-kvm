// Package linux提供登录后用户会话中的真实udev/logind事件来源。
// 外部工具由调用者显式配置，不搜索PATH、不安装依赖、不修改权限或系统配置。
package linux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/events"
	"github.com/orzlee/displayKvm/internal/model"
)

type Config struct {
	BusctlPath  string
	UdevadmPath string
	// SessionID为空时优先使用登录环境的XDG_SESSION_ID，再按当前进程PID查询。
	// 环境值只作目标选择，必须再由logind核查所属UID，不能作为权限证明。
	SessionID string
}

// Snapshot与其他平台使用同形匿名别名，使调用方可复用返回类型而不改共享契约。
// USBKnown区分确认无设备与来源未知；SessionPresent=nil表示未知或不支持。
type Snapshot = struct {
	ObservedAt     time.Time
	USBDevices     []model.USBDevice
	USBKnown       bool
	SessionPresent *bool
}

type Source struct {
	config            Config
	runner            commandRunner
	platform          string
	uid, pid          int
	infrastructure    func() error
	pause             func(context.Context, time.Duration) error
	mu                sync.Mutex
	active, connected bool
	changed           chan struct{}
	baseline          *Snapshot
	lastErr           error
	sequence          uint64
}

var _ events.EventSource = (*Source)(nil)

func NewSource(config Config) *Source {
	if config.SessionID == "" {
		config.SessionID = os.Getenv("XDG_SESSION_ID")
	}
	return &Source{config: config, runner: execRunner{}, platform: runtime.GOOS, uid: os.Getuid(), pid: os.Getpid(), infrastructure: udevInfrastructure, pause: pause, changed: make(chan struct{})}
}

func (*Source) Name() string { return "linux" }

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func capabilitySet(usbErr, sessionErr error) []model.EventCapability {
	capability := func(kind model.EventKind, err error, fields []string) model.EventCapability {
		c := model.EventCapability{Kind: kind, Available: err == nil, FilterFields: fields}
		if err != nil {
			c.Reason = err.Error()
		}
		return c
	}
	usbFields := []string{"vendor_id", "product_id", "serial", "path"}
	caps := []model.EventCapability{
		capability(model.EventUSBAdded, usbErr, append([]string(nil), usbFields...)),
		capability(model.EventUSBRemoved, usbErr, append([]string(nil), usbFields...)),
		capability(model.EventSessionStarted, sessionErr, []string{"session_id"}),
		capability(model.EventResume, sessionErr, nil),
		{Kind: model.EventCustom, Reason: "本来源不支持自定义、锁屏、解锁或显示热插拔事件；不会用USB或会话事件替代"},
		{Kind: model.EventTimer, Reason: "定时事件由调度器提供，Linux通知来源不产生此事件"},
	}
	if sessionErr == nil {
		caps[2].Reason = "仅系统总线中核验同UID的目标会话；不含锁屏或解锁"
		caps[3].Reason = "仅logind PrepareForSleep(false)；不含桌面用户总线事件"
	}
	return caps
}

func (s *Source) Capabilities(ctx context.Context) ([]model.EventCapability, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, caps, _ := s.open(ctx)
	if r != nil {
		if err := r.close(); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return caps, nil
}

func cloneSnapshot(value Snapshot) Snapshot {
	value.USBDevices = append([]model.USBDevice{}, value.USBDevices...)
	if value.SessionPresent != nil {
		present := *value.SessionPresent
		value.SessionPresent = &present
	}
	return value
}

func (s *Source) notifyLocked() {
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

func (s *Source) Snapshot(ctx context.Context) (Snapshot, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		s.mu.Lock()
		if !s.active {
			err := s.lastErr
			if err == nil {
				err = model.NewError(model.CodeBackendUnavailable, "Linux事件来源尚未运行或已经退出", nil)
			}
			s.mu.Unlock()
			return Snapshot{}, err
		}
		if s.connected && s.baseline != nil {
			value := cloneSnapshot(*s.baseline)
			s.mu.Unlock()
			return value, nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		case <-changed:
		}
	}
}

type observation struct {
	usb    *udevRecord
	signal logindSignal
	at     time.Time
}

type eventRound struct {
	source        *Source
	ctx           context.Context
	cancel        context.CancelFunc
	subscriptions []subscription
	barriers      []chan chan struct{}
	queue         chan observation
	errors        chan error
	wg            sync.WaitGroup
	usb           bool
	identity      *logindIdentity
}

func (r *eventRound) fail(err error) {
	select {
	case r.errors <- err:
	default:
	}
	r.cancel()
}

func (r *eventRound) enqueue(value observation) error {
	select {
	case <-r.ctx.Done():
		return r.ctx.Err()
	default:
	}
	select {
	case r.queue <- value:
		return nil
	default:
		return model.NewError(model.CodeMessageTooLarge, "启动或消费期间通知超过256项缓存，不能静默丢弃", nil)
	}
}

func (r *eventRound) close() error {
	// 先取消所有发送与扫描，再回收每个真实子进程，最后等待读取协程。
	// 不关闭调用者的输出通道；未完全回收前不得启动下一轮订阅。
	r.cancel()
	var failures []error
	for _, sub := range r.subscriptions {
		if err := sub.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	r.wg.Wait()
	return errors.Join(failures...)
}

func waitReady(ctx context.Context, sub subscription) error {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-sub.Ready():
		if err != nil {
			return err
		}
		select {
		case <-sub.Done():
			if err := sub.Err(); err != nil {
				return err
			}
			return model.NewError(model.CodeTransport, "订阅在就绪期间退出", nil)
		default:
			return nil
		}
	case <-sub.Done():
		if err := sub.Err(); err != nil {
			return err
		}
		return model.NewError(model.CodeTransport, "订阅在就绪前退出", nil)
	case <-timer.C:
		return model.NewError(model.CodeTimeout, "真实事件订阅未在时限内确认监听就绪", context.DeadlineExceeded)
	}
}

func (s *Source) open(ctx context.Context) (*eventRound, []model.EventCapability, error) {
	childCtx, cancel := context.WithCancel(ctx)
	r := &eventRound{source: s, ctx: childCtx, cancel: cancel, queue: make(chan observation, 256), errors: make(chan error, 1)}
	if s.platform != "linux" {
		err := model.NewError(model.CodeUnsupportedOperation, "真实Linux事件来源仅能在Linux运行", nil)
		r.close()
		return nil, capabilitySet(err, err), err
	}
	usbErr := model.NewError(model.CodeBackendUnavailable, "未配置udevadm绝对路径", nil)
	var uerr error = usbErr
	if s.config.UdevadmPath != "" {
		uerr = validCommandPath(s.config.UdevadmPath)
		if uerr == nil {
			uerr = s.infrastructure()
		}
		if uerr == nil {
			var version []byte
			version, uerr = s.runner.run(childCtx, s.config.UdevadmPath, "--version")
			if uerr == nil {
				uerr = checkUdevVersion(version)
			}
		}
		if uerr == nil {
			var sub subscription
			sub, uerr = s.runner.start(childCtx, s.config.UdevadmPath, []string{"monitor", "--udev", "--property", "--subsystem-match=usb/usb_device"}, readyUdev)
			if uerr == nil {
				uerr = waitReady(childCtx, sub)
			}
			if uerr != nil && sub != nil {
				uerr = errors.Join(uerr, sub.Close())
			}
			if uerr == nil {
				r.usb = true
				r.attachUdev(sub)
			}
		}
	}
	var serr error = model.NewError(model.CodeBackendUnavailable, "未配置busctl绝对路径", nil)
	if s.config.BusctlPath != "" {
		var id *logindIdentity
		id, serr = s.probeLogind(childCtx)
		if serr == nil {
			var sub subscription
			sub, serr = s.runner.start(childCtx, s.config.BusctlPath, logindArguments(id.owner), readyNotify)
			if serr == nil {
				serr = waitReady(childCtx, sub)
			}
			if serr != nil && sub != nil {
				serr = errors.Join(serr, sub.Close())
			}
			if serr == nil {
				r.identity = id
				r.attachLogind(sub)
			}
		}
	}
	caps := capabilitySet(uerr, serr)
	if ctx.Err() != nil {
		cleanupErr := r.close()
		return nil, caps, errors.Join(ctx.Err(), cleanupErr)
	}
	if !r.usb && r.identity == nil {
		code := model.CodeBackendUnavailable
		for _, err := range []error{uerr, serr} {
			current := model.CodeOf(err)
			if current == model.CodePermissionDenied {
				code = model.CodePermissionDenied
				break
			}
			if code == model.CodeBackendUnavailable && current != model.CodeBackendUnavailable && current != model.CodeNotFound {
				code = current
			}
		}
		err := model.NewError(code, "没有可用的真实Linux事件来源: "+uerr.Error()+"; "+serr.Error(), errors.Join(uerr, serr))
		r.close()
		return nil, caps, err
	}
	return r, caps, nil
}

func (r *eventRound) pump(sub subscription, consume func(string) error) {
	barrier := make(chan chan struct{})
	r.subscriptions = append(r.subscriptions, sub)
	r.barriers = append(r.barriers, barrier)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		read := func(line string, ok bool) error {
			if !ok {
				if err := sub.Err(); err != nil {
					return err
				}
				return model.NewError(model.CodeTransport, "事件订阅输出意外关闭", nil)
			}
			return consume(line)
		}
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-sub.Done():
				err := sub.Err()
				if err == nil {
					err = model.NewError(model.CodeTransport, "事件订阅已断开", nil)
				}
				r.fail(err)
				return
			case line, ok := <-sub.Lines():
				if err := read(line, ok); err != nil {
					r.fail(err)
					return
				}
			case ack := <-barrier:
				// 初扫结束后先读完当前已排队的通知，防止调度延迟把扫描期间的USB变化
				// 错当成初始新增。边界后的真实变化继续增量发送，不宣称硬件事务原子性。
				for {
					select {
					case <-r.ctx.Done():
						return
					case line, ok := <-sub.Lines():
						if err := read(line, ok); err != nil {
							r.fail(err)
							return
						}
					default:
						close(ack)
						goto drained
					}
				}
			drained:
			}
		}
	}()
}

func (r *eventRound) attachUdev(sub subscription) {
	fields := make(map[string]string)
	started := false
	r.pump(sub, func(line string) error {
		if line == "" {
			if !started {
				return nil
			}
			record, err := parseUdevRecord(fields)
			fields, started = make(map[string]string), false
			if err != nil {
				return err
			}
			if record != nil {
				return r.enqueue(observation{usb: record, at: time.Now()})
			}
			return nil
		}
		if strings.HasPrefix(line, "UDEV ") {
			if started {
				return model.NewError(model.CodeUnsupportedProtocol, "udev通知缺少记录结束边界", nil)
			}
			started = true
			return nil
		}
		key, value, ok := strings.Cut(line, "=")
		if !started || !ok || key == "" {
			return model.NewError(model.CodeUnsupportedProtocol, "udev通知属性格式无效", nil)
		}
		if len(fields) >= 128 || len(value) > 4096 {
			return model.NewError(model.CodeMessageTooLarge, "udev通知属性超过容量限制", nil)
		}
		if previous, exists := fields[key]; exists && previous != value {
			return model.NewError(model.CodeUnsupportedProtocol, "udev通知含冲突的重复属性", nil)
		}
		fields[key] = value
		return nil
	})
}

func (r *eventRound) attachLogind(sub subscription) {
	r.pump(sub, func(line string) error {
		if line == "" {
			return nil
		}
		signal, err := parseLogindSignal(line, r.identity.owner)
		if err != nil {
			return err
		}
		if signal.session != "" && signal.session != r.identity.session {
			return nil
		}
		if signal.kind != "" || signal.removed {
			return r.enqueue(observation{signal: signal, at: time.Now()})
		}
		return nil
	})
}

type baselineState struct {
	devices  map[string]model.USBDevice
	usbKnown bool
	present  *bool
}
type scanResult struct {
	state baselineState
	err   error
}

func (s *Source) scan(r *eventRound) (baselineState, error) {
	state := baselineState{devices: make(map[string]model.USBDevice), usbKnown: r.usb}
	if r.usb {
		data, err := s.runner.run(r.ctx, s.config.UdevadmPath, "info", "--export-db")
		if err != nil {
			return state, err
		}
		state.devices, err = parseUSBDatabase(data)
		if err != nil {
			return state, err
		}
	}
	if r.identity != nil {
		present, err := s.sessionSnapshot(r.ctx, r.identity)
		if err != nil {
			return state, err
		}
		state.present = &present
	}
	return state, nil
}

func (s *Source) baselineScan(ctx context.Context, r *eventRound) (baselineState, []observation, error) {
	done := make(chan scanResult, 1)
	r.wg.Add(1)
	go func() { defer r.wg.Done(); state, err := s.scan(r); done <- scanResult{state: state, err: err} }()
	buffer := make([]observation, 0)
	appendObservation := func(value observation) error {
		if len(buffer) >= 256 {
			return model.NewError(model.CodeMessageTooLarge, "初扫期间通知超过256项缓存", nil)
		}
		buffer = append(buffer, value)
		return nil
	}
	var state baselineState
	for {
		select {
		case <-ctx.Done():
			return state, nil, ctx.Err()
		case err := <-r.errors:
			return state, nil, err
		case value := <-r.queue:
			if err := appendObservation(value); err != nil {
				return state, nil, err
			}
		case result := <-done:
			if result.err != nil {
				return state, nil, result.err
			}
			state = result.state
			goto scanned
		}
	}
scanned:
	for _, barrier := range r.barriers {
		ack := make(chan struct{})
		select {
		case barrier <- ack:
		case <-ctx.Done():
			return state, nil, ctx.Err()
		case err := <-r.errors:
			return state, nil, err
		}
		for {
			select {
			case <-ack:
				goto next
			case <-ctx.Done():
				return state, nil, ctx.Err()
			case err := <-r.errors:
				return state, nil, err
			case value := <-r.queue:
				if err := appendObservation(value); err != nil {
					return state, nil, err
				}
			}
		}
	next:
	}
	for {
		select {
		case err := <-r.errors:
			return state, nil, err
		case value := <-r.queue:
			if err := appendObservation(value); err != nil {
				return state, nil, err
			}
		default:
			goto reconciled
		}
	}
reconciled:
	resumes := make([]observation, 0)
	for _, value := range buffer {
		if value.usb != nil && value.usb.kind == model.EventUSBRemoved {
			// 设备可能在扫描读到它之前已移除，因而既不在数据库也没有完整移除身份。
			// 基线只需确认该路径缺席，不发送事件，所以不能套用增量的身份补全要求。
			delete(state.devices, value.usb.device.Path)
			continue
		}
		if _, err := s.apply(r, state, value); err != nil {
			return state, nil, err
		}
		if value.signal.kind == model.EventResume {
			resumes = append(resumes, value)
		}
	}
	return state, resumes, nil
}

func (s *Source) apply(r *eventRound, state baselineState, value observation) (*model.Event, error) {
	event := &model.Event{Source: s.Name(), ObservedAt: value.at}
	if value.usb != nil {
		record := value.usb
		device := record.device
		previous, known := state.devices[device.Path]
		if record.kind == model.EventUSBRemoved {
			// remove可缺少旧数据库已删除的身份字段；只沿同一DEVPATH复用已确认身份。
			// 未知设备不能凭零VID/PID或空序列号猜测，必须显式失败。
			if !record.identified {
				if !known {
					return nil, model.NewError(model.CodeIdentityAmbiguous, "USB移除缺少身份且本轮没有对应基线", nil)
				}
				device = previous
			} else if known && device.VendorID == previous.VendorID && device.ProductID == previous.ProductID && device.Serial == "" {
				device.Serial = previous.Serial
			}
			delete(state.devices, device.Path)
		} else {
			state.devices[device.Path] = device
		}
		event.Kind, event.USB = record.kind, &device
	} else {
		signal := value.signal
		if signal.session != "" {
			if signal.session != r.identity.session {
				return nil, nil
			}
			if signal.removed {
				if state.present != nil {
					*state.present = false
				}
				return nil, nil
			}
			if err := s.verifySession(r.ctx, signal.path, signal.session); err != nil {
				return nil, err
			}
			if state.present != nil {
				*state.present = true
			}
			event.Attributes = map[string]string{"session_id": signal.session}
		}
		event.Kind = signal.kind
	}
	if event.Kind == "" {
		return nil, nil
	}
	s.sequence++
	event.ID = fmt.Sprintf("linux-%d-%d", event.ObservedAt.UnixNano(), s.sequence)
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return event, nil
}

func (s *Source) publish(state baselineState) {
	value := Snapshot{ObservedAt: time.Now(), USBKnown: state.usbKnown, SessionPresent: state.present, USBDevices: make([]model.USBDevice, 0, len(state.devices))}
	for _, device := range state.devices {
		value.USBDevices = append(value.USBDevices, device)
	}
	sort.Slice(value.USBDevices, func(i, j int) bool { return value.USBDevices[i].Path < value.USBDevices[j].Path })
	s.mu.Lock()
	// 每次重连都先重新订阅并扫描内部状态，不能把旧基线当作新连接就绪凭据。
	// 对外快照仍固定为本Run首次成功启动的深副本；新Run会显式清空该基线。
	if s.baseline == nil {
		cloned := cloneSnapshot(value)
		s.baseline = &cloned
	}
	s.connected = true
	s.notifyLocked()
	s.mu.Unlock()
}

func (s *Source) consumeRound(ctx context.Context, r *eventRound, out chan<- model.Event) (error, bool) {
	state, pending, err := s.baselineScan(ctx, r)
	if err != nil {
		return err, false
	}
	s.publish(state)
	for {
		var value observation
		if len(pending) > 0 {
			value, pending = pending[0], pending[1:]
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err(), false
			case err := <-r.errors:
				return err, true
			case value = <-r.queue:
			}
		}
		event, err := s.apply(r, state, value)
		if err != nil {
			return err, false
		}
		if event == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err(), false
		case err := <-r.errors:
			return err, true
		case out <- *event:
		}
	}
}

func (s *Source) Run(ctx context.Context, out chan<- model.Event) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if out == nil {
		return model.NewError(model.CodeInvalidRequest, "事件输出通道不能为空", nil)
	}
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return model.NewError(model.CodeInvalidRequest, "同一Linux来源不能并发运行", nil)
	}
	s.active, s.connected, s.baseline, s.lastErr = true, false, nil, nil
	s.notifyLocked()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active, s.connected, s.lastErr = false, false, result
		s.notifyLocked()
		s.mu.Unlock()
	}()
	for attempt := 0; attempt < 3; attempt++ {
		r, _, err := s.open(ctx)
		// 已经发生过真实断线时，重建连接的暂时缺席也计入同一个有限重试预算。
		retry := attempt > 0 && err != nil
		if err == nil {
			err, retry = s.consumeRound(ctx, r, out)
			if ctx.Err() == nil && errors.Is(err, context.Canceled) {
				// 读取协程会因原始协议/容量错误取消本轮扫描，扫描返回的Canceled
				// 不能覆盖真正根因，否则调用方会把数据丢失误判成正常退出。
				select {
				case original := <-r.errors:
					err = original
				default:
				}
			}
			s.mu.Lock()
			s.connected = false
			s.notifyLocked()
			s.mu.Unlock()
			if cleanupErr := r.close(); cleanupErr != nil {
				return errors.Join(err, cleanupErr)
			}
		}
		if ctx.Err() != nil {
			return model.NewError(model.CodeCanceled, "Linux事件来源已取消", errors.Join(ctx.Err(), err))
		}
		if !retry || (model.CodeOf(err) != model.CodeTransport && model.CodeOf(err) != model.CodeIO && model.CodeOf(err) != model.CodeBackendUnavailable) || attempt == 2 {
			return err
		}
		// 最多三轮，且旧轮已全部释放；权限、协议、初扫与容量错误不可用无限重试掩盖。
		if err := s.pause(ctx, time.Duration(100*(1<<attempt))*time.Millisecond); err != nil {
			return model.NewError(model.CodeCanceled, "等待重连已取消", err)
		}
	}
	return model.NewError(model.CodeInternal, "Linux事件来源未完成订阅", nil)
}
