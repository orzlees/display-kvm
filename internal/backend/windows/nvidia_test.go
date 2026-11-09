package windows

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

type fakeNVAPIOutput struct {
	gpu    uintptr
	output uint32
}

type fakeNVAPIWrite struct {
	fakeNVAPIOutput
	address  byte
	register []byte
	data     []byte
}

// 假驱动仅提供原生枚举与I²C调用结果；GDI、GPU、输出和完整EDID的唯一性
// 必须由真实LG传输核验，不能由测试替身直接返回“已定位”或“支持LG”。
type fakeNVAPISession struct {
	displayList []uintptr
	names       map[uintptr]string
	gpus        map[uintptr][]uintptr
	outputs     map[uintptr]uint32
	edids       map[fakeNVAPIOutput][]byte
	mappingErr  error
	edidErr     error
	writeErr    error
	closeErr    error
	writes      []fakeNVAPIWrite
	closed      int
	onEDID      func()
	onWrite     func()
}

func (s *fakeNVAPISession) displays() ([]uintptr, error) {
	return append([]uintptr(nil), s.displayList...), s.mappingErr
}
func (s *fakeNVAPISession) displayName(display uintptr) (string, error) {
	return s.names[display], s.mappingErr
}
func (s *fakeNVAPISession) physicalGPUs(display uintptr) ([]uintptr, error) {
	return append([]uintptr(nil), s.gpus[display]...), s.mappingErr
}
func (s *fakeNVAPISession) outputID(display uintptr) (uint32, error) {
	return s.outputs[display], s.mappingErr
}
func (s *fakeNVAPISession) edid(gpu uintptr, output uint32) ([]byte, error) {
	data := append([]byte(nil), s.edids[fakeNVAPIOutput{gpu, output}]...)
	if s.onEDID != nil {
		s.onEDID()
	}
	return data, s.edidErr
}
func (s *fakeNVAPISession) writeI2C(gpu uintptr, output uint32, address byte, register, data []byte) error {
	s.writes = append(s.writes, fakeNVAPIWrite{
		fakeNVAPIOutput: fakeNVAPIOutput{gpu, output}, address: address,
		register: append([]byte(nil), register...), data: append([]byte(nil), data...),
	})
	if s.onWrite != nil {
		s.onWrite()
	}
	return s.writeErr
}
func (s *fakeNVAPISession) close() error { s.closed++; return s.closeErr }

type fakeNVIDIANativeAPI struct {
	*fakeLGNativeAPI
	nv                   *fakeNVAPISession
	vendor               uint32
	vendorErr, nvOpenErr error
	nvOpens, adlAttempts int
}

func newFakeNVIDIANativeAPI() *fakeNVIDIANativeAPI {
	f := &fakeNVIDIANativeAPI{fakeLGNativeAPI: newFakeLGNativeAPI(), vendor: 4318}
	// PCI厂商4318就是0x10DE；ADL自己的AMD字段仍使用既有十进制1002契约，
	// 不可把两种来源的厂商值混用，也不能把NVIDIA输出误交给ADL。
	f.session.adapterList[0].Vendor = 4318
	f.nv = &fakeNVAPISession{
		displayList: []uintptr{31}, names: map[uintptr]string{31: `\\.\DISPLAY1`},
		gpus: map[uintptr][]uintptr{31: {47}}, outputs: map[uintptr]uint32{31: 8},
		edids: map[fakeNVAPIOutput][]byte{{47, 8}: append([]byte(nil), f.registry...)},
	}
	return f
}
func (f *fakeNVIDIANativeAPI) displayVendor(string) (uint32, error) {
	return f.vendor, f.vendorErr
}
func (f *fakeNVIDIANativeAPI) openADL() (adlSession, error) {
	f.adlAttempts++
	return f.fakeLGNativeAPI.openADL()
}

// 使用匿名接口返回值，使本阶段测试不依赖尚未新增的产品类型，现代码仍可编译。
// 后续产品的会话类型可用此接口的类型别名，避免仅为编译RED而先写生产占位逻辑。
func (f *fakeNVIDIANativeAPI) openNVAPI() (interface {
	displays() ([]uintptr, error)
	displayName(uintptr) (string, error)
	physicalGPUs(uintptr) ([]uintptr, error)
	outputID(uintptr) (uint32, error)
	edid(uintptr, uint32) ([]byte, error)
	writeI2C(uintptr, uint32, byte, []byte, []byte) error
	close() error
}, error) {
	if f.nvOpenErr != nil {
		return nil, f.nvOpenErr
	}
	f.nvOpens++
	return f.nv, nil
}

func TestNVIDIALGUniqueMappingCapabilitiesAndSubmission(t *testing.T) {
	t.Run("capabilities", func(t *testing.T) {
		f := newFakeNVIDIANativeAPI()
		f.caps[101] = []byte("(vcp(10))\x00")
		b := New(newNativeTransport(f))
		device := enumerateOne(t, b)
		caps, err := b.Capabilities(context.Background(), device)
		if err != nil || len(caps.Protocols) != 1 || caps.Protocols[0] != model.ProtocolLGInputAlt || caps.CanReadInput || caps.UnavailableReason != "" {
			t.Fatalf("NVIDIA唯一输出未声明仅可提交的LG能力：%+v %v", caps, err)
		}
		if f.nvOpens != 1 || f.nv.closed != 1 || len(f.nv.writes) != 0 || f.adlAttempts != 0 || len(f.writes) != 0 {
			t.Fatalf("LG能力探测误选驱动、写入或未清理：NV=%d/%d ADL=%d", f.nvOpens, f.nv.closed, f.adlAttempts)
		}
	})
	for _, test := range []struct {
		value uint16
		frame [8]byte
	}{
		{208, [8]byte{0x6e, 0x50, 0x84, 0x03, 0xf4, 0, 0xd0, 0x9d}},
		{145, [8]byte{0x6e, 0x50, 0x84, 0x03, 0xf4, 0, 0x91, 0xdc}},
	} {
		t.Run(fmt.Sprintf("submit-%d", test.value), func(t *testing.T) {
			f := newFakeNVIDIANativeAPI()
			f.caps[101] = []byte("(vcp(10))\x00")
			b := New(newNativeTransport(f))
			device := enumerateOne(t, b)
			scheme := lgScheme()
			scheme.Value = test.value
			receipt, err := b.SetInput(context.Background(), device, scheme)
			if err != nil || receipt.SentAt.IsZero() || len(f.nv.writes) != 1 {
				t.Fatalf("NVIDIA LG未提交一次固定输入命令：receipt=%+v err=%v writes=%v", receipt, err, f.nv.writes)
			}
			call := f.nv.writes[0]
			packet, packetErr := lgInputPacket(test.value)
			if packetErr != nil || packet != test.frame || call.gpu != 47 || call.output != 8 || call.address != 0x6e || !bytes.Equal(call.register, []byte{0x50}) || !bytes.Equal(call.data, test.frame[2:]) {
				t.Fatalf("NVIDIA目标或地址/寄存器/数据拆分错误：call=%+v packet=%x err=%v", call, packet, packetErr)
			}
			if f.adlAttempts != 0 || len(f.session.writes) != 0 || len(f.writes) != 0 || f.nvOpens != 1 || f.nv.closed != 1 || f.destroyed[101] != 2 {
				t.Fatalf("NVIDIA提交发生跨驱动回退或清理错误：NV=%d/%d ADL=%d physical=%v", f.nvOpens, f.nv.closed, f.adlAttempts, f.destroyed)
			}
		})
	}
}

func TestNVIDIALGSelectsVendorWithoutCrossDriverFallback(t *testing.T) {
	for _, vendor := range []uint32{4318, 4098, 8086} {
		t.Run(fmt.Sprintf("pci-vendor-%d", vendor), func(t *testing.T) {
			f := newFakeNVIDIANativeAPI()
			f.vendor = vendor
			// 两套驱动都给出可写证据，选择必须来自活动GDI的PCI厂商，
			// 不能由DLL安装顺序或先成功的候选决定最终物理通路。
			f.session.adapterList[0].Vendor = 1002
			b := New(newNativeTransport(f))
			device := enumerateOne(t, b)
			receipt, err := b.SetInput(context.Background(), device, lgScheme())
			switch vendor {
			case 4318:
				if err != nil || receipt.SentAt.IsZero() || len(f.nv.writes) != 1 || f.adlAttempts != 0 {
					t.Fatalf("NVIDIA PCI厂商未独立选择NVAPI：%+v %v NV=%v ADL=%d", receipt, err, f.nv.writes, f.adlAttempts)
				}
			case 4098:
				if err != nil || receipt.SentAt.IsZero() || len(f.session.writes) != 1 || f.nvOpens != 0 {
					t.Fatalf("AMD PCI厂商0x1002破坏既有ADL路径：%+v %v", receipt, err)
				}
			default:
				if err == nil || !receipt.SentAt.IsZero() || len(f.nv.writes) != 0 || len(f.session.writes) != 0 || len(f.writes) != 0 {
					t.Fatalf("未知PCI厂商借用其他显卡通路写入：%+v %v", receipt, err)
				}
			}
		})
	}
}

func TestNVIDIALGRejectsUnreliableMappingBeforeWrite(t *testing.T) {
	for _, kind := range []string{"driver-missing", "vendor-error", "mapping-error", "missing-gdi", "duplicate-display", "duplicate-gdi", "missing-gpu", "multiple-gpus", "missing-output", "clone-output", "missing-edid", "edid-mismatch", "extension-mismatch", "incomplete-edid", "bad-checksum"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeNVIDIANativeAPI()
			cause := errors.New("NVAPI injected mapping failure")
			switch kind {
			case "driver-missing":
				f.nvOpenErr = model.NewError(model.CodeUnsupportedOperation, "nvapi64.dll不可用", cause)
				f.session.adapterList[0].Vendor = 1002
			case "vendor-error":
				f.vendorErr = cause
			case "mapping-error":
				f.nv.mappingErr = cause
			case "missing-gdi":
				f.nv.names[31] = `\\.\DISPLAY22`
			case "duplicate-display":
				f.nv.displayList = append(f.nv.displayList, 31)
			case "duplicate-gdi":
				f.nv.displayList = append(f.nv.displayList, 32)
				f.nv.names[32], f.nv.gpus[32], f.nv.outputs[32] = f.nv.names[31], []uintptr{47}, 16
				f.nv.edids[fakeNVAPIOutput{47, 16}] = append([]byte(nil), f.registry...)
			case "missing-gpu":
				f.nv.gpus[31] = nil
			case "multiple-gpus":
				f.nv.gpus[31] = []uintptr{47, 48}
			case "missing-output":
				f.nv.outputs[31] = 0
			case "clone-output":
				f.nv.outputs[31] = 8 | 16
				f.nv.edids[fakeNVAPIOutput{47, 8 | 16}] = append([]byte(nil), f.registry...)
			case "missing-edid":
				f.nv.edids[fakeNVAPIOutput{47, 8}] = nil
			case "edid-mismatch":
				f.nv.edids[fakeNVAPIOutput{47, 8}] = testEDID(2, 1)
			case "extension-mismatch":
				data := append([]byte(nil), f.registry...)
				data[128], data[255] = 1, 255
				f.nv.edids[fakeNVAPIOutput{47, 8}] = data
			case "incomplete-edid":
				f.nv.edids[fakeNVAPIOutput{47, 8}] = append([]byte(nil), f.registry[:128]...)
			case "bad-checksum":
				f.nv.edids[fakeNVAPIOutput{47, 8}][20]++
			}
			b := New(newNativeTransport(f))
			device := enumerateOne(t, b)
			receipt, err := b.SetInput(context.Background(), device, lgScheme())
			if err == nil || !receipt.SentAt.IsZero() || len(f.nv.writes) != 0 || len(f.session.writes) != 0 || len(f.writes) != 0 || f.nv.closed != f.nvOpens || f.destroyed[101] != 2 {
				t.Fatalf("NVIDIA身份不足仍写入、回退或未清理：%+v %v NV=%v ADL=%v", receipt, err, f.nv.writes, f.session.writes)
			}
			if (kind == "driver-missing" || kind == "vendor-error" || kind == "mapping-error") && !errors.Is(err, cause) {
				t.Fatalf("原始NVIDIA定位错误被其他驱动结果覆盖：%v", err)
			}
		})
	}
}

func TestNVIDIALGRejectsIdentityChangesBeforeWrite(t *testing.T) {
	for _, kind := range []string{"windows-target", "windows-edid", "gpu", "output", "gdi"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeNVIDIANativeAPI()
			b := New(newNativeTransport(f))
			device := enumerateOne(t, b)
			edidCalls := 0
			f.nv.onEDID = func() {
				edidCalls++
				f.nv.onEDID = nil
				switch kind {
				case "windows-target":
					f.targets[f.paths[0].Target] = nativeTargetName{Path: `\\?\DISPLAY#REPLACEMENT`}
				case "windows-edid":
					f.registry = testEDID(2, 1)
				case "gpu":
					f.nv.gpus[31] = []uintptr{48}
					f.nv.edids[fakeNVAPIOutput{48, 8}] = append([]byte(nil), f.registry...)
				case "output":
					f.nv.outputs[31] = 16
					f.nv.edids[fakeNVAPIOutput{47, 16}] = append([]byte(nil), f.registry...)
				case "gdi":
					f.nv.names[31] = `\\.\DISPLAY22`
				}
			}
			receipt, err := b.SetInput(context.Background(), device, lgScheme())
			if err == nil || !receipt.SentAt.IsZero() || edidCalls != 1 || len(f.nv.writes) != 0 || len(f.session.writes) != 0 || len(f.writes) != 0 || f.nv.closed != f.nvOpens {
				t.Fatalf("NVIDIA写前未复核目标或未执行竞争窗口：%+v %v EDID=%d NV=%v", receipt, err, edidCalls, f.nv.writes)
			}
		})
	}
}

func TestNVIDIALGSubmissionFailurePreservesUnknownWithoutRetry(t *testing.T) {
	for _, kind := range []string{"write-error", "write-cancel", "close-error"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeNVIDIANativeAPI()
			// 即使标准VCP和ADL都能提交，未知发送状态也不能触发另一条通路。
			f.session.adapterList[0].Vendor = 1002
			b := New(newNativeTransport(f))
			device := enumerateOne(t, b)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := errors.New("NVAPI write may already be submitted")
			switch kind {
			case "write-error":
				f.nv.writeErr = cause
			case "write-cancel":
				f.nv.onWrite = cancel
				cause = context.Canceled
			case "close-error":
				f.nv.closeErr = cause
			}
			receipt, err := b.SetInput(ctx, device, lgScheme())
			if !errors.Is(err, cause) || !receipt.SentAt.IsZero() || len(f.nv.writes) != 1 || len(f.session.writes) != 0 || f.adlAttempts != 0 || len(f.writes) != 0 || f.nvOpens != 1 || f.nv.closed != 1 || f.destroyed[101] != 2 {
				t.Fatalf("NVIDIA未知提交被误报成功、重试、回退或丢失错误：receipt=%+v err=%v NV=%v ADL=%v", receipt, err, f.nv.writes, f.session.writes)
			}
		})
	}
}

func TestNVIDIALGReadUnsupportedAndStandardVCPUnaffected(t *testing.T) {
	f := newFakeNVIDIANativeAPI()
	b := New(newNativeTransport(f))
	device := enumerateOne(t, b)
	obs, err := b.ReadInput(context.Background(), device, lgScheme())
	if model.CodeOf(err) != model.CodeUnsupportedOperation || obs.Value != nil || !obs.ObservedAt.IsZero() || len(f.reads) != 0 || f.nvOpens != 0 || len(f.nv.writes) != 0 {
		t.Fatalf("LG私有读回制造观测或触发驱动：%+v %v", obs, err)
	}
	f.nvOpenErr = errors.New("optional NVIDIA driver unavailable")
	caps, err := b.Capabilities(context.Background(), device)
	if err != nil || len(caps.Protocols) != 1 || caps.Protocols[0] != model.ProtocolVCP || !caps.CanReadInput || caps.UnavailableReason != "" {
		t.Fatalf("可选NVIDIA失败撤销标准能力：%+v %v", caps, err)
	}
	obs, err = b.ReadInput(context.Background(), device, standardScheme())
	if err != nil || obs.Value == nil || *obs.Value != 17 || len(f.reads) != 1 {
		t.Fatalf("NVIDIA环境标准读回回归：%+v %v", obs, err)
	}
	receipt, err := b.SetInput(context.Background(), device, standardScheme())
	if err != nil || receipt.SentAt.IsZero() || len(f.writes) != 1 || len(f.nv.writes) != 0 || len(f.session.writes) != 0 {
		t.Fatalf("NVIDIA环境标准提交回归：%+v %v", receipt, err)
	}
}
