package windows

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

// 测试替换原生API而不是Transport；身份映射、能力解析与句柄归属均执行产品适配代码。
type fakeNativeAPI struct {
	paths       []nativeDisplayPath
	sources     map[nativeDisplayID]string
	targets     map[nativeDisplayID]nativeTargetName
	monitors    []uintptr
	gdi         map[uintptr]string
	physical    map[uintptr][]nativePhysicalMonitor
	caps        map[uintptr][]byte
	capErrors   map[uintptr]error
	replyErrors map[uintptr]error
	openErrors  map[uintptr]error
	countErrors map[uintptr]error
	destroyErrs map[uintptr]error
	lengths     map[uintptr]uint32
	destroyed   map[uintptr]int
	reads       []vcpCall
	writes      []vcpCall
	queryErrors []error
	queryCalls  int
	probeErr    error
	readErr     error
	writeErr    error
	readValue   uint32
	readMaximum uint32
	onRead      func()
	onWrite     func()
	onOpen      func()
}

func newFakeNativeAPI() *fakeNativeAPI {
	f := &fakeNativeAPI{
		sources: make(map[nativeDisplayID]string), targets: make(map[nativeDisplayID]nativeTargetName),
		gdi: make(map[uintptr]string), physical: make(map[uintptr][]nativePhysicalMonitor),
		caps: make(map[uintptr][]byte), capErrors: make(map[uintptr]error), replyErrors: make(map[uintptr]error), openErrors: make(map[uintptr]error), countErrors: make(map[uintptr]error),
		destroyErrs: make(map[uintptr]error), lengths: make(map[uintptr]uint32), destroyed: make(map[uintptr]int),
		readValue: 17, readMaximum: 255,
	}
	f.addMonitor(1, 101, 1, `\\.\DISPLAY1`, `\\?\DISPLAY#ABC#ONE`)
	return f
}

func (f *fakeNativeAPI) addMonitor(logical, physical uintptr, id uint32, gdi, path string) {
	source := nativeDisplayID{Adapter: nativeLUID{LowPart: 7}, ID: id}
	target := nativeDisplayID{Adapter: nativeLUID{LowPart: 7}, ID: id + 10}
	f.paths = append(f.paths, nativeDisplayPath{Source: source, Target: target})
	f.sources[source], f.targets[target] = gdi, nativeTargetName{Path: path, FriendlyName: "native monitor"}
	f.monitors = append(f.monitors, logical)
	f.gdi[logical] = gdi
	f.physical[logical] = []nativePhysicalMonitor{{Handle: physical, Description: "native monitor"}}
	f.caps[physical] = []byte("(prot(monitor)type(LCD)vcp(10 60(0F 11 12)))\x00")
}

func (f *fakeNativeAPI) probe() error { return f.probeErr }
func (f *fakeNativeAPI) displayConfigBufferSizes() (uint32, uint32, error) {
	return uint32(len(f.paths)), uint32(len(f.paths) * 2), nil
}
func (f *fakeNativeAPI) queryDisplayConfig(_, _ uint32) ([]nativeDisplayPath, error) {
	f.queryCalls++
	if len(f.queryErrors) > 0 {
		err := f.queryErrors[0]
		f.queryErrors = f.queryErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	return append([]nativeDisplayPath(nil), f.paths...), nil
}
func (f *fakeNativeAPI) sourceName(id nativeDisplayID) (string, error) { return f.sources[id], nil }
func (f *fakeNativeAPI) targetName(id nativeDisplayID) (nativeTargetName, error) {
	return f.targets[id], nil
}
func (f *fakeNativeAPI) enumDisplayMonitors() ([]uintptr, error) {
	return append([]uintptr(nil), f.monitors...), nil
}
func (f *fakeNativeAPI) monitorName(handle uintptr) (string, error) { return f.gdi[handle], nil }
func (f *fakeNativeAPI) physicalMonitorCount(handle uintptr) (uint32, error) {
	return uint32(len(f.physical[handle])), f.countErrors[handle]
}
func (f *fakeNativeAPI) physicalMonitors(handle uintptr, _ uint32) ([]nativePhysicalMonitor, error) {
	if f.onOpen != nil {
		f.onOpen()
	}
	return append([]nativePhysicalMonitor(nil), f.physical[handle]...), f.openErrors[handle]
}
func (f *fakeNativeAPI) capabilitiesLength(handle uintptr) (uint32, error) {
	if length, ok := f.lengths[handle]; ok {
		return length, f.capErrors[handle]
	}
	return uint32(len(f.caps[handle])), f.capErrors[handle]
}
func (f *fakeNativeAPI) capabilities(handle uintptr, _ uint32) ([]byte, error) {
	return append([]byte(nil), f.caps[handle]...), f.replyErrors[handle]
}
func (f *fakeNativeAPI) getVCP(handle uintptr, code uint8) (uint32, uint32, error) {
	f.reads = append(f.reads, vcpCall{handle: handle, code: code})
	if f.onRead != nil {
		f.onRead()
	}
	return f.readValue, f.readMaximum, f.readErr
}
func (f *fakeNativeAPI) setVCP(handle uintptr, code uint8, value uint32) error {
	f.writes = append(f.writes, vcpCall{handle: handle, code: code, value: value})
	if f.onWrite != nil {
		f.onWrite()
	}
	return f.writeErr
}
func (f *fakeNativeAPI) destroyPhysicalMonitor(handle uintptr) error {
	f.destroyed[handle]++
	return f.destroyErrs[handle]
}

func TestNativeAdapterMapsStablePathAndUsesFreshHandles(t *testing.T) {
	f := newFakeNativeAPI()
	f.addMonitor(2, 102, 2, `\\.\DISPLAY2`, `\\?\DISPLAY#ABC#TWO`)
	b := New(newNativeTransport(f))
	devices, err := b.Enumerate(context.Background())
	if err != nil || len(devices) != 2 {
		t.Fatalf("原生适配枚举：%v %v", devices, err)
	}
	device := devices[0]
	if device.Identity.Path != `\\?\display#abc#one` || device.Identity.Serial != "" || device.Identity.EDIDSHA256 != "" || device.Identity.Manufacturer != "" {
		t.Fatalf("身份必须只来自真实规范化路径：%+v", device.Identity)
	}
	// 逻辑句柄、物理句柄和枚举次序都改变，路径仍指向同一个已绑定目标。
	f.monitors = []uintptr{2, 3}
	f.gdi[3] = f.gdi[1]
	f.physical[3] = []nativePhysicalMonitor{{Handle: 203, Description: "native monitor"}}
	f.caps[203] = f.caps[101]
	f.paths[0], f.paths[1] = f.paths[1], f.paths[0]
	observation, err := b.ReadInput(context.Background(), device, standardScheme())
	if err != nil || observation.Value == nil || *observation.Value != 17 || observation.ObservedAt.IsZero() {
		t.Fatalf("原生读取未产生真实观测：%+v %v", observation, err)
	}
	receipt, err := b.SetInput(context.Background(), device, standardScheme())
	if err != nil || receipt.SentAt.IsZero() || len(f.writes) != 1 || f.writes[0] != (vcpCall{203, 0x60, 18}) || len(f.reads) != 1 || f.reads[0].handle != 203 {
		t.Fatalf("原生映射或输入值错误：%+v %v reads=%v writes=%v", receipt, err, f.reads, f.writes)
	}
	if f.destroyed[101] != 1 || f.destroyed[102] != 3 || f.destroyed[203] != 2 {
		t.Fatalf("新旧句柄未完整清理：%v", f.destroyed)
	}
}

func TestNativeAdapterRejectsAmbiguousMappingBeforeWriting(t *testing.T) {
	for _, kind := range []string{"clone", "multiple-physical", "missing-path", "duplicate-path", "duplicate-hmonitor"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeNativeAPI()
			b := New(newNativeTransport(f))
			device := enumerateOne(t, b)
			switch kind {
			case "clone":
				path := f.paths[0]
				path.Target.ID++
				f.paths = append(f.paths, path)
				f.targets[path.Target] = nativeTargetName{Path: `\\?\DISPLAY#CLONE`}
			case "multiple-physical":
				f.physical[1] = append(f.physical[1], nativePhysicalMonitor{Handle: 102})
			case "missing-path":
				f.targets[f.paths[0].Target] = nativeTargetName{}
			case "duplicate-path":
				f.addMonitor(2, 102, 2, `\\.\DISPLAY2`, `\\?\display#abc#one`)
			case "duplicate-hmonitor":
				f.monitors = append(f.monitors, 1)
			}
			receipt, err := b.SetInput(context.Background(), device, standardScheme())
			requireCode(t, err, model.CodeIdentityAmbiguous)
			if !receipt.SentAt.IsZero() || len(f.writes) != 0 {
				t.Fatalf("映射不唯一仍发生写入：%+v %v", receipt, f.writes)
			}
		})
	}
}

func TestNativeAdapterRejectsTargetChangedDuringPhysicalOpen(t *testing.T) {
	f := newFakeNativeAPI()
	b := New(newNativeTransport(f))
	device := enumerateOne(t, b)
	f.onOpen = func() {
		f.targets[f.paths[0].Target] = nativeTargetName{Path: `\\?\DISPLAY#REPLACEMENT`}
	}
	receipt, err := b.SetInput(context.Background(), device, standardScheme())
	if err == nil || len(f.writes) != 0 || !receipt.SentAt.IsZero() || f.destroyed[101] != 2 {
		t.Fatalf("打开期间替换目标仍写入或未清理：receipt=%+v err=%v closed=%v", receipt, err, f.destroyed)
	}
}

func TestNativeAdapterCapabilitiesDoNotInventSupport(t *testing.T) {
	for _, content := range []string{"(vcp(10 12))\x00", "(vcp(60(11 12))\x00", "(vcp(60(ZZ)))\x00", "(vcp(60))", "\x00"} {
		t.Run(strings.ReplaceAll(content, "\x00", "NUL"), func(t *testing.T) {
			f := newFakeNativeAPI()
			f.caps[101] = []byte(content)
			b := New(newNativeTransport(f))
			devices, err := b.Enumerate(context.Background())
			if err == nil && len(devices) == 1 {
				caps, capErr := b.Capabilities(context.Background(), devices[0])
				if capErr == nil && (len(caps.Protocols) != 0 || caps.CanReadInput || caps.UnavailableReason == "") {
					t.Fatalf("无效能力被当作支持：%+v", caps)
				}
				_, _ = b.SetInput(context.Background(), devices[0], standardScheme())
			}
			if len(f.writes) != 0 || f.destroyed[101] == 0 {
				t.Fatalf("无效能力仍写入或泄漏句柄：writes=%v closed=%v", f.writes, f.destroyed)
			}
		})
	}
	f := newFakeNativeAPI()
	f.lengths[101] = maxNativeCapabilities + 1
	_, err := New(newNativeTransport(f)).Enumerate(context.Background())
	if err == nil || f.destroyed[101] != 1 {
		t.Fatalf("不可信长度未拒绝或未清理：%v %v", err, f.destroyed)
	}
}

func TestNativeAdapterParsesOnlyCompleteVCPFeatures(t *testing.T) {
	tests := []struct {
		name, content string
		supported     bool
	}{
		{"nested-values", "(model(Screen60) vcp(10 60(0f 11 (12)) D6(01 04)))\x00", true},
		{"mixed-case-space", "( MODEL(Screen) VcP( 10\t60 ( 0f 11 )\r\nD6(01) ) )\x00", true},
		{"other-section", "(model(Screen60) cmds(60) vcp(10 12))\x00", false},
		{"values-only", "(vcp(10(60) D6(01)))\x00", false},
		{"no-vcp", "(model(Screen60) cmds(60))\x00", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFakeNativeAPI()
			f.caps[101] = []byte(test.content)
			b := New(newNativeTransport(f))
			device := enumerateOne(t, b)
			caps, err := b.Capabilities(context.Background(), device)
			if err != nil || (len(caps.Protocols) == 1) != test.supported || caps.CanReadInput != test.supported {
				t.Fatalf("MCCS分区解析不符合真实内容：caps=%+v err=%v", caps, err)
			}
			_, err = b.SetInput(context.Background(), device, standardScheme())
			if (err == nil) != test.supported || (len(f.writes) == 1) != test.supported {
				t.Fatalf("能力未约束实际输入操作：err=%v writes=%v", err, f.writes)
			}
		})
	}
}

func TestNativeAdapterRejectsMalformedCapabilityBoundaries(t *testing.T) {
	contents := []string{
		"(vcp(600))\x00", "(vcp(60) vcp(10))\x00", "(vcp(60))\x00trailing\x00",
		strings.Repeat("(", maxCapabilityDepth+1) + strings.Repeat(")", maxCapabilityDepth+1) + "\x00",
		"(model(" + strings.Repeat("x", maxNativeCapabilities) + ")vcp(60))\x00",
	}
	for index, content := range contents {
		f := newFakeNativeAPI()
		f.caps[101] = []byte(content)
		devices, err := New(newNativeTransport(f)).Enumerate(context.Background())
		if err == nil || len(devices) != 0 || len(f.writes) != 0 || f.destroyed[101] != 1 {
			t.Fatalf("损坏能力边界%d未拒绝或未清理：devices=%v err=%v closed=%v", index, devices, err, f.destroyed)
		}
	}
}

func TestNativeAdapterKnownNonDDCMonitorDoesNotBlockExternal(t *testing.T) {
	f := newFakeNativeAPI()
	f.addMonitor(2, 102, 2, `\\.\DISPLAY2`, `\\?\DISPLAY#EXTERNAL`)
	f.capErrors[101] = syscall.Errno(50)
	b := New(newNativeTransport(f))
	devices, err := b.Enumerate(context.Background())
	if err != nil || len(devices) != 2 {
		t.Fatalf("已知非DDC内屏阻断外屏：%v %v", devices, err)
	}
	caps, err := b.Capabilities(context.Background(), devices[0])
	if err != nil || len(caps.Protocols) != 0 || !strings.Contains(caps.UnavailableReason, "50") || !strings.Contains(caps.UnavailableReason, "GetCapabilitiesStringLength") {
		t.Fatalf("非DDC原因缺少原码和阶段：%+v %v", caps, err)
	}
	if _, err = b.SetInput(context.Background(), devices[0], standardScheme()); err == nil || len(f.writes) != 0 {
		t.Fatalf("非DDC设备发生写入：%v %v", err, f.writes)
	}
	observation, err := b.ReadInput(context.Background(), devices[1], standardScheme())
	if err != nil || observation.Value == nil || *observation.Value != 17 {
		t.Fatalf("正常外屏读取失败：%+v %v", observation, err)
	}
	if _, err = b.SetInput(context.Background(), devices[1], standardScheme()); err != nil || len(f.writes) != 1 || f.writes[0].handle != 102 {
		t.Fatalf("正常外屏写入失败：%v %v", err, f.writes)
	}
}

func TestNativeAdapterPhysicalCountFailureAndEmptyResult(t *testing.T) {
	f := newFakeNativeAPI()
	f.addMonitor(2, 102, 2, `\\.\DISPLAY2`, `\\?\DISPLAY#SECOND`)
	f.countErrors[2] = syscall.Errno(50)
	devices, err := New(newNativeTransport(f)).Enumerate(context.Background())
	if len(devices) != 0 || !errors.Is(err, syscall.Errno(50)) || !strings.Contains(err.Error(), "GetNumberOfPhysicalMonitorsFromHMONITOR") || f.destroyed[101] != 1 || f.destroyed[102] != 0 {
		t.Fatalf("物理数量失败被跳过、丢失原码或泄漏此前句柄：devices=%v err=%v closed=%v", devices, err, f.destroyed)
	}
	f = newFakeNativeAPI()
	f.physical[1] = nil
	f.addMonitor(2, 102, 2, `\\.\DISPLAY2`, `\\?\DISPLAY#SECOND`)
	b := New(newNativeTransport(f))
	device := enumerateOne(t, b)
	if device.Identity.Path != `\\?\display#second` || len(f.destroyed) != 1 || f.destroyed[102] != 1 {
		t.Fatalf("合法零数量产生虚假设备或错误清理：device=%+v closed=%v", device, f.destroyed)
	}
	if _, err := b.SetInput(context.Background(), device, standardScheme()); err != nil || len(f.writes) != 1 || f.writes[0].handle != 102 {
		t.Fatalf("零物理句柄目标阻断真实外屏：%v %v", err, f.writes)
	}
}

func TestNativeAdapterFailuresRetainNativeCauseAndAllCleanup(t *testing.T) {
	for _, stage := range []string{"open", "capabilities", "capabilities-reply"} {
		t.Run(stage, func(t *testing.T) {
			f := newFakeNativeAPI()
			f.addMonitor(2, 102, 2, `\\.\DISPLAY2`, `\\?\DISPLAY#SECOND`)
			cause, cleanup := syscall.Errno(31), syscall.Errno(5)
			if stage == "open" {
				f.openErrors[2] = cause
			} else if stage == "capabilities" {
				f.capErrors[102] = cause
			} else {
				f.replyErrors[102] = cause
			}
			f.destroyErrs[101], f.destroyErrs[102] = cleanup, syscall.Errno(6)
			devices, err := New(newNativeTransport(f)).Enumerate(context.Background())
			if len(devices) != 0 || !errors.Is(err, cause) || !errors.Is(err, cleanup) || !errors.Is(err, syscall.Errno(6)) || f.destroyed[101] != 1 || f.destroyed[102] != 1 {
				t.Fatalf("原生失败未保留或未全量清理：%v %v %v", devices, err, f.destroyed)
			}
		})
	}
	f := newFakeNativeAPI()
	f.probeErr = syscall.Errno(126)
	_, err := New(newNativeTransport(f)).Enumerate(context.Background())
	requireCode(t, err, model.CodeBackendUnavailable)
	if !errors.Is(err, syscall.Errno(126)) || f.queryCalls != 0 {
		t.Fatalf("DLL不可用未保原码或仍调用API：%v", err)
	}
}

func TestNativeAdapterRetriesOnlyExplicitTopologyBufferRace(t *testing.T) {
	f := newFakeNativeAPI()
	f.queryErrors = []error{syscall.Errno(122), nil}
	if _, err := New(newNativeTransport(f)).Enumerate(context.Background()); err != nil || f.queryCalls < 2 {
		t.Fatalf("拓扑缓冲区竞争未重查：calls=%d err=%v", f.queryCalls, err)
	}
	f = newFakeNativeAPI()
	f.queryErrors = []error{syscall.Errno(31), nil}
	_, err := New(newNativeTransport(f)).Enumerate(context.Background())
	if !errors.Is(err, syscall.Errno(31)) || f.queryCalls != 1 {
		t.Fatalf("未知失败被重试或吞掉：calls=%d err=%v", f.queryCalls, err)
	}
	f = newFakeNativeAPI()
	f.queryErrors = []error{syscall.Errno(122), syscall.Errno(122), syscall.Errno(122), syscall.Errno(122)}
	_, err = New(newNativeTransport(f)).Enumerate(context.Background())
	if !errors.Is(err, syscall.Errno(122)) || f.queryCalls > 3 {
		t.Fatalf("拓扑竞争未有界停止：calls=%d err=%v", f.queryCalls, err)
	}
}

func TestNativeAdapterCancellationPreservesReadAndWaitsForWrite(t *testing.T) {
	f := newFakeNativeAPI()
	b := New(newNativeTransport(f))
	device := enumerateOne(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	f.onRead = cancel
	observation, err := b.ReadInput(ctx, device, standardScheme())
	if !errors.Is(err, context.Canceled) || observation.Value == nil || *observation.Value != 17 || observation.ObservedAt.IsZero() || f.destroyed[101] != 2 {
		t.Fatalf("读后取消丢失观测或未清理：%+v %v %v", observation, err, f.destroyed)
	}
	ctx, cancel = context.WithCancel(context.Background())
	entered, release := make(chan struct{}), make(chan struct{})
	f.onWrite = func() { close(entered); <-release }
	type result struct {
		receipt model.WriteReceipt
		err     error
	}
	done := make(chan result, 1)
	go func() { receipt, err := b.SetInput(ctx, device, standardScheme()); done <- result{receipt, err} }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("原生写入未开始")
	}
	cancel()
	select {
	case result := <-done:
		close(release)
		t.Fatalf("同步写入未返回就提前结束：%+v", result)
	default:
	}
	if f.destroyed[101] != 2 {
		close(release)
		t.Fatal("同步写入仍在使用的句柄提前释放")
	}
	close(release)
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || !result.receipt.SentAt.IsZero() || f.destroyed[101] != 3 || len(f.writes) != 1 {
			t.Fatalf("写中取消结果不确定性错误：%+v closed=%v writes=%v", result, f.destroyed, f.writes)
		}
	case <-time.After(time.Second):
		t.Fatal("同步写入返回后仍未结束")
	}
}

func TestNativeAdapterCancellationBeforeAndDuringOpen(t *testing.T) {
	f := newFakeNativeAPI()
	b := New(newNativeTransport(f))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Enumerate(ctx)
	if !errors.Is(err, context.Canceled) || f.queryCalls != 0 {
		t.Fatalf("开始前取消仍访问API：%v calls=%d", err, f.queryCalls)
	}
	device := enumerateOne(t, b)
	ctx, cancel = context.WithCancel(context.Background())
	f.onOpen = cancel
	_, err = b.ReadInput(ctx, device, standardScheme())
	if !errors.Is(err, context.Canceled) || f.destroyed[101] != 2 || len(f.reads) != 0 {
		t.Fatalf("打开时取消未停止后续I/O或未清理：%v closed=%v reads=%v", err, f.destroyed, f.reads)
	}
}

func TestNativeAdapterUsesConfiguredInputAndRejectsInvalidRead(t *testing.T) {
	f := newFakeNativeAPI()
	b := New(newNativeTransport(f))
	device := enumerateOne(t, b)
	scheme := standardScheme()
	scheme.Value = 99
	if _, err := b.SetInput(context.Background(), device, scheme); err != nil || len(f.writes) != 1 || f.writes[0].value != 99 {
		t.Fatalf("未按既有配置原义发送输入：%v %v", err, f.writes)
	}
	// VCP 0x60是非连续输入源代码，驱动的maximum不是有效端口范围。
	for _, maximum := range []uint32{0, 1, 65536} {
		f.readValue, f.readMaximum = 17, maximum
		observation, err := b.ReadInput(context.Background(), device, standardScheme())
		if err != nil || observation.Value == nil || *observation.Value != 17 || f.reads[len(f.reads)-1].code != 0x60 {
			t.Fatalf("非连续输入被maximum误拒：maximum=%d observation=%+v err=%v", maximum, observation, err)
		}
	}
	for _, value := range []uint32{65536, ^uint32(0)} {
		f.readValue = value
		observation, err := b.ReadInput(context.Background(), device, standardScheme())
		if err == nil || observation.Value != nil {
			t.Fatalf("非法VCP值被当作有效观测：%+v %v", observation, err)
		}
	}
}

func TestNativeBOOLFailurePreservesCodeAndSuccessIgnoresLastError(t *testing.T) {
	if err := nativeBOOLResult("SetVCPFeature", 1, syscall.Errno(5)); err != nil {
		t.Fatalf("成功调用误用遗留错误码：%v", err)
	}
	err := nativeBOOLResult("SetVCPFeature", 0, syscall.Errno(5))
	if !errors.Is(err, syscall.Errno(5)) || model.CodeOf(err) != model.CodePermissionDenied {
		t.Fatalf("BOOL失败丢失原码：%v", err)
	}
}
