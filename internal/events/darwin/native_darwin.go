//go:build darwin && cgo

package darwin

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include "bridge_darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/model"
)

type nativeSubscription struct {
	source *C.DKSource
}

func openNative(ctx context.Context) (subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var failure C.DKError
	source := C.dk_open(&failure)
	if source == nil {
		return nil, nativeError(failure)
	}
	listener := &nativeSubscription{source: source}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, listener.close())
	}
	return listener, nil
}

func (n *nativeSubscription) capabilities() []model.EventCapability {
	var capabilities C.DKCapabilities
	C.dk_capabilities(n.source, &capabilities)
	return eventCapabilities(capabilities.usb_available != 0, capabilities.resume_available != 0,
		C.GoString(&capabilities.usb_error.message[0]), C.GoString(&capabilities.resume_error.message[0]))
}

func (n *nativeSubscription) scan(ctx context.Context) ([]nativeDevice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var devices *C.DKDevice
	var count C.size_t
	var failure C.DKError
	if C.dk_scan(n.source, &devices, &count, &failure) != 0 {
		return nil, nativeError(failure)
	}
	defer C.dk_free_devices(devices)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uint64(count) > C.DK_MAX_DEVICES {
		return nil, model.NewError(model.CodeTransport, "原生USB枚举数量超过桥接边界", nil)
	}
	result := make([]nativeDevice, 0, int(count))
	for _, device := range unsafe.Slice(devices, int(count)) {
		result = append(result, copyNativeDevice(device))
	}
	return result, nil
}

func (n *nativeSubscription) poll(ctx context.Context, wait time.Duration) ([]notification, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var buffer [C.DK_QUEUE_CAPACITY]C.DKNotification
	var count C.size_t
	var failure C.DKError
	if C.dk_poll(n.source, C.double(wait.Seconds()), &buffer[0], C.size_t(len(buffer)), &count, &failure) != 0 {
		return nil, nativeError(failure)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if count > C.size_t(len(buffer)) {
		return nil, model.NewError(model.CodeTransport, "原生通知数量超过桥接缓冲区", nil)
	}
	result := make([]notification, 0, int(count))
	for _, value := range buffer[:int(count)] {
		var kind model.EventKind
		switch value.kind {
		case C.DK_USB_ADDED:
			kind = model.EventUSBAdded
		case C.DK_USB_REMOVED:
			kind = model.EventUSBRemoved
		case C.DK_RESUME:
			kind = model.EventResume
		default:
			return nil, model.NewError(model.CodeTransport, "IOKit桥接返回未知通知", nil)
		}
		result = append(result, notification{
			Kind: kind, Device: copyNativeDevice(value.device),
			ObservedAt: time.Unix(int64(value.observed_seconds), int64(value.observed_nanoseconds)).UTC(),
		})
	}
	return result, nil
}

func copyNativeDevice(device C.DKDevice) nativeDevice {
	// C缓冲区随后会被复用或释放，字符串必须在原生调用返回边界复制成Go值。
	return nativeDevice{ID: uint64(device.registry_id), USB: model.USBDevice{
		VendorID: uint16(device.vendor_id), ProductID: uint16(device.product_id),
		Serial: C.GoString(&device.serial[0]), Path: C.GoString(&device.path[0]),
	}}
}

func (n *nativeSubscription) close() error {
	if n.source == nil {
		return nil
	}
	var failure C.DKError
	C.dk_close(n.source, &failure)
	n.source = nil
	return nativeError(failure)
}

func nativeError(failure C.DKError) error {
	if failure.code == 0 {
		return nil
	}
	code := model.CodeIO
	switch failure.code {
	case C.DK_ERROR_PERMISSION:
		code = model.CodePermissionDenied
	case C.DK_ERROR_UNAVAILABLE:
		code = model.CodeBackendUnavailable
	case C.DK_ERROR_OVERFLOW:
		code = model.CodeTransport
	}
	message := C.GoString(&failure.message[0])
	if failure.native_code != 0 {
		message = fmt.Sprintf("%s（系统代码0x%x）", message, uint32(failure.native_code))
	}
	return model.NewError(code, message, nil)
}
