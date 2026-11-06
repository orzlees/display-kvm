package switcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

type recordedWrite struct {
	device model.Device
	scheme model.InputScheme
}

// 每次枚举都使旧引用失效，模拟Windows后端的代次约束；仅断言SetInput互斥
// 无法发现诊断枚举在发送期间破坏当前引用的问题，因此所有后端入口都计入并发检查。
type fakeBackend struct {
	mu         sync.Mutex
	generation int
	active     int
	concurrent bool
	stale      bool
	writes     []recordedWrite
	reads      map[string]int
	readHook   func(context.Context, model.Device, model.InputScheme, int) (model.InputObservation, error)
	writeHook  func(context.Context, model.Device, model.InputScheme) (model.WriteReceipt, error)
	capsHook   func(model.Device) (model.DeviceCapabilities, error)
	enumHook   func(context.Context, int) error
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) enter(device *model.Device) (func(), error) {
	f.mu.Lock()
	f.active++
	f.concurrent = f.concurrent || f.active > 1
	var err error
	if device != nil && device.BackendID != fmt.Sprintf("%d:%s", f.generation, device.Identity.Path) {
		f.stale = true
		err = model.NewError(model.CodeNotFound, "设备引用已过期", nil)
	}
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}, err
}

func (f *fakeBackend) Enumerate(ctx context.Context) ([]model.Device, error) {
	leave, _ := f.enter(nil)
	defer leave()
	f.mu.Lock()
	f.generation++
	generation, hook := f.generation, f.enumHook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, generation); err != nil {
			return nil, err
		}
	}
	devices := make([]model.Device, 0, 2)
	for _, name := range []string{"left", "right"} {
		devices = append(devices, model.Device{
			BackendID: fmt.Sprintf("%d:/%s", generation, name),
			Identity:  model.IdentityHints{Manufacturer: "m", ProductCode: "p", Serial: name, Path: "/" + name},
		})
	}
	return devices, nil
}

func (f *fakeBackend) Capabilities(ctx context.Context, device model.Device) (model.DeviceCapabilities, error) {
	leave, err := f.enter(&device)
	defer leave()
	if err != nil {
		return model.DeviceCapabilities{}, err
	}
	if f.capsHook != nil {
		return f.capsHook(device)
	}
	return model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolVCP}, CanReadInput: true}, nil
}

func (f *fakeBackend) ReadInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.InputObservation, error) {
	leave, err := f.enter(&device)
	defer leave()
	if err != nil {
		return model.InputObservation{}, err
	}
	f.mu.Lock()
	if f.reads == nil {
		f.reads = make(map[string]int)
	}
	f.reads[device.Identity.Path]++
	call := f.reads[device.Identity.Path]
	f.mu.Unlock()
	if f.readHook != nil {
		return f.readHook(ctx, device, scheme, call)
	}
	value := scheme.Value
	return model.InputObservation{Value: &value, ObservedAt: time.Now()}, nil
}

func (f *fakeBackend) SetInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
	leave, err := f.enter(&device)
	defer leave()
	if err != nil {
		return model.WriteReceipt{}, err
	}
	f.mu.Lock()
	f.writes = append(f.writes, recordedWrite{device, scheme})
	f.mu.Unlock()
	if f.writeHook != nil {
		return f.writeHook(ctx, device, scheme)
	}
	return model.WriteReceipt{SentAt: time.Now()}, nil
}

func switchConfig() config.Config {
	return config.Config{
		Version: config.CurrentVersion,
		Displays: []model.Display{
			{ID: "left", Identity: model.IdentityHints{Serial: "left"}, Binding: &model.Binding{Path: "/left"}},
			{ID: "right", Identity: model.IdentityHints{Serial: "right"}, Binding: &model.Binding{Path: "/right"}},
		},
		InputSchemes: []model.InputScheme{
			{ID: "left-port", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17},
			{ID: "right-port", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 22},
		},
		Targets: []model.Target{{ID: "work", Inputs: []model.TargetInput{{DisplayID: "left", SchemeID: "left-port"}, {DisplayID: "right", SchemeID: "right-port"}}}},
		Groups:  []model.Group{{ID: "desk", DisplayIDs: []model.DisplayID{"left", "right"}}},
	}
}

func newTestQueue(t *testing.T, f *fakeBackend, cfg config.Config) *Queue {
	t.Helper()
	q, err := New(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func onlyLeft(id model.RequestID) model.SwitchRequest {
	return model.SwitchRequest{ID: id, TargetID: "work", DisplayIDs: []model.DisplayID{"left"}}
}

func checkedSwitch(t *testing.T, q *Queue, req model.SwitchRequest) model.SwitchResult {
	t.Helper()
	result, err := q.Switch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("结果不满足公共契约: %v", err)
	}
	return result
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("等待队列清理或结果超时")
		var zero T
		return zero
	}
}

func writeCount(f *fakeBackend) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func awaitQueued(t *testing.T, q *Queue, count int) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		q.mu.Lock()
		pending := len(q.jobs)
		q.mu.Unlock()
		if pending >= count {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("诊断操作未全部进入共享队列")
		}
	}
}

func TestMappingSnapshotAndCachedResultIsolation(t *testing.T) {
	f := &fakeBackend{}
	cfg := switchConfig()
	q := newTestQueue(t, f, cfg)
	// 构造后编辑原切片及嵌套指针，不能改变队列已接收的配置快照。
	cfg.Displays[0].Binding.Path = "/wrong"
	cfg.Targets[0].Inputs[0].SchemeID = "right-port"
	cfg.Groups[0].DisplayIDs[0] = "right"
	cfg.InputSchemes[0].Value = 99
	req := model.SwitchRequest{ID: "mapping", TargetID: "work", GroupID: "desk"}
	result := checkedSwitch(t, q, req)
	if len(result.Displays) != 2 || result.Displays[0].DisplayID != "left" || result.Displays[1].DisplayID != "right" {
		t.Fatalf("分组顺序或逐屏集合错误: %+v", result)
	}
	for i, value := range []uint16{17, 22} {
		if result.Displays[i].DesiredValue != value || result.Displays[i].State != model.StateConfirmed || f.writes[i].scheme.Value != value {
			t.Fatalf("未使用各屏独立映射: %+v", result.Displays[i])
		}
	}
	*result.Displays[0].Observation.Value = 999
	*result.Displays[0].SentAt = time.Time{}
	cached := checkedSwitch(t, q, req)
	if cached.Displays[0].State != model.StateConfirmed || *cached.Displays[0].Observation.Value != 17 || writeCount(f) != 2 {
		t.Fatal("返回值污染去重缓存或相同ID重复写入")
	}
}

func TestPreflightRejectsEntireSelectionBeforeWrite(t *testing.T) {
	t.Run("missing_mapping", func(t *testing.T) {
		f := &fakeBackend{}
		cfg := switchConfig()
		cfg.Targets[0].Inputs = cfg.Targets[0].Inputs[:1]
		q := newTestQueue(t, f, cfg)
		_, err := q.Switch(context.Background(), model.SwitchRequest{ID: "missing", TargetID: "work", GroupID: "desk"})
		if model.CodeOf(err) != model.CodeInvalidRequest || writeCount(f) != 0 {
			t.Fatalf("缺映射未整请求拒绝: %v", err)
		}
	})
	t.Run("ambiguous_identity", func(t *testing.T) {
		f := &fakeBackend{}
		cfg := switchConfig()
		cfg.Displays[1].Binding = nil
		cfg.Displays[1].Identity = model.IdentityHints{}
		q := newTestQueue(t, f, cfg)
		result := checkedSwitch(t, q, model.SwitchRequest{ID: "ambiguous", TargetID: "work"})
		if result.Error == nil || result.Error.Code != model.CodeIdentityAmbiguous || len(result.Displays) != 2 || writeCount(f) != 0 {
			t.Fatalf("歧义未阻止整请求: %+v", result)
		}
		for _, display := range result.Displays {
			if display.State != model.StateFailed || display.SentAt != nil {
				t.Fatal("身份拒绝被伪装成发送")
			}
		}
	})
}

func TestPartialFailureAndUncertainSubmission(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			f := &fakeBackend{}
			if uncertain {
				f.writeHook = func(ctx context.Context, d model.Device, s model.InputScheme) (model.WriteReceipt, error) {
					if d.Identity.Path == "/right" {
						err := model.NewError(model.CodeTransport, "提交后连接中断", nil)
						err.Retryable = true
						return model.WriteReceipt{SentAt: time.Now()}, err
					}
					return model.WriteReceipt{SentAt: time.Now()}, nil
				}
			} else {
				f.capsHook = func(d model.Device) (model.DeviceCapabilities, error) {
					if d.Identity.Path == "/right" {
						return model.DeviceCapabilities{}, model.NewError(model.CodePermissionDenied, "明确在写入前拒绝", nil)
					}
					return model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolVCP}, CanReadInput: true}, nil
				}
			}
			q := newTestQueue(t, f, switchConfig())
			result := checkedSwitch(t, q, model.SwitchRequest{ID: "partial", TargetID: "work"})
			if result.Error != nil || result.Displays[0].State != model.StateConfirmed {
				t.Fatalf("其他屏成功证据丢失: %+v", result)
			}
			want, count := model.StateFailed, 1
			if uncertain {
				want, count = model.StateUnknown, 2
			}
			if result.Displays[1].State != want || result.Displays[1].SentAt != nil || writeCount(f) != count {
				t.Fatalf("失败被误判或执行重发/回滚: %+v", result)
			}
		})
	}
}

func TestLostChannelRetainsOldObservationAndBoundsReadRetries(t *testing.T) {
	f := &fakeBackend{}
	oldTime := time.Now().Add(-time.Minute)
	f.readHook = func(ctx context.Context, d model.Device, s model.InputScheme, call int) (model.InputObservation, error) {
		if call == 1 {
			value := uint16(3)
			return model.InputObservation{Value: &value, ObservedAt: oldTime}, nil
		}
		err := model.NewError(model.CodeUnreachable, "切走后通道不可达", nil)
		err.Retryable = true
		return model.InputObservation{}, err
	}
	q := newTestQueue(t, f, switchConfig())
	result := checkedSwitch(t, q, onlyLeft("lost"))
	d := result.Displays[0]
	if d.State != model.StateUnknown || d.SentAt == nil || d.Error.Code != model.CodeUnreachable || d.Observation == nil || *d.Observation.Value != 3 || !d.Observation.ObservedAt.Equal(oldTime) {
		t.Fatalf("失联未保留未知和历史事实: %+v", d)
	}
	if f.reads["/left"] != 4 || writeCount(f) != 1 {
		t.Fatal("读重试未限制为初次加两次，或发生额外写入")
	}
}

func TestConfirmationRequiresFreshMatchingObservation(t *testing.T) {
	for _, mode := range []string{"stale", "different", "no_read", "zero_receipt"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeBackend{}
			if mode == "no_read" {
				f.capsHook = func(model.Device) (model.DeviceCapabilities, error) {
					return model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolVCP}}, nil
				}
			} else if mode == "zero_receipt" {
				f.writeHook = func(context.Context, model.Device, model.InputScheme) (model.WriteReceipt, error) {
					return model.WriteReceipt{}, nil
				}
			} else {
				f.readHook = func(ctx context.Context, d model.Device, s model.InputScheme, call int) (model.InputObservation, error) {
					value, observed := s.Value, time.Now()
					if mode == "stale" {
						observed = observed.Add(-time.Hour)
					} else {
						value++
					}
					return model.InputObservation{Value: &value, ObservedAt: observed}, nil
				}
			}
			q := newTestQueue(t, f, switchConfig())
			result := checkedSwitch(t, q, onlyLeft(model.RequestID(mode)))
			if result.Displays[0].State != model.StateUnknown || writeCount(f) != 1 {
				t.Fatalf("不足证据被确认或重发: %+v", result)
			}
			if mode == "zero_receipt" && result.Displays[0].SentAt != nil {
				t.Fatal("零回执被补造发送证据")
			}
		})
	}
}

func TestInFlightDedupAndIndependentWaiterCancellation(t *testing.T) {
	f := &fakeBackend{}
	started, release := make(chan struct{}, 1), make(chan struct{})
	f.writeHook = func(ctx context.Context, d model.Device, s model.InputScheme) (model.WriteReceipt, error) {
		started <- struct{}{}
		select {
		case <-release:
			return model.WriteReceipt{SentAt: time.Now()}, nil
		case <-ctx.Done():
			return model.WriteReceipt{}, ctx.Err()
		}
	}
	q := newTestQueue(t, f, switchConfig())
	req := onlyLeft("shared")
	owner := make(chan model.SwitchResult, 1)
	go func() {
		r, _ := q.Switch(context.Background(), req)
		owner <- r
	}()
	await(t, started)
	different := req
	different.DisplayIDs = []model.DisplayID{"right"}
	if _, err := q.Switch(context.Background(), different); model.CodeOf(err) != model.CodeInvalidRequest {
		t.Fatalf("相同ID不同内容未拒绝: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := q.Switch(waitCtx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("在途重复等待者应共享执行并独立结束等待: %v", err)
	}
	other := make(chan model.SwitchResult, 1)
	go func() {
		r, _ := q.Switch(context.Background(), req)
		other <- r
	}()
	close(release)
	for _, ch := range []chan model.SwitchResult{owner, other} {
		if r := await(t, ch); len(r.Displays) != 1 || r.Displays[0].State != model.StateConfirmed {
			t.Fatalf("重复等待者取消影响原请求: %+v", r)
		}
	}
	if writeCount(f) != 1 {
		t.Fatal("在途相同请求重复发送")
	}
}

func TestQueueBudgetExpiresWithoutWaitingForActiveWrite(t *testing.T) {
	f := &fakeBackend{}
	started, release := make(chan struct{}, 1), make(chan struct{})
	f.writeHook = func(ctx context.Context, d model.Device, s model.InputScheme) (model.WriteReceipt, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return model.WriteReceipt{}, ctx.Err()
		case <-release:
			return model.WriteReceipt{SentAt: time.Now()}, nil
		}
	}
	q := newTestQueue(t, f, switchConfig())
	active := make(chan model.SwitchResult, 1)
	go func() {
		r, _ := q.Switch(context.Background(), onlyLeft("active"))
		active <- r
	}()
	await(t, started)
	queued := onlyLeft("queued")
	queued.TimeoutMillis = 50
	expired := make(chan model.SwitchResult, 1)
	go func() {
		r, _ := q.Switch(context.Background(), queued)
		expired <- r
	}()
	r := await(t, expired)
	if r.Error == nil || r.Error.Code != model.CodeTimeout || len(r.Displays) != 1 || r.Displays[0].State != model.StateFailed || writeCount(f) != 1 {
		t.Fatalf("排队预算被刷新或取消后发送: %+v", r)
	}
	close(release)
	await(t, active)
	checkedSwitch(t, q, queued)
	if writeCount(f) != 1 {
		t.Fatal("缓存中的超时请求被重新发送")
	}
}

func TestOwnerCancellationPreservesPartialResultsAndCloseCleansUp(t *testing.T) {
	f := &fakeBackend{}
	started := make(chan struct{}, 1)
	f.writeHook = func(ctx context.Context, d model.Device, s model.InputScheme) (model.WriteReceipt, error) {
		started <- struct{}{}
		<-ctx.Done()
		return model.WriteReceipt{}, ctx.Err()
	}
	q := newTestQueue(t, f, switchConfig())
	results := make(chan model.SwitchResult, 1)
	go func() {
		r, _ := q.Switch(context.Background(), model.SwitchRequest{ID: "closing", TargetID: "work"})
		results <- r
	}()
	await(t, started)
	closed := make(chan error, 1)
	go func() { closed <- q.Close() }()
	if err := await(t, closed); err != nil {
		t.Fatal(err)
	}
	r := await(t, results)
	if err := r.Validate(); err != nil || len(r.Displays) != 2 || r.Displays[0].State != model.StateUnknown || r.Displays[1].State != model.StateFailed || writeCount(f) != 1 {
		t.Fatalf("取消后丢失逐屏依据或继续发送: %+v, %v", r, err)
	}
	if _, err := q.Enumerate(context.Background()); model.CodeOf(err) != model.CodeBackendUnavailable {
		t.Fatalf("关闭后仍允许访问后端: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCallerCancellationStopsUnsentDisplays(t *testing.T) {
	f := &fakeBackend{}
	started := make(chan struct{}, 1)
	f.writeHook = func(ctx context.Context, d model.Device, s model.InputScheme) (model.WriteReceipt, error) {
		started <- struct{}{}
		<-ctx.Done()
		return model.WriteReceipt{}, ctx.Err()
	}
	q := newTestQueue(t, f, switchConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan model.SwitchResult, 1)
	go func() {
		r, _ := q.Switch(ctx, model.SwitchRequest{ID: "caller-cancel", TargetID: "work"})
		results <- r
	}()
	await(t, started)
	cancel()
	r := await(t, results)
	if err := r.Validate(); err != nil || len(r.Displays) != 2 || r.Displays[0].State != model.StateUnknown || r.Displays[1].State != model.StateFailed || writeCount(f) != 1 {
		t.Fatalf("调用者取消后继续发送或丢失部分结果: %+v, %v", r, err)
	}
	if r.Error == nil || !errors.Is(r.Error, context.Canceled) || !errors.Is(r.Displays[0].Error, context.Canceled) {
		t.Fatal("逐屏取消未保留context根因")
	}
}

func TestSafeReadRetriesAndPreservesObservationOnError(t *testing.T) {
	f := &fakeBackend{}
	f.enumHook = func(ctx context.Context, generation int) error {
		if generation == 1 {
			err := model.NewError(model.CodeTransport, "枚举暂时失败", nil)
			err.Retryable = true
			return err
		}
		return nil
	}
	observedAt := time.Now()
	f.readHook = func(ctx context.Context, d model.Device, s model.InputScheme, call int) (model.InputObservation, error) {
		err := model.NewError(model.CodeTransport, "读回后清理失败", nil)
		err.Retryable = true
		if call == 1 {
			value := uint16(7)
			return model.InputObservation{Value: &value, ObservedAt: observedAt}, err
		}
		return model.InputObservation{}, err
	}
	q := newTestQueue(t, f, switchConfig())
	observation, err := q.ReadInput(context.Background(), "left", switchConfig().InputSchemes[0])
	if model.CodeOf(err) != model.CodeTransport || observation.Value == nil || *observation.Value != 7 || !observation.ObservedAt.Equal(observedAt) || f.generation != 2 || f.reads["/left"] != 3 || writeCount(f) != 0 {
		t.Fatalf("安全重试丢失观测或越界: %+v, %v", observation, err)
	}
}

func TestDiagnosticsAndConcurrentSwitchesShareOneOwner(t *testing.T) {
	f := &fakeBackend{}
	started, release := make(chan struct{}, 1), make(chan struct{})
	var first sync.Once
	f.writeHook = func(ctx context.Context, d model.Device, s model.InputScheme) (model.WriteReceipt, error) {
		blocked := false
		first.Do(func() {
			blocked = true
			started <- struct{}{}
		})
		if blocked {
			select {
			case <-ctx.Done():
				return model.WriteReceipt{}, ctx.Err()
			case <-release:
			}
		}
		return model.WriteReceipt{SentAt: time.Now()}, nil
	}
	q := newTestQueue(t, f, switchConfig())
	finished := make(chan error, 24)
	go func() {
		_, err := q.Switch(context.Background(), onlyLeft("first"))
		finished <- err
	}()
	await(t, started)
	go func() {
		_, err := q.Enumerate(context.Background())
		finished <- err
	}()
	go func() {
		_, err := q.Capabilities(context.Background(), "right")
		finished <- err
	}()
	go func() {
		_, err := q.ReadInput(context.Background(), "right", switchConfig().InputSchemes[1])
		finished <- err
	}()
	for i := 0; i < 20; i++ {
		go func(i int) {
			_, err := q.Switch(context.Background(), onlyLeft(model.RequestID(fmt.Sprintf("parallel-%d", i))))
			finished <- err
		}(i)
	}
	awaitQueued(t, q, 23)
	close(release)
	for i := 0; i < 24; i++ {
		if err := await(t, finished); err != nil {
			t.Fatal(err)
		}
	}
	if f.concurrent || f.stale || writeCount(f) != 21 {
		t.Fatalf("诊断与写入争用或使用旧引用: concurrent=%v stale=%v writes=%d", f.concurrent, f.stale, writeCount(f))
	}
}

func TestLogicalReadsResolveAgainstCompleteConfiguration(t *testing.T) {
	f := &fakeBackend{}
	cfg := switchConfig()
	for i := range cfg.Displays {
		cfg.Displays[i].Binding = nil
		cfg.Displays[i].Identity = model.IdentityHints{Manufacturer: "m", ProductCode: "p", Serial: "left", Path: "/left"}
	}
	q := newTestQueue(t, f, cfg)
	if _, err := q.ReadInput(context.Background(), "left", cfg.InputSchemes[0]); model.CodeOf(err) != model.CodeIdentityAmbiguous {
		t.Fatalf("只解析所选屏忽略全配置身份竞争: %v", err)
	}
	if len(f.reads) != 0 || writeCount(f) != 0 {
		t.Fatal("歧义读取触及目标设备")
	}
}

func TestDedupCapacityRejectsWithoutEvictingLiveResults(t *testing.T) {
	f := &fakeBackend{}
	q := newTestQueue(t, f, switchConfig())
	for i := 0; i < 256; i++ {
		checkedSwitch(t, q, onlyLeft(model.RequestID(fmt.Sprintf("cache-%d", i))))
	}
	if _, err := q.Switch(context.Background(), onlyLeft("overflow")); model.CodeOf(err) != model.CodeBackendUnavailable {
		t.Fatalf("缓存无界增长或提前逐出有效记录: %v", err)
	}
	checkedSwitch(t, q, onlyLeft("cache-0"))
	if writeCount(f) != 256 {
		t.Fatal("容量耗尽后重复执行已有ID")
	}
}
