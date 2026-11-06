// Package switcher拥有后台唯一的后端访问队列。诊断枚举也会使Windows旧枚举引用
// 失效，因此枚举、读取及切换必须共用本队列，调用方不得另持Backend并发访问。
package switcher

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/backend"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/identity"
	"github.com/orzlee/displayKvm/internal/model"
)

const (
	queueCapacity = 64
	dedupCapacity = 256
	dedupTTL      = 5 * time.Minute
	readAttempts  = 3
	readRetryWait = 10 * time.Millisecond
)

// Queue在New时启动唯一工作协程。配置是独立、固定快照，不提供第二套更新入口；
// 配置变更时须先Close旧队列，再用新快照构造，以免同一后端出现两个访问所有者。
// 队列不跨进程或重启承诺幂等；Close须等待遵守context契约的后端完成资源清理。
type Queue struct {
	backend backend.Backend
	cfg     config.Config
	life    context.Context
	cancel  context.CancelFunc
	jobs    chan *job
	done    chan struct{}

	mu       sync.Mutex
	closed   bool
	switches map[model.RequestID]*job
}

type outcome struct {
	switchResult model.SwitchResult
	preview      model.InputPreviewStatus
	devices      []model.Device
	capabilities model.DeviceCapabilities
	observation  model.InputObservation
	err          error
}

type job struct {
	ctx          context.Context
	cancel       context.CancelFunc
	run          func(context.Context) outcome
	abort        func(error) outcome
	request      *model.SwitchRequest
	done         chan struct{}
	stopShutdown func() bool
	stopCanceled func() bool
	// 以下状态只在Queue.mu内写入；done关闭后结果不再修改，等待者得到深复制。
	started    bool
	finished   bool
	finishedAt time.Time
	result     outcome
}

// New只校验并复制配置，不扫描或控制硬件。调用方在构造返回前须保持输入快照
// 不变；返回后可编辑原配置，嵌套映射、分组和Binding均不与队列共享存储。
func New(b backend.Backend, cfg config.Config) (*Queue, error) {
	if b == nil {
		return nil, model.NewError(model.CodeBackendUnavailable, "切换队列未配置后端", nil)
	}
	snapshot := cloneConfig(cfg)
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(context.Background())
	q := &Queue{
		backend: b, cfg: snapshot, life: life, cancel: cancel,
		jobs: make(chan *job, queueCapacity), done: make(chan struct{}),
		switches: make(map[model.RequestID]*job),
	}
	go q.work()
	return q, nil
}

// Close拒绝新操作，取消在途及排队操作，并等待唯一工作协程停止。不能通过另起
// 协程提前放弃后端调用，否则句柄仍被使用时就可能由新队列再次访问或错误释放。
// 重复Close安全；调用方不得从后端回调中调用Close并等待自身结束。
func (q *Queue) Close() error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.cancel()
		close(q.jobs)
	}
	q.mu.Unlock()
	<-q.done
	return nil
}

// Switch将请求语法/选择拒绝作为error返回。成功入队后的取消、身份拒绝和部分
// 失败都保留在SwitchResult内，外层error为nil，避免调用者丢弃已经发送屏的证据。
// 同ID同原始字段内容共享执行和缓存结果；不同内容拒绝。显示器顺序和超时字段
// 也属于内容，不能复用同一ID修改选择。原调用者拥有执行取消权，重复调用者取消
// 只退出自己的等待。最多保留256条在途/完成记录；完成后保留5分钟再懒过期，
// 容量满时拒绝新ID而不提前逐出。超出此窗口后须使用新ID并核对设备实际状态。
func (q *Queue) Switch(ctx context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
	request.DisplayIDs = append([]model.DisplayID(nil), request.DisplayIDs...)
	if err := request.Validate(); err != nil {
		return model.SwitchResult{}, err
	}
	plan, err := q.plan(request)
	if err != nil {
		return model.SwitchResult{}, err
	}
	operation, cancel := context.WithTimeout(ctx, request.EffectiveTimeout())
	defer cancel()
	j := &job{ctx: operation, cancel: cancel, request: &request, done: make(chan struct{})}
	j.run = func(ctx context.Context) outcome {
		return outcome{switchResult: q.execute(ctx, request.ID, plan)}
	}
	j.abort = func(err error) outcome {
		return outcome{switchResult: failedRequest(request.ID, plan, err)}
	}
	actual, owner, err := q.enqueue(j)
	if err != nil {
		return model.SwitchResult{}, err
	}
	if owner {
		// 原调用者须等实际执行停止才取得最终逐屏结果；排队取消由回调直接完成，
		// 不必等待前一项。执行中则由后端响应context并清理后再发布真实结果。
		<-actual.done
	} else {
		select {
		case <-actual.done:
		case <-operation.Done():
			return model.SwitchResult{}, operationError(operation.Err())
		}
	}
	return cloneResult(actual.result.switchResult), actual.result.err
}

// Enumerate返回仅供当前诊断/人工绑定使用的描述副本。BackendID不持久化，也不
// 允许再传回队列操作；后续能力读取及输入读取必须使用逻辑DisplayID重新解析。
func (q *Queue) Enumerate(ctx context.Context) ([]model.Device, error) {
	result, err := q.readJob(ctx, func(ctx context.Context) outcome {
		devices, err := retryRead(ctx, q.backend.Enumerate)
		return outcome{devices: append([]model.Device(nil), devices...), err: err}
	})
	return append([]model.Device(nil), result.devices...), err
}

func (q *Queue) Capabilities(ctx context.Context, id model.DisplayID) (model.DeviceCapabilities, error) {
	if err := q.validateDisplayID(id); err != nil {
		return model.DeviceCapabilities{}, err
	}
	result, err := q.readJob(ctx, func(ctx context.Context) outcome {
		device, err := q.resolveDevice(ctx, id)
		if err != nil {
			return outcome{err: err}
		}
		caps, err := retryRead(ctx, func(ctx context.Context) (model.DeviceCapabilities, error) {
			return q.backend.Capabilities(ctx, device)
		})
		caps.Protocols = append([]model.InputProtocol(nil), caps.Protocols...)
		return outcome{capabilities: caps, err: err}
	})
	caps := result.capabilities
	caps.Protocols = append([]model.InputProtocol(nil), caps.Protocols...)
	return caps, err
}

func (q *Queue) ReadInput(ctx context.Context, id model.DisplayID, scheme model.InputScheme) (model.InputObservation, error) {
	if err := q.validateDisplayID(id); err != nil {
		return model.InputObservation{}, err
	}
	if err := scheme.Validate(); err != nil {
		return model.InputObservation{}, err
	}
	result, err := q.readJob(ctx, func(ctx context.Context) outcome {
		device, err := q.resolveDevice(ctx, id)
		if err != nil {
			return outcome{err: err}
		}
		observation, err := q.readObservation(ctx, device, scheme)
		return outcome{observation: cloneObservation(observation), err: err}
	})
	return cloneObservation(result.observation), err
}

func (q *Queue) readJob(ctx context.Context, run func(context.Context) outcome) (outcome, error) {
	operation, cancel := context.WithTimeout(ctx, model.DefaultRequestTimeout)
	defer cancel()
	j := &job{
		ctx: operation, cancel: cancel, run: run, done: make(chan struct{}),
		abort: func(err error) outcome { return outcome{err: err} },
	}
	if _, _, err := q.enqueue(j); err != nil {
		return outcome{}, err
	}
	<-j.done
	return j.result, j.result.err
}

func (q *Queue) enqueue(j *job) (*job, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, false, model.NewError(model.CodeBackendUnavailable, "后端访问队列已关闭", nil)
	}
	if err := j.ctx.Err(); err != nil {
		return nil, false, operationError(err)
	}
	if j.request != nil {
		now := time.Now()
		for id, cached := range q.switches {
			if cached.finished && !now.Before(cached.finishedAt.Add(dedupTTL)) {
				delete(q.switches, id)
			}
		}
		if cached, ok := q.switches[j.request.ID]; ok {
			if !sameRequest(*cached.request, *j.request) {
				return nil, false, model.NewError(model.CodeInvalidRequest, "相同请求ID不能用于不同切换内容", nil)
			}
			return cached, false, nil
		}
		if len(q.switches) >= dedupCapacity {
			return nil, false, model.NewError(model.CodeBackendUnavailable, "请求去重记录已满，请等待保留窗口结束并核对设备状态", nil)
		}
	}
	if len(q.jobs) >= cap(q.jobs) {
		return nil, false, model.NewError(model.CodeBackendUnavailable, "后端访问队列已满，未接收本次操作", nil)
	}
	j.stopShutdown = context.AfterFunc(q.life, j.cancel)
	j.stopCanceled = context.AfterFunc(j.ctx, func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		if !j.started && !j.finished {
			q.finishLocked(j, j.abort(operationError(j.ctx.Err())))
		}
	})
	if j.request != nil {
		q.switches[j.request.ID] = j
	}
	// 所有发送及close均受同一锁保护。工作协程只会取走元素，因此容量检查后
	// 发送不会阻塞，也不会与Close竞争而向已关闭通道发送。
	q.jobs <- j
	return j, true, nil
}

func (q *Queue) work() {
	defer close(q.done)
	for j := range q.jobs {
		q.mu.Lock()
		if j.finished {
			q.mu.Unlock()
			j.cleanup()
			continue
		}
		j.started = true
		q.mu.Unlock()
		if q.life.Err() != nil {
			j.cancel()
		}
		var result outcome
		if err := j.ctx.Err(); err != nil {
			result = j.abort(operationError(err))
		} else {
			result = j.run(j.ctx)
		}
		q.mu.Lock()
		q.finishLocked(j, result)
		q.mu.Unlock()
		j.cleanup()
	}
}

func (q *Queue) finishLocked(j *job, result outcome) {
	j.result = result
	j.finished = true
	j.finishedAt = time.Now()
	close(j.done)
}

func (j *job) cleanup() {
	j.stopShutdown()
	j.stopCanceled()
	j.cancel()
}

func (q *Queue) validateDisplayID(id model.DisplayID) error {
	if !model.ValidID(string(id)) {
		return model.NewError(model.CodeInvalidRequest, "读取操作需要有效逻辑显示器ID", nil)
	}
	for _, display := range q.cfg.Displays {
		if display.ID == id {
			return nil
		}
	}
	return model.NewError(model.CodeNotFound, "逻辑显示器不在当前配置中", nil)
}

func (q *Queue) resolveDevice(ctx context.Context, id model.DisplayID) (model.Device, error) {
	devices, err := retryRead(ctx, q.backend.Enumerate)
	if err != nil {
		return model.Device{}, err
	}
	// 必须传入完整配置和完整枚举。只解析所选屏会遗漏其他逻辑屏对同一物理
	// 设备的竞争，且不能复用其他任务保存的BackendID跨越新的枚举周期。
	resolutions, err := identity.Resolve(q.cfg.Displays, devices)
	if err != nil {
		return model.Device{}, err
	}
	for _, resolution := range resolutions {
		if resolution.DisplayID == id {
			if resolution.Error != nil {
				return model.Device{}, resolution.Error
			}
			if resolution.Device != nil {
				return *resolution.Device, nil
			}
		}
	}
	return model.Device{}, model.NewError(model.CodeNotFound, "当前枚举未能解析逻辑显示器", nil)
}

func (q *Queue) readObservation(ctx context.Context, device model.Device, scheme model.InputScheme) (model.InputObservation, error) {
	var latest model.InputObservation
	observation, err := retryRead(ctx, func(ctx context.Context) (model.InputObservation, error) {
		value, err := q.backend.ReadInput(ctx, device, scheme)
		// 后端可能已读到有效值，但在取消或清理时失败。后续安全重试若不再有
		// 观测，仍须保留之前取得的值及原时间，不能用重试时刻重写历史事实。
		if validObservation(value) && (!validObservation(latest) || !value.ObservedAt.Before(latest.ObservedAt)) {
			latest = cloneObservation(value)
		}
		return value, err
	})
	if err == nil && !validObservation(observation) {
		err = model.NewError(model.CodeTransport, "后端未提供有效输入观测及原始时间", nil)
	}
	if err != nil && validObservation(latest) {
		return latest, err
	}
	return observation, err
}

// 只读操作最多初次加两次重试；仅接受后端明确标记可重试的通路/传输失败。
// 总context在入队前已建立，不因重试刷新。此函数绝不用于SetInput，因为其
// 错误或零回执不能证明尚未发送，按类别重试可能把已生效操作重复提交。
func retryRead[T any](ctx context.Context, call func(context.Context) (T, error)) (T, error) {
	var value T
	var err error
	for attempt := 0; attempt < readAttempts; attempt++ {
		if ctx.Err() != nil {
			return value, operationError(errors.Join(err, ctx.Err()))
		}
		value, err = call(ctx)
		if ctx.Err() != nil {
			return value, operationError(errors.Join(err, ctx.Err()))
		}
		var classified *model.Error
		code := model.CodeOf(err)
		if err == nil || attempt == readAttempts-1 || !errors.As(err, &classified) || classified == nil || !classified.Retryable || (code != model.CodeUnreachable && code != model.CodeTransport) {
			return value, err
		}
		timer := time.NewTimer(readRetryWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return value, operationError(errors.Join(err, ctx.Err()))
		case <-timer.C:
		}
	}
	return value, err
}

func operationError(err error) *model.Error {
	var classified *model.Error
	if errors.As(err, &classified) && classified != nil && classified.Code == model.CodeOf(err) {
		copy := model.NewError(classified.Code, classified.Message, err)
		copy.Field = classified.Field
		copy.Retryable = classified.Retryable
		return copy
	}
	message := "后端操作失败"
	if model.CodeOf(err) == model.CodeCanceled {
		message = "操作已取消，保留已发生的逐屏事实"
	} else if model.CodeOf(err) == model.CodeTimeout {
		message = "操作总预算已到期，保留已发生的逐屏事实"
	}
	return model.NewError(model.CodeOf(err), message, err)
}

func sameRequest(a, b model.SwitchRequest) bool {
	if a.ID != b.ID || a.TargetID != b.TargetID || a.GroupID != b.GroupID || a.TimeoutMillis != b.TimeoutMillis || len(a.DisplayIDs) != len(b.DisplayIDs) {
		return false
	}
	for i, id := range a.DisplayIDs {
		if b.DisplayIDs[i] != id {
			return false
		}
	}
	return true
}

func cloneConfig(cfg config.Config) config.Config {
	copy := cfg
	copy.Displays = append([]model.Display(nil), cfg.Displays...)
	for i := range copy.Displays {
		if cfg.Displays[i].Binding != nil {
			binding := *cfg.Displays[i].Binding
			copy.Displays[i].Binding = &binding
		}
	}
	copy.InputSchemes = append([]model.InputScheme(nil), cfg.InputSchemes...)
	copy.Targets = append([]model.Target(nil), cfg.Targets...)
	for i := range copy.Targets {
		copy.Targets[i].Inputs = append([]model.TargetInput(nil), cfg.Targets[i].Inputs...)
	}
	copy.Groups = append([]model.Group(nil), cfg.Groups...)
	for i := range copy.Groups {
		copy.Groups[i].DisplayIDs = append([]model.DisplayID(nil), cfg.Groups[i].DisplayIDs...)
	}
	return copy
}

func cloneObservation(observation model.InputObservation) model.InputObservation {
	if observation.Value != nil {
		value := *observation.Value
		observation.Value = &value
	}
	return observation
}

func cloneResult(result model.SwitchResult) model.SwitchResult {
	result.Displays = append([]model.DisplayResult(nil), result.Displays...)
	if result.Error != nil {
		copy := *result.Error
		result.Error = &copy
	}
	for i := range result.Displays {
		display := &result.Displays[i]
		if display.SentAt != nil {
			sent := *display.SentAt
			display.SentAt = &sent
		}
		if display.Observation != nil {
			observation := cloneObservation(*display.Observation)
			display.Observation = &observation
		}
		if display.Error != nil {
			copy := *display.Error
			display.Error = &copy
		}
	}
	return result
}
