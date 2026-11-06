package windows

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

type vcpCall struct {
	handle uintptr
	code   uint8
	value  uint32
}

type fakeTransport struct {
	logical        []LogicalMonitor
	physical       map[string][]PhysicalMonitor
	enumerateErr   error
	openErrors     map[string]error
	destroyErrors  map[uintptr]error
	readErr        error
	writeErr       error
	readValue      uint32
	enumerateCalls int
	openCalls      int
	destroyed      map[uintptr]int
	reads          []vcpCall
	writes         []vcpCall
	onOpen         func()
	onRead         func()
	onWrite        func()
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		logical: []LogicalMonitor{{ID: "logical-a", Handle: 1}},
		physical: map[string][]PhysicalMonitor{
			"logical-a": {{
				ID: "physical-a", Handle: 101, Name: "LG test monitor",
				Identity: model.IdentityHints{Serial: "serial-a"},
				Capabilities: model.DeviceCapabilities{
					Protocols: []model.InputProtocol{model.ProtocolVCP}, CanReadInput: true,
				},
			}},
		},
		openErrors: make(map[string]error), destroyErrors: make(map[uintptr]error),
		destroyed: make(map[uintptr]int), readValue: 17,
	}
}

func (f *fakeTransport) EnumDisplayMonitors(context.Context) ([]LogicalMonitor, error) {
	f.enumerateCalls++
	return append([]LogicalMonitor(nil), f.logical...), f.enumerateErr
}

func (f *fakeTransport) GetPhysicalMonitors(_ context.Context, logical LogicalMonitor) ([]PhysicalMonitor, error) {
	f.openCalls++
	if f.onOpen != nil {
		f.onOpen()
	}
	return append([]PhysicalMonitor(nil), f.physical[logical.ID]...), f.openErrors[logical.ID]
}

func (f *fakeTransport) DestroyPhysicalMonitor(monitor PhysicalMonitor) error {
	f.destroyed[monitor.Handle]++
	return f.destroyErrors[monitor.Handle]
}

func (f *fakeTransport) GetVCPFeature(_ context.Context, monitor PhysicalMonitor, code uint8) (uint32, error) {
	f.reads = append(f.reads, vcpCall{handle: monitor.Handle, code: code})
	if f.onRead != nil {
		f.onRead()
	}
	return f.readValue, f.readErr
}

func (f *fakeTransport) SetVCPFeature(_ context.Context, monitor PhysicalMonitor, code uint8, value uint32) error {
	f.writes = append(f.writes, vcpCall{handle: monitor.Handle, code: code, value: value})
	if f.onWrite != nil {
		f.onWrite()
	}
	return f.writeErr
}

func standardScheme() model.InputScheme {
	return model.InputScheme{ID: "hdmi", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 18}
}

func enumerateOne(t *testing.T, backend *Backend) model.Device {
	t.Helper()
	devices, err := backend.Enumerate(context.Background())
	if err != nil || len(devices) != 1 {
		t.Fatalf("枚举单设备：devices=%v err=%v", devices, err)
	}
	return devices[0]
}

func requireCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	if actual := model.CodeOf(err); actual != code {
		t.Fatalf("错误分类=%q，期望%q：%v", actual, code, err)
	}
}

func TestStandardVCPUsesReacquiredPhysicalHandle(t *testing.T) {
	transport := newFakeTransport()
	backend := New(transport)
	device := enumerateOne(t, backend)
	if transport.destroyed[101] != 1 {
		t.Fatal("枚举返回前未释放物理句柄")
	}
	// 相同对象重新打开时句柄可以变化，控制不能复用枚举时已经释放的句柄。
	transport.physical["logical-a"][0].Handle = 202
	observation, err := backend.ReadInput(context.Background(), device, standardScheme())
	if err != nil || observation.Value == nil || *observation.Value != 17 || observation.ObservedAt.IsZero() {
		t.Fatalf("读回没有保留真实观测：%+v err=%v", observation, err)
	}
	transport.physical["logical-a"][0].Handle = 303
	receipt, err := backend.SetInput(context.Background(), device, standardScheme())
	if err != nil || receipt.SentAt.IsZero() {
		t.Fatalf("成功提交缺少发送时间：%+v err=%v", receipt, err)
	}
	if len(transport.reads) != 1 || transport.reads[0] != (vcpCall{handle: 202, code: 0x60}) {
		t.Fatalf("读回参数错误：%+v", transport.reads)
	}
	if len(transport.writes) != 1 || transport.writes[0] != (vcpCall{handle: 303, code: 0x60, value: 18}) {
		t.Fatalf("写入参数错误：%+v", transport.writes)
	}
	for _, handle := range []uintptr{101, 202, 303} {
		if transport.destroyed[handle] != 1 {
			t.Fatalf("句柄%d释放次数=%d", handle, transport.destroyed[handle])
		}
	}
}

func TestInvalidSchemeAndVendorPolicyPerformNoIO(t *testing.T) {
	tests := []struct {
		name   string
		scheme model.InputScheme
		code   model.ErrorCode
	}{
		{"missing-id", model.InputScheme{Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}, model.CodeInvalidRequest},
		{"wrong-vcp", model.InputScheme{ID: "x", Protocol: model.ProtocolVCP, VCPCode: 0x10, Value: 17}, model.CodeInvalidRequest},
		{"zero-value", model.InputScheme{ID: "x", Protocol: model.ProtocolVCP, VCPCode: 0x60}, model.CodeInvalidRequest},
		{"unknown-protocol", model.InputScheme{ID: "x", Protocol: "gpu-private", Value: 17}, model.CodeUnsupportedProtocol},
		{"invalid-lg-code", model.InputScheme{ID: "x", Protocol: model.ProtocolLGInputAlt, VCPCode: 0x60, Value: 17}, model.CodeInvalidRequest},
		{"explicit-lg-no-fallback", model.InputScheme{ID: "x", Protocol: model.ProtocolLGInputAlt, Value: 17}, model.CodeUnsupportedProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newFakeTransport()
			backend := New(transport)
			_, err := backend.ReadInput(context.Background(), model.Device{}, test.scheme)
			requireCode(t, err, test.code)
			receipt, err := backend.SetInput(context.Background(), model.Device{}, test.scheme)
			requireCode(t, err, test.code)
			if !receipt.SentAt.IsZero() || transport.enumerateCalls != 0 || transport.openCalls != 0 || len(transport.reads) != 0 || len(transport.writes) != 0 {
				t.Fatal("方案拒绝前发生了设备I/O或产生发送证据")
			}
		})
	}
}

func TestMultiplePhysicalMonitorsReleaseOnPartialFailure(t *testing.T) {
	transport := newFakeTransport()
	second := transport.physical["logical-a"][0]
	second.ID, second.Handle = "physical-b", 102
	transport.physical["logical-a"] = append(transport.physical["logical-a"], second)
	transport.logical = append(transport.logical, LogicalMonitor{ID: "logical-b", Handle: 2})
	third := second
	third.ID, third.Handle = "physical-c", 103
	transport.physical["logical-b"] = []PhysicalMonitor{third}
	partialFailure := errors.New("physical enumeration partially failed")
	transport.openErrors["logical-b"] = partialFailure
	devices, err := New(transport).Enumerate(context.Background())
	requireCode(t, err, model.CodeTransport)
	if len(devices) != 0 || !errors.Is(err, partialFailure) {
		t.Fatalf("枚举失败未保留原因或泄漏半份设备：%v %v", devices, err)
	}
	for _, handle := range []uintptr{101, 102, 103} {
		if transport.destroyed[handle] != 1 {
			t.Fatalf("部分失败后句柄%d释放次数=%d", handle, transport.destroyed[handle])
		}
	}
}

func TestSuccessfulMultiMonitorEnumerationClosesAllHandles(t *testing.T) {
	transport := newFakeTransport()
	second := transport.physical["logical-a"][0]
	second.ID, second.Handle, second.Identity.Serial = "physical-b", 102, "serial-b"
	transport.physical["logical-a"] = append(transport.physical["logical-a"], second)
	backend := New(transport)
	devices, err := backend.Enumerate(context.Background())
	if err != nil || len(devices) != 2 || devices[0].BackendID == devices[1].BackendID {
		t.Fatalf("多物理屏枚举错误：%v %v", devices, err)
	}
	if transport.destroyed[101] != 1 || transport.destroyed[102] != 1 {
		t.Fatalf("多物理屏句柄未闭合：%v", transport.destroyed)
	}
	if _, err = backend.SetInput(context.Background(), devices[1], standardScheme()); err != nil {
		t.Fatal(err)
	}
	if len(transport.writes) != 1 || transport.writes[0].handle != 102 || transport.destroyed[101] != 2 || transport.destroyed[102] != 2 {
		t.Fatalf("控制错误物理屏或遗留其他句柄：writes=%v closed=%v", transport.writes, transport.destroyed)
	}
}

func TestTransportErrorsPreserveClassificationAndCause(t *testing.T) {
	tests := []struct {
		name  string
		cause error
		code  model.ErrorCode
	}{
		{"permission", fs.ErrPermission, model.CodePermissionDenied},
		{"unreachable", model.NewError(model.CodeUnreachable, "设备通路断开", nil), model.CodeUnreachable},
		{"unavailable", model.NewError(model.CodeBackendUnavailable, "传输不可用", nil), model.CodeBackendUnavailable},
		{"unsupported", errors.ErrUnsupported, model.CodeUnsupportedOperation},
		{"canceled", fmt.Errorf("native wait: %w", context.Canceled), model.CodeCanceled},
		{"timeout", context.DeadlineExceeded, model.CodeTimeout},
		{"transport", errors.New("native failure"), model.CodeTransport},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newFakeTransport()
			backend := New(transport)
			device := enumerateOne(t, backend)
			transport.writeErr = test.cause
			receipt, err := backend.SetInput(context.Background(), device, standardScheme())
			requireCode(t, err, test.code)
			if !errors.Is(err, test.cause) || !receipt.SentAt.IsZero() || len(transport.writes) != 1 || transport.destroyed[101] != 2 {
				t.Fatalf("失败丢失原因、伪造发送证据、盲重试或未释放：receipt=%v err=%v writes=%v closed=%v", receipt, err, transport.writes, transport.destroyed)
			}
		})
	}
}

func TestPrimaryAndCleanupFailuresRemainDiagnosable(t *testing.T) {
	transport := newFakeTransport()
	backend := New(transport)
	device := enumerateOne(t, backend)
	writeFailure, cleanupFailure := errors.New("write may have been submitted"), errors.New("destroy failed")
	transport.writeErr, transport.destroyErrors[101] = writeFailure, cleanupFailure
	receipt, err := backend.SetInput(context.Background(), device, standardScheme())
	if !errors.Is(err, writeFailure) || !errors.Is(err, cleanupFailure) || !receipt.SentAt.IsZero() || transport.destroyed[101] != 2 {
		t.Fatalf("主错误或释放错误被吞掉：receipt=%v err=%v closed=%v", receipt, err, transport.destroyed)
	}
	transport.writeErr = nil
	receipt, err = backend.SetInput(context.Background(), device, standardScheme())
	if !errors.Is(err, cleanupFailure) || !receipt.SentAt.IsZero() || len(transport.writes) != 2 {
		t.Fatalf("发送后释放失败被报告为成功：receipt=%v err=%v", receipt, err)
	}
}

func TestCleanupFailureStillClosesRemainingPhysicalMonitors(t *testing.T) {
	transport := newFakeTransport()
	second := transport.physical["logical-a"][0]
	second.ID, second.Handle = "physical-b", 102
	transport.physical["logical-a"] = append(transport.physical["logical-a"], second)
	firstFailure, secondFailure := errors.New("destroy first failed"), errors.New("destroy second failed")
	transport.destroyErrors[101], transport.destroyErrors[102] = firstFailure, secondFailure
	devices, err := New(transport).Enumerate(context.Background())
	if len(devices) != 0 || !errors.Is(err, firstFailure) || !errors.Is(err, secondFailure) || transport.destroyed[101] != 1 || transport.destroyed[102] != 1 {
		t.Fatalf("释放失败阻止后续清理或丢失原因：devices=%v err=%v closed=%v", devices, err, transport.destroyed)
	}
}

func TestReadFailureHasNoInventedObservation(t *testing.T) {
	transport := newFakeTransport()
	backend := New(transport)
	device := enumerateOne(t, backend)
	readFailure := model.NewError(model.CodeUnreachable, "切走输入后通路失联", nil)
	transport.readErr = readFailure
	observation, err := backend.ReadInput(context.Background(), device, standardScheme())
	requireCode(t, err, model.CodeUnreachable)
	if !errors.Is(err, readFailure) || observation.Value != nil || !observation.ObservedAt.IsZero() || len(transport.reads) != 1 || transport.destroyed[101] != 2 {
		t.Fatalf("失败被当作读回成功或没有释放：observation=%v err=%v closed=%v", observation, err, transport.destroyed)
	}
}

func TestEnumerationFailureDoesNotPublishDevices(t *testing.T) {
	transport := newFakeTransport()
	backend := New(transport)
	oldDevice := enumerateOne(t, backend)
	cleanupFailure := errors.New("enumeration close failed")
	transport.destroyErrors[101] = cleanupFailure
	devices, err := backend.Enumerate(context.Background())
	if !errors.Is(err, cleanupFailure) || len(devices) != 0 {
		t.Fatalf("释放失败仍发布设备：devices=%v err=%v", devices, err)
	}
	before := transport.enumerateCalls
	_, err = backend.SetInput(context.Background(), oldDevice, standardScheme())
	requireCode(t, err, model.CodeNotFound)
	if transport.enumerateCalls != before {
		t.Fatal("失效的枚举周期仍进行了设备I/O")
	}
	transport.enumerateErr = fs.ErrPermission
	_, err = backend.Enumerate(context.Background())
	requireCode(t, err, model.CodePermissionDenied)
}

func TestCancellationBeforeAndDuringAcquisition(t *testing.T) {
	transport := newFakeTransport()
	backend := New(transport)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := backend.Enumerate(ctx)
	requireCode(t, err, model.CodeCanceled)
	if transport.enumerateCalls != 0 {
		t.Fatal("已取消仍调用枚举")
	}
	device := enumerateOne(t, backend)
	ctx, cancel = context.WithCancel(context.Background())
	transport.onOpen = cancel
	_, err = backend.ReadInput(ctx, device, standardScheme())
	if !errors.Is(err, context.Canceled) || transport.destroyed[101] != 2 || len(transport.reads) != 0 {
		t.Fatalf("取得句柄时取消没有停止读回或清理：err=%v closed=%v", err, transport.destroyed)
	}
}

func TestCancellationAfterSynchronousIOWaitsForReturnAndCleansUp(t *testing.T) {
	transport := newFakeTransport()
	backend := New(transport)
	device := enumerateOne(t, backend)
	ctx, cancel := context.WithCancel(context.Background())
	transport.onWrite = func() {
		cancel()
		if transport.destroyed[101] != 1 {
			t.Fatal("同步调用尚未返回就释放了使用中的句柄")
		}
	}
	receipt, err := backend.SetInput(ctx, device, standardScheme())
	if !errors.Is(err, context.Canceled) || !receipt.SentAt.IsZero() || len(transport.writes) != 1 || transport.destroyed[101] != 2 {
		t.Fatalf("取消后发送不确定性或清理错误：receipt=%v err=%v closed=%v", receipt, err, transport.destroyed)
	}
	ctx, cancel = context.WithCancel(context.Background())
	transport.onRead = cancel
	observation, err := backend.ReadInput(ctx, device, standardScheme())
	if !errors.Is(err, context.Canceled) || observation.Value == nil || *observation.Value != 17 || observation.ObservedAt.IsZero() || transport.destroyed[101] != 3 {
		t.Fatalf("读回后取消丢失有效观测或未清理：observation=%v err=%v", observation, err)
	}
}

func TestDeviceCapabilitiesAndReadUnsupported(t *testing.T) {
	transport := newFakeTransport()
	backend := New(transport)
	device := enumerateOne(t, backend)
	transport.physical["logical-a"][0].Capabilities = model.DeviceCapabilities{
		Protocols: []model.InputProtocol{model.ProtocolVCP, model.ProtocolLGInputAlt},
	}
	capabilities, err := backend.Capabilities(context.Background(), device)
	if err != nil || len(capabilities.Protocols) != 1 || capabilities.Protocols[0] != model.ProtocolVCP || capabilities.CanReadInput {
		t.Fatalf("能力没有按已实现策略约束：%v %v", capabilities, err)
	}
	_, err = backend.ReadInput(context.Background(), device, standardScheme())
	requireCode(t, err, model.CodeUnsupportedOperation)
	if len(transport.reads) != 0 {
		t.Fatal("不支持读回仍发送读回命令")
	}
	transport.physical["logical-a"][0].Capabilities = model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolLGInputAlt}, CanReadInput: true}
	capabilities, err = backend.Capabilities(context.Background(), device)
	if err != nil || len(capabilities.Protocols) != 0 || capabilities.CanReadInput || capabilities.UnavailableReason == "" {
		t.Fatalf("无策略设备缺少不可用说明：%v %v", capabilities, err)
	}
	_, err = backend.SetInput(context.Background(), device, standardScheme())
	requireCode(t, err, model.CodeUnsupportedProtocol)
	if len(transport.writes) != 0 {
		t.Fatal("未报告VCP能力仍进行了发送")
	}
}

func TestDeviceCycleAndIdentityMustStillMatch(t *testing.T) {
	transport := newFakeTransport()
	backend := New(transport)
	oldDevice := enumerateOne(t, backend)
	device := enumerateOne(t, backend)
	if oldDevice.BackendID == device.BackendID {
		t.Fatal("新的枚举周期复用了旧设备ID")
	}
	before := transport.enumerateCalls
	_, err := backend.SetInput(context.Background(), oldDevice, standardScheme())
	requireCode(t, err, model.CodeNotFound)
	if transport.enumerateCalls != before {
		t.Fatal("旧设备引用触发了I/O")
	}
	transport.physical["logical-a"][0].Identity.Serial = "replacement"
	_, err = backend.SetInput(context.Background(), device, standardScheme())
	requireCode(t, err, model.CodeUnreachable)
	if len(transport.writes) != 0 || transport.destroyed[101] != 3 {
		t.Fatal("对象替换后仍写入或没有释放重新取得的句柄")
	}
	// 同一传输交给另一后端实例，也不能让旧实例的临时ID通过检查。
	other := New(transport)
	otherDevice := enumerateOne(t, other)
	if otherDevice.BackendID == device.BackendID {
		t.Fatal("不同后端实例生成了相同临时设备ID")
	}
}

func TestMissingObjectDoesNotSelectAnotherMonitor(t *testing.T) {
	transport := newFakeTransport()
	backend := New(transport)
	device := enumerateOne(t, backend)
	transport.physical["logical-a"][0].ID = "different-object"
	_, err := backend.SetInput(context.Background(), device, standardScheme())
	requireCode(t, err, model.CodeUnreachable)
	if len(transport.writes) != 0 || transport.destroyed[101] != 2 {
		t.Fatalf("原对象消失后选择了其他候选或未释放：writes=%v closed=%v", transport.writes, transport.destroyed)
	}
}

func TestDuplicateIdentityAndHandleNeverDoubleDestroy(t *testing.T) {
	for _, duplicateHandle := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicateHandle), func(t *testing.T) {
			transport := newFakeTransport()
			second := transport.physical["logical-a"][0]
			if !duplicateHandle {
				second.Handle = 102
			}
			transport.physical["logical-a"] = append(transport.physical["logical-a"], second)
			_, err := New(transport).Enumerate(context.Background())
			if err == nil || transport.destroyed[101] != 1 || (!duplicateHandle && transport.destroyed[102] != 1) {
				t.Fatalf("重复资源/标识未拒绝或释放次数错误：err=%v closed=%v", err, transport.destroyed)
			}
		})
	}
}

func TestOutOfRangeReadAndMissingTransport(t *testing.T) {
	transport := newFakeTransport()
	backend := New(transport)
	device := enumerateOne(t, backend)
	transport.readValue = 65536
	observation, err := backend.ReadInput(context.Background(), device, standardScheme())
	requireCode(t, err, model.CodeTransport)
	if observation.Value != nil || transport.destroyed[101] != 2 {
		t.Fatal("越界值被截断为有效读回或句柄未释放")
	}
	_, err = New(nil).Enumerate(context.Background())
	requireCode(t, err, model.CodeBackendUnavailable)
}
