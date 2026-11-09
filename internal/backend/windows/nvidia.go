package windows

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/model"
)

// 会话保持结构化可选接口，宿主测试只替换驱动调用，实际厂商选择、唯一性
// 校验与写前复核仍执行产品代码。类型别名兼容已有匿名接口测试替身。
type nvapiSession = interface {
	displays() ([]uintptr, error)
	displayName(uintptr) (string, error)
	physicalGPUs(uintptr) ([]uintptr, error)
	outputID(uintptr) (uint32, error)
	edid(uintptr, uint32) ([]byte, error)
	writeI2C(uintptr, uint32, byte, []byte, []byte) error
	close() error
}

type nativeNVIDIAAPI interface {
	displayVendor(string) (uint32, error)
	openNVAPI() (nvapiSession, error)
}

type nvapiTarget struct {
	display, gpu uintptr
	output       uint32
}

func resolveNVIDIATarget(ctx context.Context, session nvapiSession, snapshot lgIdentitySnapshot) (nvapiTarget, error) {
	displays, err := session.displays()
	if err != nil {
		return nvapiTarget{}, err
	}
	if len(displays) > maxNativeDisplayItems {
		return nvapiTarget{}, nativeInvalid("NVAPI显示源数量超出安全上限")
	}
	var display uintptr
	seen := make(map[uintptr]bool)
	for _, candidate := range displays {
		if err := ctx.Err(); err != nil {
			return nvapiTarget{}, err
		}
		if candidate == 0 || seen[candidate] {
			return nvapiTarget{}, nativeAmbiguous("NVAPI显示源句柄为空或重复")
		}
		seen[candidate] = true
		name, err := session.displayName(candidate)
		if err != nil {
			return nvapiTarget{}, err
		}
		if strings.ContainsRune(name, 0) {
			return nvapiTarget{}, nativeInvalid("NVAPI显示源名称含嵌入NUL")
		}
		if normalizeNativeName(name) == snapshot.gdi {
			if display != 0 {
				return nvapiTarget{}, nativeAmbiguous("活动GDI源对应多个NVAPI显示源")
			}
			display = candidate
		}
	}
	if display == 0 {
		return nvapiTarget{}, lgUnsupported("活动GDI源没有对应NVIDIA显示源")
	}
	gpus, err := session.physicalGPUs(display)
	if err != nil {
		return nvapiTarget{}, err
	}
	// 官方接口在多GPU时给出“活动输出优先”的次序，但该次序不能证明目标
	// 唯一；SLI、跨GPU和重复候选都必须拒绝，不能直接使用数组第一项。
	if len(gpus) != 1 || gpus[0] == 0 {
		return nvapiTarget{}, nativeAmbiguous("NVIDIA显示源不能唯一关联一个物理GPU")
	}
	output, err := session.outputID(display)
	if err != nil {
		return nvapiTarget{}, err
	}
	if output == 0 || output&(output-1) != 0 {
		return nvapiTarget{}, nativeAmbiguous("NVIDIA输出标记不是唯一单bit物理输出")
	}
	data, err := session.edid(gpus[0], output)
	if err != nil {
		return nvapiTarget{}, err
	}
	edid, err := validatedLGEDID(data)
	if err != nil {
		return nvapiTarget{}, err
	}
	if !bytes.Equal(edid, snapshot.edid) {
		return nvapiTarget{}, lgUnsupported("NVIDIA输出完整EDID与Windows显示实例不匹配")
	}
	return nvapiTarget{display: display, gpu: gpus[0], output: output}, ctx.Err()
}

func (t *nativeTransport) withNVIDIALGInput(ctx context.Context, monitor PhysicalMonitor, before lgIdentitySnapshot, packet *[8]byte, api nativeNVIDIAAPI) (err error) {
	session, err := api.openNVAPI()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, session.close()) }()
	first, err := resolveNVIDIATarget(ctx, session, before)
	if err != nil {
		return err
	}
	if packet == nil {
		return ctx.Err()
	}
	// Windows的一对一路由先排除克隆目标；NVAPI的单bit输出在Clone/Span下
	// 可能仅表示主屏，不能单独作为写入证据。两端完整身份必须在写前再次一致。
	after, err := t.lgSnapshot(ctx, monitor)
	if err != nil {
		return err
	}
	second, err := resolveNVIDIATarget(ctx, session, after)
	if err != nil {
		return err
	}
	final, err := t.lgSnapshot(ctx, monitor)
	if err != nil {
		return err
	}
	if !sameLGSnapshot(before, after) || !sameLGSnapshot(before, final) || first != second {
		return model.NewError(model.CodeUnreachable, "LG写入前Windows拓扑或NVIDIA输出身份已变化", nil)
	}
	vendor, err := api.displayVendor(final.gdi)
	if err != nil {
		return err
	}
	if vendor != 0x10de {
		return model.NewError(model.CodeUnreachable, "LG写入前活动显卡厂商已变化", nil)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// NVAPI将目的地址、寄存器和其余六字节分开传入，但总线上的报文与ADL
	// 完全一致。同步写只调用一次；失败也可能已经提交，不能重试或改走VCP/ADL。
	return session.writeI2C(first.gpu, first.output, packet[0], packet[1:2], packet[2:])
}

// ABI仅按NVIDIA公开头独立声明：nvapi.h / nvapi_interface.h，固定版本
// 70d337db9186e968eab622f7e786de7e437faf3d。指针按Win64自然对齐，
// 不使用packed或手工补齐猜测；版本字段由真实sizeof与官方版本号共同构造。
type nvapiI2CInfo struct {
	Version       uint32
	DisplayMask   uint32
	IsDDCPort     uint8
	DeviceAddress uint8
	Register      *byte
	RegisterSize  uint32
	Data          *byte
	DataSize      uint32
	Speed         uint32
	SpeedKhz      uint32
	PortID        uint8
	IsPortIDSet   uint32
}

type nvapiEDID struct {
	Version uint32
	Data    [256]byte
	Size    uint32
	ID      uint32
	Offset  uint32
}

func validateNVAPIABI() error {
	var i nvapiI2CInfo
	var e nvapiEDID
	checks := []struct{ got, want uintptr }{
		{unsafe.Sizeof(i), 64}, {unsafe.Offsetof(i.Version), 0},
		{unsafe.Offsetof(i.DisplayMask), 4}, {unsafe.Offsetof(i.IsDDCPort), 8},
		{unsafe.Offsetof(i.DeviceAddress), 9}, {unsafe.Offsetof(i.Register), 16},
		{unsafe.Offsetof(i.RegisterSize), 24}, {unsafe.Offsetof(i.Data), 32},
		{unsafe.Offsetof(i.DataSize), 40}, {unsafe.Offsetof(i.Speed), 44},
		{unsafe.Offsetof(i.SpeedKhz), 48}, {unsafe.Offsetof(i.PortID), 52},
		{unsafe.Offsetof(i.IsPortIDSet), 56}, {unsafe.Sizeof(e), 272},
		{unsafe.Offsetof(e.Data), 4}, {unsafe.Offsetof(e.Size), 260},
		{unsafe.Offsetof(e.ID), 264}, {unsafe.Offsetof(e.Offset), 268},
	}
	for _, check := range checks {
		if check.got != check.want {
			return lgUnsupported("NVAPI结构布局不符合已核验Win64 ABI")
		}
	}
	return nil
}

func nvapiStatus(stage string, result uintptr) error {
	status := int32(uint32(result))
	if status == 0 {
		return nil
	}
	code := model.CodeTransport
	switch status {
	case -2, -3, -6, -104:
		code = model.CodeUnsupportedOperation
	case -8, -10:
		code = model.CodeUnreachable
	}
	// NVAPI返回有符号状态码，不使用Win32的GetLastError/BOOL约定。保留阶段
	// 和原始十进制码，避免把负返回值解释成成功或丢失驱动诊断。
	return model.NewError(code, fmt.Sprintf("%s失败：NVAPI %d", stage, status), nil)
}
