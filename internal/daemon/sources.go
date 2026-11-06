package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/orzlee/displayKvm/internal/ipc"
	"github.com/orzlee/displayKvm/internal/model"
)

// rescan串行执行临时来源轮次，防止同一来源同时打开多轮原生通知。任一来源失败
// 都返回未知基线与错误，不把旧设备、零值或已知空列表冒充本次成功观测。
func (d *Daemon) rescan(ctx context.Context) (Baseline, error) {
	d.mu.Lock()
	factories := append([]SourceFactory(nil), d.sources...)
	life := d.life
	if life != nil && life.Err() != nil {
		d.mu.Unlock()
		return Baseline{}, life.Err()
	}
	d.active.Add(1)
	d.mu.Unlock()
	defer d.active.Done()
	operation, cancel := context.WithTimeout(ctx, model.DefaultRequestTimeout)
	defer cancel()
	if life != nil {
		stop := context.AfterFunc(life, cancel)
		defer stop()
	}
	select {
	case d.scanGate <- struct{}{}:
		defer func() { <-d.scanGate }()
	case <-operation.Done():
		return Baseline{}, operation.Err()
	}
	if err := operation.Err(); err != nil {
		return Baseline{}, err
	}
	if len(factories) == 0 {
		return Baseline{}, model.NewError(model.CodeBackendUnavailable, "未注册可补扫的事件来源", nil)
	}
	var baseline Baseline
	for _, factory := range factories {
		source := factory()
		if source == nil {
			return Baseline{}, model.NewError(model.CodeBackendUnavailable, "补扫工厂返回空来源", nil)
		}
		value, err := scanRound(operation, source)
		if err != nil {
			return Baseline{}, err
		}
		if value.ObservedAt.After(baseline.ObservedAt) {
			baseline.ObservedAt = value.ObservedAt
		}
		if value.USBKnown {
			baseline.USBKnown = true
			baseline.USBDevices = append(baseline.USBDevices, value.USBDevices...)
		}
		if value.SessionPresent != nil {
			if baseline.SessionPresent != nil && *baseline.SessionPresent != *value.SessionPresent {
				return Baseline{}, model.NewError(model.CodeBackendUnavailable, "来源的会话观测冲突，保留未知", nil)
			}
			present := *value.SessionPresent
			baseline.SessionPresent = &present
		}
	}
	return baseline, nil
}

func scanRound(ctx context.Context, source Source) (baseline Baseline, result error) {
	life, cancel := context.WithCancel(ctx)
	output := make(chan model.Event, 64)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for range output {
			// 临时补扫轮次只取基线，不重播监听事件，避免与长期来源重复触发规则。
		}
	}()
	runDone := make(chan struct{})
	var runError error
	go func() {
		defer close(runDone)
		runError = source.Run(life, output)
	}()
	defer func() {
		cancel()
		<-runDone
		close(output)
		<-drainDone
		// 取消后的context错误属于正常回收，真实清理错误仍须反馈，不能发布假成功。
		if remaining := nonCancellation(runError); remaining != nil {
			baseline = Baseline{}
			result = errors.Join(result, remaining)
		}
	}()
	for {
		if err := life.Err(); err != nil {
			return Baseline{}, err
		}
		value, err := source.Snapshot(life)
		if err == nil {
			if err := life.Err(); err != nil {
				return Baseline{}, err
			}
			select {
			case <-runDone:
				return Baseline{}, model.NewError(model.CodeBackendUnavailable, "补扫来源已停止，基线不可用", runError)
			default:
			}
			if value.ObservedAt.IsZero() {
				return Baseline{}, model.NewError(model.CodeBackendUnavailable, "补扫缺少原始观测时间", nil)
			}
			value.USBDevices = append([]model.USBDevice(nil), value.USBDevices...)
			if value.SessionPresent != nil {
				present := *value.SessionPresent
				value.SessionPresent = &present
			}
			return value, nil
		}
		// Run在新协程开始，Snapshot可能先看到尚未active。只等待同一新轮次启动；
		// 已运行来源的Snapshot固定不刷新，成功后绝不重复调用它作为后续补扫。
		if model.CodeOf(err) != model.CodeBackendUnavailable {
			return Baseline{}, err
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-life.Done():
			timer.Stop()
			return Baseline{}, life.Err()
		case <-runDone:
			timer.Stop()
			return Baseline{}, errors.Join(err, runError)
		case <-timer.C:
		}
	}
}

// 取消常与原生关闭错误通过errors.Join一起返回。只移除纯context分支，不能因为
// errors.Is(err, context.Canceled)为true就吞掉同一错误树中的资源回收失败。
func nonCancellation(err error) error {
	if err == nil || err == context.Canceled || err == context.DeadlineExceeded {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var remaining error
		for _, child := range joined.Unwrap() {
			remaining = errors.Join(remaining, nonCancellation(child))
		}
		return remaining
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok && wrapped.Unwrap() != nil {
		remaining := nonCancellation(wrapped.Unwrap())
		if remaining == nil {
			return nil
		}
		if model.CodeOf(err) == model.CodeCanceled || model.CodeOf(err) == model.CodeTimeout {
			return remaining
		}
	}
	return err
}

func (d *Daemon) sourceFailed(index int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sourceErrs[index] = err
	if len(d.caps[index]) == 0 {
		for _, kind := range []model.EventKind{model.EventUSBAdded, model.EventUSBRemoved, model.EventSessionStarted, model.EventResume} {
			d.caps[index] = append(d.caps[index], model.EventCapability{Kind: kind})
		}
	}
	for i := range d.caps[index] {
		d.caps[index][i].Available = false
		d.caps[index][i].Reason = "事件来源不可用：" + ipc.PublicError(err).Error()
	}
}

// SourceErrors保留来源的真实本地根因用于诊断，网络只发送有界能力说明。
func (d *Daemon) SourceErrors() []error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]error(nil), d.sourceErrs...)
}

func (d *Daemon) eventCapabilities() []model.EventCapability {
	d.mu.Lock()
	defer d.mu.Unlock()
	kinds := []model.EventKind{model.EventUSBAdded, model.EventUSBRemoved, model.EventSessionStarted, model.EventResume, model.EventCustom, model.EventTimer}
	merged := make(map[model.EventKind]model.EventCapability)
	for _, kind := range kinds {
		merged[kind] = model.EventCapability{Kind: kind, Reason: "未注册提供此能力的事件来源"}
	}
	for _, caps := range d.caps {
		for _, capability := range caps {
			if capability.Validate() == nil && (capability.Available || !merged[capability.Kind].Available) {
				merged[capability.Kind] = capability
			}
		}
	}
	// 自定义入口的可用性取决于实际消费者；仅注册来源不代表自动化已经装配。
	custom := model.EventCapability{Kind: model.EventCustom, Available: d.consumers > 0, Reason: "仅接受custom事件；接受不表示已切换", FilterFields: []string{"name", "source"}}
	if d.consumers == 0 {
		custom.Reason = "尚未接入自动化消费者"
	}
	merged[model.EventCustom] = custom
	var output []model.EventCapability
	for _, kind := range kinds {
		output = append(output, merged[kind])
	}
	return cloneCapabilities(output)
}

func cloneCapabilities(caps []model.EventCapability) []model.EventCapability {
	copy := append([]model.EventCapability(nil), caps...)
	for index := range copy {
		copy[index].FilterFields = append([]string(nil), copy[index].FilterFields...)
	}
	return copy
}
