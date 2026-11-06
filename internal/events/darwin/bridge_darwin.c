//go:build darwin && cgo

#include "bridge_darwin.h"

#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitKeys.h>
#include <IOKit/IOKitLib.h>
#include <IOKit/IOMessage.h>
#include <IOKit/IOReturn.h>
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <IOKit/usb/IOUSBHostFamilyDefinitions.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

struct DKSource {
    CFRunLoopRef loop;
    IONotificationPortRef usb_port;
    CFRunLoopSourceRef usb_runloop_source;
    io_iterator_t added_iterator;
    io_iterator_t removed_iterator;
    IONotificationPortRef power_port;
    CFRunLoopSourceRef power_runloop_source;
    io_connect_t power_connection;
    io_object_t power_notifier;
    DKCapabilities capabilities;
    DKError fatal;
    DKNotification queue[DK_QUEUE_CAPACITY];
    size_t head;
    size_t count;
    int initialising;
};

static void dk_set_error(DKError *error, int code, int native_code, const char *message) {
    if (error->code != 0) return;
    error->code = code;
    error->native_code = native_code;
    snprintf(error->message, sizeof(error->message), "%s", message);
}

static void dk_set_io_error(DKError *error, IOReturn result, const char *message) {
    int code = (result == kIOReturnNotPrivileged || result == kIOReturnNotPermitted)
        ? DK_ERROR_PERMISSION : DK_ERROR_IO;
    dk_set_error(error, code, (int)result, message);
}

static void dk_release_object(io_object_t object, DKError *error) {
    if (object == IO_OBJECT_NULL) return;
    IOReturn result = IOObjectRelease(object);
    if (result != kIOReturnSuccess) dk_set_io_error(error, result, "释放IOKit对象失败");
}

static int dk_number(io_registry_entry_t entry, CFStringRef key, uint16_t *out, DKError *error) {
    CFTypeRef property = IORegistryEntryCreateCFProperty(entry, key, kCFAllocatorDefault, 0);
    int64_t value = 0;
    int valid = property != NULL && CFGetTypeID(property) == CFNumberGetTypeID()
        && CFNumberGetValue((CFNumberRef)property, kCFNumberSInt64Type, &value)
        && value >= 0 && value <= UINT16_MAX;
    if (property != NULL) CFRelease(property);
    if (!valid) {
        dk_set_error(error, DK_ERROR_IO, 0, "USB设备缺少有效的厂商或产品编号");
        return -1;
    }
    *out = (uint16_t)value;
    return 0;
}

static int dk_read_device(io_registry_entry_t entry, DKDevice *device, DKError *error) {
    memset(device, 0, sizeof(*device));
    IOReturn result = IORegistryEntryGetRegistryEntryID(entry, &device->registry_id);
    if (result != kIOReturnSuccess) {
        dk_set_io_error(error, result, "读取USB注册表实例编号失败");
        return -1;
    }
    if (dk_number(entry, CFSTR(kUSBHostMatchingPropertyVendorID), &device->vendor_id, error) != 0
        || dk_number(entry, CFSTR(kUSBHostMatchingPropertyProductID), &device->product_id, error) != 0) return -1;

    /* 序列号允许缺失或重复，不能代替本轮注册表实例编号。不能截断后伪装成另一个
     * 有效序列号；异常类型或超长值明确失败，避免规则匹配获得错误身份。 */
    CFTypeRef serial = IORegistryEntryCreateCFProperty(entry,
        CFSTR(kUSBHostDevicePropertySerialNumberString), kCFAllocatorDefault, 0);
    if (serial != NULL) {
        int valid = CFGetTypeID(serial) == CFStringGetTypeID()
            && CFStringGetCString((CFStringRef)serial, device->serial, sizeof(device->serial), kCFStringEncodingUTF8);
        CFRelease(serial);
        if (!valid) {
            dk_set_error(error, DK_ERROR_IO, 0, "USB序列号不是可完整读取的字符串");
            return -1;
        }
    }
    io_string_t path;
    result = IORegistryEntryGetPath(entry, kIOServicePlane, path);
    if (result != kIOReturnSuccess) {
        dk_set_io_error(error, result, "读取USB注册表路径失败");
        return -1;
    }
    snprintf(device->path, sizeof(device->path), "%s", path);
    return 0;
}

static void dk_enqueue(DKSource *source, int kind, const DKDevice *device) {
    if (source->fatal.code != 0) return;
    if (source->count == DK_QUEUE_CAPACITY) {
        /* 原生回调不能等待Go消费者，否则会延迟必须及时确认的系统睡眠请求。
         * 队列耗尽时记录故障，后续Go调用会失败并注销，禁止静默丢弃后继续运行。 */
        dk_set_error(&source->fatal, DK_ERROR_OVERFLOW, 0, "IOKit原生通知队列已满，需重新订阅并初扫");
        return;
    }
    struct timespec observed;
    if (clock_gettime(CLOCK_REALTIME, &observed) != 0) {
        dk_set_error(&source->fatal, DK_ERROR_IO, errno, "读取原生通知观测时间失败");
        return;
    }
    size_t index = (source->head + source->count) % DK_QUEUE_CAPACITY;
    DKNotification *notification = &source->queue[index];
    memset(notification, 0, sizeof(*notification));
    notification->kind = kind;
    if (device != NULL) notification->device = *device;
    notification->observed_seconds = observed.tv_sec;
    notification->observed_nanoseconds = (int32_t)observed.tv_nsec;
    source->count++;
}

static void dk_usb_added(void *context, io_iterator_t iterator) {
    DKSource *source = context;
    io_service_t entry;
    /* 每次必须耗尽迭代器才能重新启用通知；即使已故障，也必须释放每个取出的对象。
     * 注册后首次手动耗尽只用于启用监听，不能把已存在设备当成刚刚插入。 */
    while ((entry = IOIteratorNext(iterator)) != IO_OBJECT_NULL) {
        if (!source->initialising && source->fatal.code == 0) {
            DKDevice device;
            if (dk_read_device(entry, &device, &source->fatal) == 0) dk_enqueue(source, DK_USB_ADDED, &device);
        }
        dk_release_object(entry, &source->fatal);
    }
    if (!IOIteratorIsValid(iterator)) {
        dk_set_error(&source->fatal, DK_ERROR_IO, 0, "USB首次匹配迭代器失效，需重新订阅");
    }
}

static void dk_usb_removed(void *context, io_iterator_t iterator) {
    DKSource *source = context;
    io_service_t entry;
    while ((entry = IOIteratorNext(iterator)) != IO_OBJECT_NULL) {
        if (!source->initialising && source->fatal.code == 0) {
            DKDevice device = {0};
            IOReturn result = IORegistryEntryGetRegistryEntryID(entry, &device.registry_id);
            if (result == kIOReturnSuccess) {
                /* 终止对象的属性可能已撤销，Go侧按实例编号读取本轮缓存，不能再读设备身份。 */
                dk_enqueue(source, DK_USB_REMOVED, &device);
            } else {
                dk_set_io_error(&source->fatal, result, "读取已移除USB实例编号失败");
            }
        }
        dk_release_object(entry, &source->fatal);
    }
    if (!IOIteratorIsValid(iterator)) {
        dk_set_error(&source->fatal, DK_ERROR_IO, 0, "USB终止迭代器失效，需重新订阅");
    }
}

static void dk_power_changed(void *context, io_service_t service, natural_t message_type, void *argument) {
    (void)service;
    DKSource *source = context;
    switch (message_type) {
        case kIOMessageCanSystemSleep:
        case kIOMessageSystemWillSleep: {
            /* 两个请求都必须确认；先确认再处理任何队列故障，绝不因观察者阻塞或溢出
             * 拖延睡眠，也不调用IOCancelPowerChange否决系统决定。睡眠本身不产生事件。 */
            IOReturn result = IOAllowPowerChange(source->power_connection, (intptr_t)argument);
            if (result != kIOReturnSuccess) dk_set_io_error(&source->fatal, result, "确认系统睡眠许可失败");
            break;
        }
        case kIOMessageSystemHasPoweredOn:
            /* WillPowerOn时系统资源尚未恢复；仅完整唤醒归一为resume，且不能对唤醒消息ack。 */
            dk_enqueue(source, DK_RESUME, NULL);
            break;
        default:
            break;
    }
}

static void dk_cleanup_usb(DKSource *source, DKError *error) {
    if (source->usb_runloop_source != NULL) {
        CFRunLoopRemoveSource(source->loop, source->usb_runloop_source, kCFRunLoopDefaultMode);
        source->usb_runloop_source = NULL;
    }
    dk_release_object(source->added_iterator, error);
    dk_release_object(source->removed_iterator, error);
    source->added_iterator = source->removed_iterator = IO_OBJECT_NULL;
    if (source->usb_port != NULL) {
        IONotificationPortDestroy(source->usb_port);
        source->usb_port = NULL;
    }
}

static void dk_cleanup_power(DKSource *source, DKError *error) {
    if (source->power_runloop_source != NULL) {
        CFRunLoopRemoveSource(source->loop, source->power_runloop_source, kCFRunLoopDefaultMode);
        source->power_runloop_source = NULL;
    }
    /* notifier是IORegisterForSystemPower返回的专属通知引用，先注销它再销毁端口；
     * connection另有所有权，仍须IOServiceClose。失败时继续尝试其余释放并保留首个错误。 */
    if (source->power_notifier != IO_OBJECT_NULL) {
        IOReturn result = IODeregisterForSystemPower(&source->power_notifier);
        if (result != kIOReturnSuccess) dk_set_io_error(error, result, "注销系统电源通知失败");
        source->power_notifier = IO_OBJECT_NULL;
    }
    if (source->power_connection != IO_OBJECT_NULL) {
        IOReturn result = IOServiceClose(source->power_connection);
        if (result != kIOReturnSuccess) dk_set_io_error(error, result, "关闭系统电源连接失败");
        source->power_connection = IO_OBJECT_NULL;
    }
    if (source->power_port != NULL) {
        IONotificationPortDestroy(source->power_port);
        source->power_port = NULL;
    }
}

static void dk_open_usb(DKSource *source) {
    DKError *error = &source->capabilities.usb_error;
    source->usb_port = IONotificationPortCreate(MACH_PORT_NULL);
    if (source->usb_port == NULL) {
        dk_set_error(error, DK_ERROR_UNAVAILABLE, 0, "无法创建IOKit USB通知端口");
        return;
    }
    source->usb_runloop_source = IONotificationPortGetRunLoopSource(source->usb_port);
    if (source->usb_runloop_source == NULL) {
        dk_set_error(error, DK_ERROR_UNAVAILABLE, 0, "IOKit USB通知没有事件循环来源");
        goto failed;
    }
    CFRunLoopAddSource(source->loop, source->usb_runloop_source, kCFRunLoopDefaultMode);

    /* 匹配字典被每次注册调用消费，包括注册失败；必须分别创建且不能再次CFRelease。
     * 先启用终止通知再启用首次匹配，随后独立初扫，缩小订阅与枚举之间的观察窗口。 */
    CFMutableDictionaryRef matching = IOServiceMatching(kIOUSBHostDeviceClassName);
    if (matching == NULL) {
        dk_set_error(error, DK_ERROR_IO, 0, "创建USB终止匹配条件失败");
        goto failed;
    }
    IOReturn result = IOServiceAddMatchingNotification(source->usb_port, kIOTerminatedNotification,
        matching, dk_usb_removed, source, &source->removed_iterator);
    if (result != kIOReturnSuccess) {
        dk_set_io_error(error, result, "注册USB终止通知失败");
        goto failed;
    }
    dk_usb_removed(source, source->removed_iterator);

    matching = IOServiceMatching(kIOUSBHostDeviceClassName);
    if (matching == NULL) {
        dk_set_error(error, DK_ERROR_IO, 0, "创建USB首次匹配条件失败");
        goto failed;
    }
    result = IOServiceAddMatchingNotification(source->usb_port, kIOFirstMatchNotification,
        matching, dk_usb_added, source, &source->added_iterator);
    if (result != kIOReturnSuccess) {
        dk_set_io_error(error, result, "注册USB首次匹配通知失败");
        goto failed;
    }
    dk_usb_added(source, source->added_iterator);
    if (source->fatal.code != 0) {
        *error = source->fatal;
        goto failed;
    }
    source->capabilities.usb_available = 1;
    return;
failed:
    dk_cleanup_usb(source, &source->fatal);
}

static void dk_open_power(DKSource *source) {
    DKError *error = &source->capabilities.resume_error;
    source->power_connection = IORegisterForSystemPower(source, &source->power_port,
        dk_power_changed, &source->power_notifier);
    if (source->power_connection == IO_OBJECT_NULL || source->power_port == NULL) {
        /* 此API只返回空连接，没有权限错误码；不能臆断成权限拒绝或自动请求额外权限。 */
        dk_set_error(error, DK_ERROR_UNAVAILABLE, 0, "系统电源通知注册失败，权限或系统服务可能不可用");
        goto failed;
    }
    source->power_runloop_source = IONotificationPortGetRunLoopSource(source->power_port);
    if (source->power_runloop_source == NULL) {
        dk_set_error(error, DK_ERROR_UNAVAILABLE, 0, "系统电源通知没有事件循环来源");
        goto failed;
    }
    CFRunLoopAddSource(source->loop, source->power_runloop_source, kCFRunLoopDefaultMode);
    source->capabilities.resume_available = 1;
    return;
failed:
    dk_cleanup_power(source, &source->fatal);
}

static void dk_pump_pending(DKSource *source) {
    /* 不等待新消息，分批处理已经就绪的来源；持续高频消息不能令取消无限饥饿。
     * 达到上限视为无法可靠跟上的故障，通知队列或事件循环均不无界增长。 */
    for (size_t i = 0; i < DK_QUEUE_CAPACITY * 2; i++) {
        SInt32 result = CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0, true);
        if (result != kCFRunLoopRunHandledSource) return;
    }
    dk_set_error(&source->fatal, DK_ERROR_OVERFLOW, 0, "IOKit事件循环持续积压，需重新订阅");
}

DKSource *dk_open(DKError *error) {
    DKSource *source = calloc(1, sizeof(*source));
    if (source == NULL) {
        dk_set_error(error, DK_ERROR_IO, errno, "分配IOKit监听上下文失败");
        return NULL;
    }
    /* GetCurrent与GetRunLoopSource都是借用引用；只保留当前RunLoop一份引用。
     * 端口来源由端口拥有，清理时移除来源后销毁端口，不能重复CFRelease来源。 */
    source->loop = (CFRunLoopRef)CFRetain(CFRunLoopGetCurrent());
    source->initialising = 1;
    dk_open_usb(source);
    dk_open_power(source);
    source->initialising = 0;
    return source;
}

void dk_capabilities(DKSource *source, DKCapabilities *capabilities) {
    *capabilities = source->capabilities;
}

int dk_scan(DKSource *source, DKDevice **devices, size_t *count, DKError *error) {
    *devices = NULL;
    *count = 0;
    if (!source->capabilities.usb_available) {
        *error = source->capabilities.usb_error;
        return -1;
    }
    CFMutableDictionaryRef matching = IOServiceMatching(kIOUSBHostDeviceClassName);
    if (matching == NULL) {
        dk_set_error(error, DK_ERROR_IO, 0, "创建USB初扫条件失败");
        return -1;
    }
    io_iterator_t iterator = IO_OBJECT_NULL;
    IOReturn result = IOServiceGetMatchingServices(MACH_PORT_NULL, matching, &iterator);
    if (result != kIOReturnSuccess) {
        dk_set_io_error(error, result, "USB初扫枚举失败");
        dk_release_object(iterator, error);
        return -1;
    }
    DKDevice *list = NULL;
    size_t length = 0;
    size_t capacity = 0;
    io_service_t entry;
    /* 订阅已启用，再逐项扫描；扫描中驱动同一线程的事件循环，将变化存入有限队列。
     * 返回后Go对账并固定本轮基线，不把IOIterator当作硬件事务快照。 */
    dk_pump_pending(source);
    while ((entry = IOIteratorNext(iterator)) != IO_OBJECT_NULL) {
        DKDevice device;
        int read_result = dk_read_device(entry, &device, error);
        dk_release_object(entry, error);
        if (read_result != 0 || error->code != 0) break;
        if (length == DK_MAX_DEVICES) {
            dk_set_error(error, DK_ERROR_OVERFLOW, 0, "USB初扫设备数量超过有限容量");
            break;
        }
        if (length == capacity) {
            size_t next = capacity == 0 ? 16 : capacity * 2;
            DKDevice *resized = realloc(list, next * sizeof(*list));
            if (resized == NULL) {
                dk_set_error(error, DK_ERROR_IO, errno, "分配USB初扫列表失败");
                break;
            }
            list = resized;
            capacity = next;
        }
        list[length++] = device;
        dk_pump_pending(source);
        if (source->fatal.code != 0) break;
    }
    /* IOIteratorNext返回零也可能表示失效，不能误判成完整的空枚举。
     * 扫描不完整时明确失败，由调用者重启这一轮，而不是返回USBKnown=true。 */
    if (!IOIteratorIsValid(iterator)) {
        dk_set_error(error, DK_ERROR_IO, 0, "USB初扫迭代器失效，不能确认启动基线");
    }
    dk_release_object(iterator, error);
    dk_pump_pending(source);
    if (error->code == 0 && source->fatal.code != 0) *error = source->fatal;
    if (error->code != 0) {
        free(list);
        return -1;
    }
    *devices = list;
    *count = length;
    return 0;
}

void dk_free_devices(DKDevice *devices) { free(devices); }

int dk_poll(DKSource *source, double wait_seconds, DKNotification *notifications,
            size_t capacity, size_t *count, DKError *error) {
    *count = 0;
    if (source->count == 0 && source->fatal.code == 0 && wait_seconds > 0) {
        /* 单次等待由Go限制为50ms。无需额外跨线程Stop或唤醒指针，取消不会与
         * 已释放的RunLoop/上下文竞争；每次返回后Go先检查上下文再继续监听。 */
        CFRunLoopRunInMode(kCFRunLoopDefaultMode, wait_seconds, false);
    }
    dk_pump_pending(source);
    if (source->fatal.code != 0) {
        *error = source->fatal;
        return -1;
    }
    while (source->count > 0 && *count < capacity) {
        notifications[(*count)++] = source->queue[source->head];
        source->head = (source->head + 1) % DK_QUEUE_CAPACITY;
        source->count--;
    }
    return 0;
}

int dk_close(DKSource *source, DKError *error) {
    if (source == NULL) return 0;
    /* 注销前处理已到达的睡眠确认。随后不再运行此来源，所有回调与释放都在
     * 同一线程串行完成；只有端口、迭代器和电源连接均处理完才释放回调上下文。 */
    dk_pump_pending(source);
    if (source->fatal.code != 0) *error = source->fatal;
    dk_cleanup_usb(source, error);
    dk_cleanup_power(source, error);
    CFRelease(source->loop);
    free(source);
    return error->code == 0 ? 0 : -1;
}
