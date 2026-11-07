package windows

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/model"
)

func TestLGFixedPacket(t *testing.T) {
	for _, test := range []struct {
		value uint16
		want  [8]byte
	}{
		{208, [8]byte{0x6e, 0x50, 0x84, 0x03, 0xf4, 0, 0xd0, 0x9d}},
		{145, [8]byte{0x6e, 0x50, 0x84, 0x03, 0xf4, 0, 0x91, 0xdc}},
		{65535, [8]byte{0x6e, 0x50, 0x84, 0x03, 0xf4, 0xff, 0xff, 0x4d}},
	} {
		packet, err := lgInputPacket(test.value)
		if err != nil || packet != test.want {
			t.Fatalf("固定报文 value=%d got=%x want=%x err=%v", test.value, packet, test.want, err)
		}
	}
	if _, err := lgInputPacket(0); err == nil {
		t.Fatal("零输入值未拒绝")
	}
}

func TestADLPublicABILayout(t *testing.T) {
	var a adlAdapterInfo
	var d adlDisplayInfo
	var id adlDisplayID
	var e adlEDIDData
	checks := []struct {
		name      string
		got, want uintptr
	}{
		{"AdapterInfo.size", unsafe.Sizeof(a), 1572},
		{"size", unsafe.Offsetof(a.Size), 0}, {"index", unsafe.Offsetof(a.Index), 4},
		{"UDID", unsafe.Offsetof(a.UDID), 8}, {"bus", unsafe.Offsetof(a.Bus), 264},
		{"device", unsafe.Offsetof(a.Device), 268}, {"function", unsafe.Offsetof(a.Function), 272},
		{"vendor", unsafe.Offsetof(a.Vendor), 276}, {"GDI", unsafe.Offsetof(a.DisplayName), 536},
		{"present", unsafe.Offsetof(a.Present), 792}, {"exist", unsafe.Offsetof(a.Exist), 796},
		{"PNP", unsafe.Offsetof(a.PNP), 1312}, {"OSindex", unsafe.Offsetof(a.OSIndex), 1568},
		{"DisplayID.size", unsafe.Sizeof(id), 16},
		{"logicalDisplay", unsafe.Offsetof(id.LogicalDisplay), 0}, {"physicalDisplay", unsafe.Offsetof(id.PhysicalDisplay), 4},
		{"logicalAdapter", unsafe.Offsetof(id.LogicalAdapter), 8}, {"physicalAdapter", unsafe.Offsetof(id.PhysicalAdapter), 12},
		{"DisplayInfo.size", unsafe.Sizeof(d), 552}, {"ID", unsafe.Offsetof(d.ID), 0},
		{"controller", unsafe.Offsetof(d.Controller), 16}, {"name", unsafe.Offsetof(d.Name), 20},
		{"manufacturer", unsafe.Offsetof(d.Manufacturer), 276}, {"type", unsafe.Offsetof(d.Type), 532},
		{"output", unsafe.Offsetof(d.Output), 536}, {"connector", unsafe.Offsetof(d.Connector), 540},
		{"mask", unsafe.Offsetof(d.Mask), 544}, {"value", unsafe.Offsetof(d.Value), 548},
		{"EDID.size", unsafe.Sizeof(e), 288}, {"EDID.Size", unsafe.Offsetof(e.Size), 0},
		{"EDID.Flag", unsafe.Offsetof(e.Flag), 4}, {"EDID.DataSize", unsafe.Offsetof(e.DataSize), 8},
		{"EDID.Block", unsafe.Offsetof(e.Block), 12}, {"EDID.Data", unsafe.Offsetof(e.Data), 16},
		{"EDID.Reserved", unsafe.Offsetof(e.Reserved), 272},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s=%d want=%d", check.name, check.got, check.want)
		}
	}
	if err := validateADLABI(); err != nil {
		t.Fatal(err)
	}
	if err := adlStatus("fake", uintptr(uint32(0xfffffff8))); err == nil || !strings.Contains(err.Error(), "-8") {
		t.Fatalf("负ADL返回值丢失或被当BOOL：%v", err)
	}
}

func testEDID(seed byte, extensions int) []byte {
	data := make([]byte, 128*(extensions+1))
	copy(data, []byte{0, 255, 255, 255, 255, 255, 255, 0})
	data[8], data[18], data[19], data[126] = seed, 1, 4, byte(extensions)
	for block := 0; block <= extensions; block++ {
		var sum byte
		for _, value := range data[block*128 : block*128+127] {
			sum += value
		}
		data[block*128+127] = -sum
	}
	return data
}

type fakeADLSession struct {
	adapterList   []adlAdapterInfo
	displayList   []adlDisplayInfo
	edids         map[int32][]byte
	err, closeErr error
	writes        []struct {
		adapter, display int32
		packet           [8]byte
	}
	closed  int
	onEDID  func()
	onWrite func()
}

func (s *fakeADLSession) adapters() ([]adlAdapterInfo, error)      { return s.adapterList, s.err }
func (s *fakeADLSession) displays(int32) ([]adlDisplayInfo, error) { return s.displayList, s.err }
func (s *fakeADLSession) edid(_, display int32) ([]byte, error) {
	data := append([]byte(nil), s.edids[display]...)
	if s.onEDID != nil {
		s.onEDID()
	}
	return data, s.err
}
func (s *fakeADLSession) writeLG(adapter, display int32, packet [8]byte) error {
	s.writes = append(s.writes, struct {
		adapter, display int32
		packet           [8]byte
	}{adapter, display, packet})
	if s.onWrite != nil {
		s.onWrite()
	}
	return s.err
}
func (s *fakeADLSession) close() error { s.closed++; return s.closeErr }

type fakeLGNativeAPI struct {
	*fakeNativeAPI
	session                *fakeADLSession
	registry               []byte
	registryErr, driverErr error
	arch                   string
	opens                  int
}

func newFakeLGNativeAPI() *fakeLGNativeAPI {
	f := &fakeLGNativeAPI{fakeNativeAPI: newFakeNativeAPI(), registry: testEDID(1, 1), arch: "amd64"}
	a := adlAdapterInfo{Size: 1572, Index: 7, Vendor: 1002, Present: 1, Exist: 1}
	copy(a.DisplayName[:], `\\.\DISPLAY1`)
	d := adlDisplayInfo{ID: adlDisplayID{LogicalDisplay: 19, PhysicalDisplay: 3, LogicalAdapter: 7, PhysicalAdapter: 7}, Mask: 7, Value: 3}
	f.session = &fakeADLSession{adapterList: []adlAdapterInfo{a}, displayList: []adlDisplayInfo{d}, edids: map[int32][]byte{19: append([]byte(nil), f.registry...)}}
	return f
}
func (f *fakeLGNativeAPI) monitorEDID(string) ([]byte, error) {
	return append([]byte(nil), f.registry...), f.registryErr
}
func (f *fakeLGNativeAPI) openADL() (adlSession, error) {
	if err := requireADLArchitecture(f.arch); err != nil {
		return nil, err
	}
	if f.driverErr != nil {
		return nil, f.driverErr
	}
	f.opens++
	return f.session, nil
}
func lgScheme() model.InputScheme {
	return model.InputScheme{ID: "lg-dp", Protocol: model.ProtocolLGInputAlt, Value: 208}
}

func TestLGNativeUniqueMappingReceiptAndStandardRegression(t *testing.T) {
	f := newFakeLGNativeAPI()
	// 不含标准0x60仍须允许明确LG方案；只能提交，不能制造输入观测或开启预览。
	f.caps[101] = []byte("(vcp(10))\x00")
	b := New(newNativeTransport(f))
	device := enumerateOne(t, b)
	caps, err := b.Capabilities(context.Background(), device)
	if err != nil || len(caps.Protocols) != 1 || caps.Protocols[0] != model.ProtocolLGInputAlt || caps.CanReadInput || caps.UnavailableReason != "" {
		t.Fatalf("LG独立能力错误：%+v %v", caps, err)
	}
	obs, err := b.ReadInput(context.Background(), device, lgScheme())
	if model.CodeOf(err) != model.CodeUnsupportedOperation || obs.Value != nil || len(f.reads) != 0 {
		t.Fatalf("私有协议伪造读回：%+v %v", obs, err)
	}
	receipt, err := b.SetInput(context.Background(), device, lgScheme())
	if err != nil || receipt.SentAt.IsZero() || len(f.session.writes) != 1 || len(f.writes) != 0 || f.session.closed != 2 {
		t.Fatalf("LG提交或清理错误：%+v %v session=%+v", receipt, err, f.session)
	}
	call := f.session.writes[0]
	packet, _ := lgInputPacket(208)
	if call.adapter != 7 || call.display != 19 || call.packet != packet {
		t.Fatalf("使用了固定旧机索引或错误报文：%+v", call)
	}
	if _, err := b.SetInput(context.Background(), device, standardScheme()); err == nil || len(f.writes) != 0 {
		t.Fatal("未声明0x60发生盲写")
	}
	f.caps[101] = []byte("(vcp(60))\x00")
	if _, err := b.SetInput(context.Background(), device, standardScheme()); err != nil || len(f.writes) != 1 {
		t.Fatalf("标准通路回归：%v", err)
	}
	caps, err = b.Capabilities(context.Background(), device)
	if err != nil || len(caps.Protocols) != 2 || !caps.CanReadInput {
		t.Fatalf("混合能力丢失标准读回：%+v %v", caps, err)
	}
	obs, err = b.ReadInput(context.Background(), device, standardScheme())
	if err != nil || obs.Value == nil || *obs.Value != 17 || len(f.reads) != 1 {
		t.Fatalf("标准读回回归：%+v %v", obs, err)
	}
	before := len(f.session.writes)
	obs, err = b.ReadInput(context.Background(), device, lgScheme())
	if model.CodeOf(err) != model.CodeUnsupportedOperation || obs.Value != nil || len(f.reads) != 1 || len(f.session.writes) != before {
		t.Fatalf("LG预读未在写前拒绝：%+v %v", obs, err)
	}
}

func TestLGMappingRejectsMissingDuplicateContradictoryEvidence(t *testing.T) {
	for _, kind := range []string{"non-amd", "pci-vendor-encoding", "missing-gdi", "duplicate-adapter", "duplicate-display", "duplicate-edid", "missing-edid", "missing-adl-edid", "bad-checksum", "incomplete-edid", "too-many-blocks", "edid-mismatch", "nonlocal", "cross-adapter", "unmapped", "invalid-mask"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeLGNativeAPI()
			s := f.session
			switch kind {
			case "non-amd":
				s.adapterList[0].Vendor = 8086
			case "pci-vendor-encoding":
				s.adapterList[0].Vendor = 0x1002
			case "missing-gdi":
				s.adapterList[0].DisplayName = [256]byte{}
			case "duplicate-adapter":
				s.adapterList = append(s.adapterList, s.adapterList[0])
			case "duplicate-display":
				s.displayList = append(s.displayList, s.displayList[0])
			case "duplicate-edid":
				other := s.displayList[0]
				other.ID.LogicalDisplay, other.ID.PhysicalDisplay = 23, 4
				s.displayList = append(s.displayList, other)
				s.edids[23] = append([]byte(nil), f.registry...)
			case "missing-edid":
				f.registry = nil
			case "missing-adl-edid":
				s.edids[19] = nil
			case "bad-checksum":
				f.registry[20]++
			case "incomplete-edid":
				f.registry = f.registry[:128]
			case "too-many-blocks":
				f.registry = testEDID(1, 2)
			case "edid-mismatch":
				s.edids[19] = testEDID(2, 1)
			case "nonlocal":
				s.displayList[0].Value |= 4
			case "cross-adapter":
				s.displayList[0].ID.PhysicalAdapter++
			case "unmapped":
				s.displayList[0].Value = 1
			case "invalid-mask":
				s.displayList[0].Mask = 1
			}
			b := New(newNativeTransport(f))
			device := enumerateOne(t, b)
			receipt, err := b.SetInput(context.Background(), device, lgScheme())
			if err == nil || !receipt.SentAt.IsZero() || len(s.writes) != 0 || len(f.writes) != 0 || s.closed != f.opens {
				t.Fatalf("无可信唯一身份仍写入或未关闭：%+v %v session=%+v opens=%d", receipt, err, s, f.opens)
			}
		})
	}
}

func TestLGRejectsStaleTopologyEDIDAndOutput(t *testing.T) {
	for _, kind := range []string{"replacement-before-open", "route-before-open", "hotplug", "edid-race", "output-race", "source-race", "target-id-race"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeLGNativeAPI()
			b := New(newNativeTransport(f))
			device := enumerateOne(t, b)
			if kind == "replacement-before-open" {
				f.registry = testEDID(2, 1)
			} else if kind == "route-before-open" {
				f.sources[f.paths[0].Source] = `\\.\DISPLAY22`
				f.gdi[1] = `\\.\DISPLAY22`
				f.session.adapterList[0].DisplayName = [256]byte{}
				copy(f.session.adapterList[0].DisplayName[:], `\\.\DISPLAY22`)
			} else {
				f.session.onEDID = func() {
					f.session.onEDID = nil
					switch kind {
					case "hotplug":
						f.targets[f.paths[0].Target] = nativeTargetName{Path: `\\?\DISPLAY#REPLACEMENT`}
					case "edid-race":
						f.registry = testEDID(2, 1)
					case "output-race":
						f.session.displayList[0].ID.PhysicalDisplay++
					case "source-race":
						f.sources[f.paths[0].Source] = `\\.\DISPLAY22`
					case "target-id-race":
						old := f.paths[0].Target
						f.paths[0].Target.ID++
						f.targets[f.paths[0].Target] = f.targets[old]
					}
				}
			}
			receipt, err := b.SetInput(context.Background(), device, lgScheme())
			if err == nil || !receipt.SentAt.IsZero() || len(f.session.writes) != 0 || f.session.closed != f.opens || f.destroyed[101] != 2 {
				t.Fatalf("拓扑竞争未拒写或清理错误：%+v %v closed=%v", receipt, err, f.destroyed)
			}
		})
	}
}

func TestLGArchitectureDriverFailureCancellationAndCleanup(t *testing.T) {
	for _, kind := range []string{"arm64", "driver-missing", "enumeration-error", "cleanup-error", "write-cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeLGNativeAPI()
			b := New(newNativeTransport(f))
			device := enumerateOne(t, b)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := errors.New("ADL injected failure")
			switch kind {
			case "arm64":
				f.arch = "arm64"
			case "driver-missing":
				f.driverErr = model.NewError(model.CodeUnsupportedOperation, "atiadlxx.dll不可用", cause)
			case "enumeration-error":
				f.session.err = cause
			case "cleanup-error":
				f.session.closeErr = cause
			case "write-cancel":
				f.session.onWrite = cancel
			}
			receipt, err := b.SetInput(ctx, device, lgScheme())
			if err == nil || !receipt.SentAt.IsZero() || f.session.closed != f.opens || f.destroyed[101] != 2 {
				t.Fatalf("失败/取消伪造回执或未关闭：%+v %v", receipt, err)
			}
			if kind == "arm64" || kind == "driver-missing" {
				requireCode(t, err, model.CodeUnsupportedOperation)
			}
			if kind == "write-cancel" && (len(f.session.writes) != 1 || !errors.Is(err, context.Canceled)) {
				t.Fatalf("写后取消重试或丢失原因：%v", err)
			}
		})
	}
}

func TestCompleteEDIDValidation(t *testing.T) {
	for _, blocks := range []int{0, 1} {
		data := testEDID(1, blocks)
		got, err := validatedLGEDID(data)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("完整有效EDID被误拒：%v", err)
		}
	}
	for _, data := range [][]byte{nil, make([]byte, 128), testEDID(1, 1)[:128], append(testEDID(1, 0), 0), testEDID(1, 2)} {
		if _, err := validatedLGEDID(data); err == nil {
			t.Fatalf("错误EDID未拒绝：len=%d", len(data))
		}
	}
}

func TestADLDisplayAllocationAlwaysFreedAndCopied(t *testing.T) {
	buffer := []adlDisplayInfo{{Controller: 17}}
	cause, cleanup := errors.New("DisplayInfo failed after allocation"), errors.New("HeapFree failed")
	for _, kind := range []string{"success", "call-failure", "bad-count", "zero-count-with-pointer", "cleanup-failure", "both-failures"} {
		t.Run(kind, func(t *testing.T) {
			count, callErr, freeErr := int32(1), error(nil), error(nil)
			switch kind {
			case "call-failure":
				callErr = cause
			case "bad-count":
				count = maxNativeDisplayItems + 1
			case "zero-count-with-pointer":
				count = 0
			case "cleanup-failure":
				freeErr = cleanup
			case "both-failures":
				callErr, freeErr = cause, cleanup
			}
			frees := 0
			got, err := copyADLDisplays(count, unsafe.Pointer(&buffer[0]), callErr, func(address unsafe.Pointer) error {
				if address != unsafe.Pointer(&buffer[0]) {
					t.Fatal("释放了不同分配")
				}
				frees++
				return freeErr
			})
			if frees != 1 || (kind == "success") != (err == nil) {
				t.Fatalf("数组资源或错误处理错误：frees=%d err=%v", frees, err)
			}
			if callErr != nil && !errors.Is(err, cause) {
				t.Fatalf("原调用错误被覆盖：%v", err)
			}
			if freeErr != nil && !errors.Is(err, cleanup) {
				t.Fatalf("释放错误被覆盖：%v", err)
			}
			if kind == "success" {
				got[0].Controller = 99
				if buffer[0].Controller != 17 {
					t.Fatal("输出仍引用已释放原生数组")
				}
			}
		})
	}
	if _, err := copyADLDisplays(1, nil, nil, func(unsafe.Pointer) error { t.Fatal("释放空指针"); return nil }); err == nil {
		t.Fatal("正数量空指针未拒绝")
	}
}

func TestOptionalLGFailureDoesNotInvalidateStandardCapabilities(t *testing.T) {
	f := newFakeLGNativeAPI()
	b := New(newNativeTransport(f))
	device := enumerateOne(t, b)
	f.registryErr = errors.New("SetupAPI temporary failure")
	caps, err := b.Capabilities(context.Background(), device)
	if err != nil || len(caps.Protocols) != 1 || caps.Protocols[0] != model.ProtocolVCP || !caps.CanReadInput || caps.UnavailableReason != "" {
		t.Fatalf("可选EDID失败撤销标准能力：%+v %v", caps, err)
	}
	if _, err := b.ReadInput(context.Background(), device, standardScheme()); err != nil || len(f.reads) != 1 {
		t.Fatalf("可选EDID失败阻断标准读取：%v", err)
	}
	if _, err := b.SetInput(context.Background(), device, standardScheme()); err != nil || len(f.writes) != 1 {
		t.Fatalf("可选EDID失败阻断标准写入：%v", err)
	}
	f.registryErr = nil
	f.driverErr = errors.New("ADL driver failure")
	caps, err = b.Capabilities(context.Background(), device)
	if err != nil || !caps.CanReadInput || len(caps.Protocols) != 1 {
		t.Fatalf("可选驱动失败撤销标准能力：%+v %v", caps, err)
	}
}
