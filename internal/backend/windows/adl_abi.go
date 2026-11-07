package windows

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/model"
)

// 这里只按公开C ABI手写本功能必需的布局，不包含或分发SDK、驱动或示例源码。
// 核对基准为GPUOpen display-library固定提交84c881cb2136f04287eb3af12de00a9e2867aa9e
// 的include/adl_structures.h和adl_defines.h；该ABI来源不代表SDK整体许可已获澄清。
// Windows C int始终32位，不能改成Go int或把返回值解释为BOOL/GetLastError。
type adlAdapterInfo struct {
	Size, Index                    int32
	UDID                           [256]byte
	Bus, Device, Function, Vendor  int32
	AdapterName, DisplayName       [256]byte
	Present, Exist                 int32
	DriverPath, DriverPathExt, PNP [256]byte
	OSIndex                        int32
}

type adlDisplayID struct {
	LogicalDisplay, PhysicalDisplay int32
	LogicalAdapter, PhysicalAdapter int32
}

type adlDisplayInfo struct {
	ID                                   adlDisplayID
	Controller                           int32
	Name, Manufacturer                   [256]byte
	Type, Output, Connector, Mask, Value int32
}

type adlEDIDData struct {
	Size, Flag, DataSize, Block int32
	Data                        [256]byte
	Reserved                    [4]int32
}

// 启动时也校验关键偏移，避免非测试构建因布局变化把驱动内存解读成另一结构。
func validateADLABI() error {
	var a adlAdapterInfo
	var d adlDisplayInfo
	var e adlEDIDData
	if unsafe.Sizeof(a) != 1572 || unsafe.Offsetof(a.Index) != 4 || unsafe.Offsetof(a.UDID) != 8 ||
		unsafe.Offsetof(a.Bus) != 264 || unsafe.Offsetof(a.Device) != 268 || unsafe.Offsetof(a.Function) != 272 ||
		unsafe.Offsetof(a.Vendor) != 276 || unsafe.Offsetof(a.DisplayName) != 536 || unsafe.Offsetof(a.Present) != 792 ||
		unsafe.Offsetof(a.Exist) != 796 || unsafe.Offsetof(a.PNP) != 1312 || unsafe.Offsetof(a.OSIndex) != 1568 ||
		unsafe.Sizeof(d) != 552 || unsafe.Sizeof(d.ID) != 16 || unsafe.Offsetof(d.Controller) != 16 ||
		unsafe.Offsetof(d.Name) != 20 || unsafe.Offsetof(d.Manufacturer) != 276 || unsafe.Offsetof(d.Type) != 532 ||
		unsafe.Offsetof(d.Output) != 536 || unsafe.Offsetof(d.Connector) != 540 || unsafe.Offsetof(d.Mask) != 544 || unsafe.Offsetof(d.Value) != 548 ||
		unsafe.Sizeof(e) != 288 || unsafe.Offsetof(e.DataSize) != 8 || unsafe.Offsetof(e.Block) != 12 ||
		unsafe.Offsetof(e.Data) != 16 || unsafe.Offsetof(e.Reserved) != 272 {
		return lgUnsupported("当前ADL公共ABI布局不受支持")
	}
	return nil
}

func adlStatus(stage string, result uintptr) error {
	status := int32(uint32(result))
	if status == 0 {
		return nil
	}
	return model.NewError(model.CodeTransport, fmt.Sprintf("%s失败：ADL状态%d", stage, status), nil)
}

func requireADLArchitecture(architecture string) error {
	if architecture != "amd64" {
		return lgUnsupported("LG AMD ADL通路只支持Windows amd64架构")
	}
	return validateADLABI()
}

// 该边界集中处理SDK分配的数组：失败也可能返回非空指针，必须在所有路径释放。
// 在返回Go切片前复制原生内存，使后续Control_Destroy或卸载DLL不影响已取得的快照。
func copyADLDisplays(count int32, pointer unsafe.Pointer, callErr error, release func(unsafe.Pointer) error) (result []adlDisplayInfo, err error) {
	if pointer != nil {
		defer func() { err = errors.Join(err, release(pointer)) }()
	}
	if callErr != nil {
		return nil, callErr
	}
	if count < 0 || count > maxNativeDisplayItems || (count > 0 && pointer == nil) || (count == 0 && pointer != nil) {
		return nil, nativeInvalid("ADL输出数组数量或指针无效")
	}
	if count == 0 {
		return nil, nil
	}
	return append([]adlDisplayInfo(nil), unsafe.Slice((*adlDisplayInfo)(pointer), int(count))...), nil
}
