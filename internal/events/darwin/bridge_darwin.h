//go:build darwin && cgo

#ifndef DISPLAYKVM_EVENTS_DARWIN_BRIDGE_H
#define DISPLAYKVM_EVENTS_DARWIN_BRIDGE_H

#include <stddef.h>
#include <stdint.h>

#define DK_QUEUE_CAPACITY 256
#define DK_MAX_DEVICES 4096

enum {
    DK_ERROR_PERMISSION = 1,
    DK_ERROR_UNAVAILABLE = 2,
    DK_ERROR_IO = 3,
    DK_ERROR_OVERFLOW = 4
};

enum {
    DK_USB_ADDED = 1,
    DK_USB_REMOVED = 2,
    DK_RESUME = 3
};

typedef struct {
    int code;
    int native_code;
    char message[256];
} DKError;

typedef struct {
    uint64_t registry_id;
    uint16_t vendor_id;
    uint16_t product_id;
    char serial[1024];
    char path[512];
} DKDevice;

typedef struct {
    int kind;
    DKDevice device;
    int64_t observed_seconds;
    int32_t observed_nanoseconds;
} DKNotification;

typedef struct {
    int usb_available;
    int resume_available;
    DKError usb_error;
    DKError resume_error;
} DKCapabilities;

typedef struct DKSource DKSource;

/* 全部入口由同一个固定线程调用。返回的DKSource与设备列表均为C所有内存，
 * 回调不保存Go指针；调用方分别通过dk_close和dk_free_devices释放。 */
DKSource *dk_open(DKError *error);
void dk_capabilities(DKSource *source, DKCapabilities *capabilities);
int dk_scan(DKSource *source, DKDevice **devices, size_t *count, DKError *error);
void dk_free_devices(DKDevice *devices);
int dk_poll(DKSource *source, double wait_seconds, DKNotification *notifications,
            size_t capacity, size_t *count, DKError *error);
int dk_close(DKSource *source, DKError *error);

#endif
