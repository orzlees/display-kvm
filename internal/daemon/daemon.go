// Package daemon统一拥有队列、事件来源、自动化与请求生命周期。它不访问后端，
// 生产环境必须传入08的唯一Queue；配置重建须先等待本后台Run退出及旧队列Close。
package daemon

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/events"
	"github.com/orzlee/displayKvm/internal/ipc"
	"github.com/orzlee/displayKvm/internal/model"
)

// Queue仅暴露后台使用的串行操作，生产实现为switcher.Queue，不向来源或客户端
// 交付Backend。Close必须等待在途操作结束，否则退出后可能仍有显示器写入者。
type Queue interface {
	Switch(context.Context, model.SwitchRequest) (model.SwitchResult, error)
	Enumerate(context.Context) ([]model.Device, error)
	Close() error
}

// Baseline与三平台Snapshot的匿名别名同形，保持未知、已知空集合和原观测时间。
type Baseline = struct {
	ObservedAt     time.Time
	USBDevices     []model.USBDevice
	USBKnown       bool
	SessionPresent *bool
}

type Source interface {
	events.EventSource
	Snapshot(context.Context) (Baseline, error)
}

// SourceFactory每次必须构造全新来源。来源Snapshot仅属于该次Run的固定启动基线，
// 唤醒补扫不能复用长期监听实例的旧Snapshot，也不能并发重启同一个来源对象。
type SourceFactory func() Source

type AutomationRuntime struct {
	Events <-chan model.Event
	Switch func(context.Context, model.SwitchRequest) (model.SwitchResult, error)
	Rescan func(context.Context) (Baseline, error)
}

type Automation interface {
	// Run必须响应ctx，返回前等待其创建的工作结束；不得关闭后台拥有的Events。
	Run(context.Context) error
}

// AutomationFactory仅构造实例，不启动工作；实际生命周期统一由后台调用Run管理。
type AutomationFactory func(AutomationRuntime) (Automation, error)

type responseDrain struct {
	done chan struct{}
	err  error
}

type Daemon struct {
	queue Queue
	mu    sync.Mutex
	// 注册表在Run前冻结；started不会复位，防止已关闭队列被同一后台再次使用。
	started           bool
	sources           []SourceFactory
	automations       []AutomationFactory
	caps              [][]model.EventCapability
	sourceErrs        []error
	life              context.Context
	input             chan model.Event
	consumers         int
	scanGate          chan struct{}
	active            sync.WaitGroup
	management        ManagementHandler
	capabilityOverlay func([]model.EventCapability) []model.EventCapability
	draining          bool
	responses         int
	drain             *responseDrain
}

func New(queue Queue) (*Daemon, error) {
	if queue == nil {
		return nil, model.NewError(model.CodeBackendUnavailable, "后台必须拥有唯一切换队列", nil)
	}
	return &Daemon{queue: queue, scanGate: make(chan struct{}, 1)}, nil
}

func (d *Daemon) RegisterEventSource(factory SourceFactory) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || factory == nil {
		return model.NewError(model.CodeInvalidRequest, "事件来源只能在Run之前注册且工厂不能为空", nil)
	}
	d.sources = append(d.sources, factory)
	return nil
}

func (d *Daemon) RegisterAutomation(factory AutomationFactory) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || factory == nil {
		return model.NewError(model.CodeInvalidRequest, "自动化只能在Run之前注册且工厂不能为空", nil)
	}
	d.automations = append(d.automations, factory)
	return nil
}

// Run接管监听器与队列，阻塞到取消或失败。调用者须先通过ipc.Acquire持有系统锁，
// 监听127.0.0.1:0并发布私有端点，系统锁只能在Run返回以后释放。
func (d *Daemon) Run(ctx context.Context, listener net.Listener, token string) (result error) {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return model.NewError(model.CodeInvalidRequest, "同一后台只能Run一次", nil)
	}
	d.started = true
	life, cancel := context.WithCancel(ctx)
	d.life, d.input, d.consumers = life, make(chan model.Event, 64), len(d.automations)
	factories := append([]SourceFactory(nil), d.sources...)
	automationFactories := append([]AutomationFactory(nil), d.automations...)
	d.caps, d.sourceErrs = make([][]model.EventCapability, len(factories)), make([]error, len(factories))
	d.mu.Unlock()
	defer cancel()
	defer func() {
		if err := d.queue.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}()
	defer d.stopRuntime(cancel)
	if listener == nil {
		return model.NewError(model.CodeInvalidRequest, "后台监听器不能为空", nil)
	}
	defer listener.Close()
	if err := ipc.ValidateListener(listener, token); err != nil {
		return err
	}

	outputs := make([]chan model.Event, len(automationFactories))
	automations := make([]Automation, len(automationFactories))
	for index, factory := range automationFactories {
		outputs[index] = make(chan model.Event, 64)
		runtime := AutomationRuntime{Events: outputs[index], Switch: d.switchRequest, Rescan: d.rescan}
		automation, err := factory(runtime)
		if err != nil {
			return err
		}
		if automation == nil {
			return model.NewError(model.CodeInvalidRequest, "自动化工厂返回空实例", nil)
		}
		automations[index] = automation
	}
	var workers sync.WaitGroup
	failures := make(chan error, len(automations)+1)
	cleanupFailures := make(chan error, len(automations)+len(factories))
	// 通道始终由后台创建与关闭。退出先取消全部生产者并等待，再关闭输入；转发者
	// 结束后才关闭每个消费者通道，不会与来源或IPC发送并发关闭同一通道。
	forwardDone := make(chan struct{})
	go func() {
		defer close(forwardDone)
		for {
			select {
			case <-life.Done():
				return
			case event := <-d.input:
				if err := event.Validate(); err != nil {
					// 非法来源事件不能静默进入规则，也不能默默丢弃后继续声称来源健康。
					failures <- model.NewError(model.CodeInvalidRequest, "事件来源产生非法事件，后台已停止", err)
					cancel()
					return
				}
				for _, output := range outputs {
					select {
					case output <- cloneEvent(event):
					case <-life.Done():
						return
					}
				}
			}
		}
	}()
	for _, automation := range automations {
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := automation.Run(life)
			if life.Err() == nil {
				if err == nil {
					err = model.NewError(model.CodeBackendUnavailable, "自动化意外停止", nil)
				}
				failures <- err
				cancel()
			} else if remaining := nonCancellation(err); remaining != nil {
				cleanupFailures <- remaining
			}
		}()
	}
	for index, factory := range factories {
		source := factory()
		if source == nil {
			d.sourceFailed(index, model.NewError(model.CodeBackendUnavailable, "事件来源工厂返回空实例", nil))
			continue
		}
		probe, finish := context.WithTimeout(life, model.DefaultRequestTimeout)
		caps, err := source.Capabilities(probe)
		finish()
		if err == nil {
			for _, capability := range caps {
				if err = capability.Validate(); err != nil {
					break
				}
			}
		}
		d.mu.Lock()
		d.caps[index] = cloneCapabilities(caps)
		d.mu.Unlock()
		if err != nil {
			d.sourceFailed(index, err)
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := source.Run(life, d.input)
			if life.Err() == nil {
				if err == nil {
					err = model.NewError(model.CodeBackendUnavailable, "事件来源意外停止", nil)
				}
				d.sourceFailed(index, err)
			} else if remaining := nonCancellation(err); remaining != nil {
				d.sourceFailed(index, remaining)
				cleanupFailures <- remaining
			}
		}()
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- ipc.Serve(life, listener, token, d.handle) }()
	select {
	case result = <-serverDone:
		cancel()
	case <-life.Done():
		cancel()
		result = <-serverDone
	}
	// Serve已经关闭并等待全部连接处理器；来源及自动化都响应同一个取消context。
	// rescan自身等待临时来源回收，08.Close最后等待所有后端工作，不遗留第二写入者。
	workers.Wait()
	d.stopRuntime(cancel)
	<-forwardDone
	close(d.input)
	for _, output := range outputs {
		close(output)
	}
	close(cleanupFailures)
	var cleanupError error
	for err := range cleanupFailures {
		cleanupError = errors.Join(cleanupError, err)
	}
	select {
	case err := <-failures:
		return errors.Join(err, cleanupError)
	default:
		if cleanupError != nil {
			return errors.Join(result, cleanupError)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return result
	}
}

// Drain只排空已受理响应，不取消业务context或关闭连接。准入与响应登记共用mu，
// 归零通道也在该锁下关闭，避免WaitGroup在等待期间漏接新登记的取消操作。
// 成功后门禁保持到Run关闭；失败则恢复旧轮次，调用方不能继续保存或另建队列。
func (d *Daemon) Drain(ctx context.Context) error {
	d.mu.Lock()
	if d.life == nil || d.life.Err() != nil || d.draining {
		d.mu.Unlock()
		return model.NewError(model.CodeBackendUnavailable, "后台未运行、已停止或正在排空", nil)
	}
	batch := &responseDrain{done: make(chan struct{})}
	d.draining, d.drain = true, batch
	if d.responses == 0 {
		close(batch.done)
	}
	d.mu.Unlock()
	select {
	case <-batch.done:
	case <-ctx.Done():
	}
	d.mu.Lock()
	err := errors.Join(batch.err, ctx.Err())
	d.drain = nil
	if err != nil {
		d.draining = false
	}
	d.mu.Unlock()
	return err
}

func (d *Daemon) responseComplete(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.responses--
	if d.drain != nil {
		d.drain.err = errors.Join(d.drain.err, err)
		if d.responses == 0 {
			close(d.drain.done)
		}
	}
}

func (d *Daemon) handle(ctx context.Context, request model.Request) model.Response {
	response := model.Response{Version: model.ProtocolVersion, ID: request.ID, Operation: request.Operation}
	d.mu.Lock()
	tracked := true
	if d.draining {
		// 新查询不延长排空；Stop只取消已有试切，必须仍计入屏障，否则可能在其
		// 响应尚未写出时换代。归零后不再接受Stop，保证关闭窗口不会重新长出写入者。
		switch {
		case request.Operation == model.OperationManagementCapabilities || request.Operation == model.OperationEventCapabilities:
			tracked = false
		case request.Operation == model.OperationManagement && request.Management != nil && request.Management.Action == model.ManagementCommitStatus:
			tracked = false
		case request.Operation == model.OperationManagement && request.Management != nil && request.Management.Action == model.ManagementTrialStop && d.responses > 0:
		default:
			d.mu.Unlock()
			response.Error = model.NewError(model.CodeBackendUnavailable, "后台正在排空已受理结果，未接受新操作", nil)
			return response
		}
	}
	if tracked {
		d.responses++
		if !ipc.OnResponseComplete(ctx, d.responseComplete) {
			// 直接调用处理器没有网络帧；必须在同步返回时清理，不能留下永不触发的计数。
			defer d.responseComplete(nil)
		}
	}
	d.mu.Unlock()
	var err error
	switch request.Operation {
	case model.OperationSwitch:
		var result model.SwitchResult
		result, err = d.queue.Switch(ctx, *request.Switch)
		if err == nil {
			response.SwitchResult = &result
		}
	case model.OperationEnumerate:
		response.Devices, err = d.queue.Enumerate(ctx)
	case model.OperationEventCapabilities:
		response.EventCapabilities = d.Capabilities()
	case model.OperationManagementCapabilities:
		d.mu.Lock()
		available := d.management != nil
		d.mu.Unlock()
		if !available {
			err = model.NewError(model.CodeUnsupportedOperation, "后台未接入管理能力", nil)
		} else {
			response.Accepted = true
		}
	case model.OperationManagement:
		d.mu.Lock()
		handler := d.management
		d.mu.Unlock()
		if handler == nil {
			err = model.NewError(model.CodeUnsupportedOperation, "后台未接入管理能力", nil)
		} else {
			var result model.ManagementResponse
			result, err = handler(ctx, *request.Management)
			if err == nil {
				response.Management = &result
			}
		}
	case model.OperationEmitEvent:
		err = d.emit(ctx, *request.Event)
		response.Accepted = err == nil
	default:
		err = model.NewError(model.CodeUnsupportedOperation, "不支持的后台操作", nil)
	}
	if err != nil {
		// 只有请求级拒绝进入外层Error；08已受理的逐屏失败始终保留在SwitchResult中。
		return model.Response{Version: model.ProtocolVersion, ID: request.ID, Operation: request.Operation, Error: ipc.PublicError(err)}
	}
	return response
}

func (d *Daemon) switchRequest(ctx context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
	d.mu.Lock()
	life := d.life
	if life == nil || life.Err() != nil || d.draining {
		d.mu.Unlock()
		return model.SwitchResult{}, model.NewError(model.CodeBackendUnavailable, "后台尚未运行、正在排空或已经停止", nil)
	}
	d.active.Add(1)
	d.mu.Unlock()
	defer d.active.Done()
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(life, cancel)
	defer stop()
	defer cancel()
	return d.queue.Switch(operation, request)
}

func (d *Daemon) stopRuntime(cancel context.CancelFunc) {
	cancel()
	// Runtime可被消费者保留或在其工作协程调用。取消后通过同一注册锁建立屏障，
	// 新调用已不能登记；随后等待全部已登记Switch/Rescan，避免与Wait并发新增计数。
	d.mu.Lock()
	d.mu.Unlock()
	d.active.Wait()
}

func (d *Daemon) emit(ctx context.Context, event model.Event) error {
	d.mu.Lock()
	life, input, consumers, draining := d.life, d.input, d.consumers, d.draining
	d.mu.Unlock()
	if life == nil || consumers == 0 || life.Err() != nil || draining {
		return model.NewError(model.CodeBackendUnavailable, "后台尚未接入自动化消费者，未接受自定义事件", nil)
	}
	select {
	case <-life.Done():
		return life.Err()
	case <-ctx.Done():
		return ctx.Err()
	case input <- cloneEvent(event):
		return nil
	}
}

func cloneEvent(event model.Event) model.Event {
	if event.USB != nil {
		usb := *event.USB
		event.USB = &usb
	}
	if event.Attributes != nil {
		attributes := make(map[string]string, len(event.Attributes))
		for key, value := range event.Attributes {
			attributes[key] = value
		}
		event.Attributes = attributes
	}
	return event
}
