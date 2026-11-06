//go:build windows

package windows

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/model"
)

const (
	wmDeviceChange     = 0x0219
	wmPowerBroadcast   = 0x0218
	wmSessionChange    = 0x02b1
	wmQueryEndSession  = 0x0011
	wmEndSession       = 0x0016
	wmClose            = 0x0010
	wmQuit             = 0x0012
	wmWake             = 0x8001
	dbtDeviceArrival   = 0x8000
	dbtDeviceRemoved   = 0x8004
	dbtDeviceInterface = 5
	maxNativeBuffer    = 1 << 20
)

type winGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var usbInterfaceGUID = winGUID{0xa5dcbf10, 0x6530, 0x11d2, [8]byte{0x90, 0x1f, 0x00, 0xc0, 0x4f, 0xb9, 0x51, 0xed}}

// 系统DLL和唯一回调桩按进程复用；NewCallback分配的桩没有释放接口，
// 因而不能每次Run重新创建。每轮窗口、通知和事件句柄则必须在原线程释放。
var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	wtsapi32 = syscall.NewLazyDLL("wtsapi32.dll")
	setupapi = syscall.NewLazyDLL("setupapi.dll")

	pRegisterClass    = user32.NewProc("RegisterClassExW")
	pUnregisterClass  = user32.NewProc("UnregisterClassW")
	pCreateWindow     = user32.NewProc("CreateWindowExW")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pDefWindowProc    = user32.NewProc("DefWindowProcW")
	pPeekMessage      = user32.NewProc("PeekMessageW")
	pTranslate        = user32.NewProc("TranslateMessage")
	pDispatch         = user32.NewProc("DispatchMessageW")
	pWaitMessages     = user32.NewProc("MsgWaitForMultipleObjectsEx")
	pPostMessage      = user32.NewProc("PostMessageW")
	pRegisterDevice   = user32.NewProc("RegisterDeviceNotificationW")
	pUnregisterDevice = user32.NewProc("UnregisterDeviceNotification")
	pModuleHandle     = kernel32.NewProc("GetModuleHandleW")
	pCreateEvent      = kernel32.NewProc("CreateEventW")
	pSetEvent         = kernel32.NewProc("SetEvent")
	pCloseHandle      = kernel32.NewProc("CloseHandle")
	pWTSRegister      = wtsapi32.NewProc("WTSRegisterSessionNotification")
	pWTSUnregister    = wtsapi32.NewProc("WTSUnRegisterSessionNotification")
	pWTSQuery         = wtsapi32.NewProc("WTSQuerySessionInformationW")
	pWTSFree          = wtsapi32.NewProc("WTSFreeMemory")

	windowStates   sync.Map
	callbackOnce   sync.Once
	windowCallback uintptr
	classSequence  atomic.Uint64
)

type windowClass struct {
	Size       uint32
	Style      uint32
	Procedure  uintptr
	ClassExtra int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	SmallIcon  uintptr
}

type windowMessage struct {
	Window  uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   struct{ X, Y int32 }
	Private uint32
}

type deviceBroadcastHeader struct {
	Size       uint32
	DeviceType uint32
	Reserved   uint32
}

type deviceBroadcastInterface struct {
	Header deviceBroadcastHeader
	Class  winGUID
	Name   [1]uint16
}

type nativePlatform struct{}

func newPlatform() platform { return nativePlatform{} }

func (nativePlatform) probe(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return model.NewError(model.CodeCanceled, "Windows系统探测已取消", err)
	}
	for _, proc := range nativeProcedures() {
		// Find返回缺失DLL/入口错误，不能使用MustLoadDLL或MustFindProc导致导入崩溃。
		if err := proc.Find(); err != nil {
			return winError("Windows系统API探测: "+proc.Name, err)
		}
	}
	session, err := currentSessionID()
	if err != nil {
		return err
	}
	if session == 0 {
		return model.NewError(model.CodeBackendUnavailable, "Windows来源仅支持登录后的用户会话，不支持Session 0或登录前服务", nil)
	}
	present, err := sessionPresent(session)
	if err != nil {
		return err
	}
	if !present {
		return model.NewError(model.CodeBackendUnavailable, "当前Windows会话没有已登录用户", nil)
	}
	if err := ctx.Err(); err != nil {
		return model.NewError(model.CodeCanceled, "Windows系统探测已取消", err)
	}
	return nil
}

func nativeProcedures() []*syscall.LazyProc {
	return []*syscall.LazyProc{
		pRegisterClass, pUnregisterClass, pCreateWindow, pDestroyWindow, pDefWindowProc,
		pPeekMessage, pTranslate, pDispatch, pWaitMessages, pPostMessage,
		pRegisterDevice, pUnregisterDevice, pModuleHandle, pCreateEvent, pSetEvent, pCloseHandle,
		pWTSRegister, pWTSUnregister, pWTSQuery, pWTSFree,
		pGetClassDevices, pEnumInterfaces, pInterfaceDetail, pDeviceProperty, pDestroyDeviceList,
	}
}

type scanRequest struct {
	ctx   context.Context
	reply chan scanResult
}

type scanResult struct {
	snapshot Snapshot
	err      error
}

type nativeSubscription struct {
	notices   chan notification
	requests  chan scanRequest
	done      chan struct{}
	ready     chan error
	mu        sync.Mutex
	window    uintptr
	stopEvent uintptr
	failure   error
	stopping  atomic.Bool
	closeOnce sync.Once
	session   uint32
}

func (p nativePlatform) open(ctx context.Context) (subscription, error) {
	if err := p.probe(ctx); err != nil {
		return nil, err
	}
	s := &nativeSubscription{
		notices: make(chan notification, notificationLimit), requests: make(chan scanRequest, 1),
		done: make(chan struct{}), ready: make(chan error, 1),
	}
	go s.loop(ctx)
	// 初始化失败也由原线程完成回收后才发送ready。取消不能遗留仍在注册的后台线程；
	// 若取消发生在同步系统调用期间，等待该调用返回，再立即唤醒并等待本轮退出。
	initErr := <-s.ready
	if initErr != nil {
		return nil, errors.Join(ctx.Err(), initErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(model.NewError(model.CodeCanceled, "Windows监听初始化已取消", err), s.Close())
	}
	return s, nil
}

func (s *nativeSubscription) Notifications() <-chan notification { return s.notices }
func (s *nativeSubscription) Done() <-chan struct{}              { return s.done }

func (s *nativeSubscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *nativeSubscription) record(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.failure = errors.Join(s.failure, err)
	s.mu.Unlock()
}

func (s *nativeSubscription) signalStop() error {
	s.stopping.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopEvent == 0 {
		return nil
	}
	ok, _, err := pSetEvent.Call(s.stopEvent)
	if ok == 0 {
		return winError("SetEvent停止Windows监听", err)
	}
	return nil
}

func (s *nativeSubscription) fail(err error) {
	s.record(err)
	s.record(s.signalStop())
}

func (s *nativeSubscription) Close() error {
	s.closeOnce.Do(func() { s.record(s.signalStop()) })
	<-s.done
	return s.Err()
}

func (s *nativeSubscription) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, model.NewError(model.CodeCanceled, "Windows设备扫描已取消", err)
	}
	request := scanRequest{ctx: ctx, reply: make(chan scanResult, 1)}
	select {
	case s.requests <- request:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-s.done:
		return Snapshot{}, subscriptionError(s)
	}
	// 不把Go指针交给窗口消息；消息只负责唤醒，实际请求由有界Go通道持有。
	// 持锁投递避免窗口销毁/句柄复用与PostMessage并发交错。
	s.mu.Lock()
	hwnd := s.window
	var postErr error
	if hwnd == 0 {
		postErr = model.NewError(model.CodeBackendUnavailable, "Windows监听窗口已销毁", nil)
	} else if ok, _, err := pPostMessage.Call(hwnd, wmWake, 0, 0); ok == 0 {
		postErr = winError("PostMessage唤醒Windows扫描", err)
	}
	s.mu.Unlock()
	if postErr != nil {
		s.fail(postErr)
		return Snapshot{}, postErr
	}
	select {
	case result := <-request.reply:
		return result.snapshot, result.err
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-s.done:
		return Snapshot{}, subscriptionError(s)
	}
}

func (s *nativeSubscription) loop(ctx context.Context) {
	runtime.LockOSThread()
	var className *uint16
	var instance, hwnd, deviceNotify uintptr
	var classRegistered, sessionRegistered, ready bool
	defer func() {
		s.stopping.Store(true)
		// 注册、注销、销毁均在窗口所属线程进行；先注销通知，再销毁窗口，
		// 且回调映射在DestroyWindow返回之后才移除，避免销毁中的回调悬空。
		if sessionRegistered {
			if ok, _, err := pWTSUnregister.Call(hwnd); ok == 0 {
				s.record(winError("WTSUnRegisterSessionNotification", err))
			}
		}
		if deviceNotify != 0 {
			if ok, _, err := pUnregisterDevice.Call(deviceNotify); ok == 0 {
				s.record(winError("UnregisterDeviceNotification", err))
			}
		}
		s.mu.Lock()
		s.window = 0
		s.mu.Unlock()
		if hwnd != 0 {
			if ok, _, err := pDestroyWindow.Call(hwnd); ok == 0 {
				s.record(winError("DestroyWindow", err))
			}
			windowStates.Delete(hwnd)
		}
		if classRegistered {
			if ok, _, err := pUnregisterClass.Call(uintptr(unsafe.Pointer(className)), instance); ok == 0 {
				s.record(winError("UnregisterClassW", err))
			}
		}
		// SetEvent与CloseHandle共用锁，不能在其他协程仍使用句柄时将其关闭。
		s.mu.Lock()
		stopEvent := s.stopEvent
		s.stopEvent = 0
		if stopEvent != 0 {
			if ok, _, err := pCloseHandle.Call(stopEvent); ok == 0 {
				s.failure = errors.Join(s.failure, winError("CloseHandle停止事件", err))
			}
		}
		s.mu.Unlock()
		runtime.KeepAlive(className)
		runtime.UnlockOSThread()
		close(s.done)
		if !ready {
			s.ready <- s.Err()
		}
	}()
	if err := ctx.Err(); err != nil {
		s.record(err)
		return
	}
	var err error
	s.session, err = currentSessionID()
	if err != nil {
		s.record(err)
		return
	}
	stopEvent, _, callErr := pCreateEvent.Call(0, 1, 0, 0)
	if stopEvent == 0 {
		s.record(winError("CreateEventW停止事件", callErr))
		return
	}
	s.mu.Lock()
	s.stopEvent = stopEvent
	s.mu.Unlock()
	instance, _, callErr = pModuleHandle.Call(0)
	if instance == 0 {
		s.record(winError("GetModuleHandleW", callErr))
		return
	}
	className, err = syscall.UTF16PtrFromString(fmt.Sprintf("DisplayKvmEvents_%d", classSequence.Add(1)))
	if err != nil {
		s.record(model.NewError(model.CodeInternal, "Windows监听窗口类名无效", err))
		return
	}
	callbackOnce.Do(func() { windowCallback = syscall.NewCallback(windowProcedure) })
	class := windowClass{Procedure: windowCallback, Instance: instance, ClassName: className}
	class.Size = uint32(unsafe.Sizeof(class))
	if atom, _, err := pRegisterClass.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		s.record(winError("RegisterClassExW", err))
		return
	}
	classRegistered = true
	// 隐藏顶层窗口没有WS_VISIBLE，不使用HWND_MESSAGE；消息专用窗口收不到
	// 系统广播的WM_POWERBROADCAST，替换窗口类型会丢失恢复事件。
	hwnd, _, callErr = pCreateWindow.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if hwnd == 0 {
		s.record(winError("CreateWindowExW", callErr))
		return
	}
	s.mu.Lock()
	s.window = hwnd
	s.mu.Unlock()
	windowStates.Store(hwnd, s)
	filter := deviceBroadcastInterface{Header: deviceBroadcastHeader{DeviceType: dbtDeviceInterface}, Class: usbInterfaceGUID}
	filter.Header.Size = uint32(unsafe.Sizeof(filter))
	deviceNotify, _, callErr = pRegisterDevice.Call(hwnd, uintptr(unsafe.Pointer(&filter)), 0)
	if deviceNotify == 0 {
		s.record(winError("RegisterDeviceNotificationW", callErr))
		return
	}
	if ok, _, err := pWTSRegister.Call(hwnd, 0); ok == 0 { // NOTIFY_FOR_THIS_SESSION
		s.record(winError("WTSRegisterSessionNotification", err))
		return
	}
	sessionRegistered = true
	if err := ctx.Err(); err != nil {
		s.record(err)
		return
	}
	ready = true
	s.ready <- nil
	for !s.stopping.Load() {
		var message windowMessage
		for !s.stopping.Load() {
			ok, _, _ := pPeekMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0, 1) // PM_REMOVE
			if ok == 0 {                                                                 // PeekMessage的0表示队列为空，不是GetMessage的错误语义。
				break
			}
			if message.Message == wmQuit {
				s.record(model.NewError(model.CodeBackendUnavailable, "Windows消息循环收到WM_QUIT", nil))
				return
			}
			pTranslate.Call(uintptr(unsafe.Pointer(&message)))
			pDispatch.Call(uintptr(unsafe.Pointer(&message)))
		}
		if s.stopping.Load() {
			return
		}
		select {
		case request := <-s.requests:
			snapshot, err := s.scan(request.ctx)
			request.reply <- scanResult{snapshot: snapshot, err: err}
		default:
		}
		// 独立内核事件可在消费者阻塞或PostMessage失败时唤醒线程；1000ms
		// 超时仅作SetEvent失败后的停止标记兜底，保留错误而非永久等待。
		result, _, err := pWaitMessages.Call(1, uintptr(unsafe.Pointer(&stopEvent)), 1000, 0x04ff, 0x0004) // QS_ALLINPUT, MWMO_INPUTAVAILABLE
		switch uint32(result) {
		case 0:
			return
		case 1, 258:
		case 0xffffffff:
			s.record(winError("MsgWaitForMultipleObjectsEx", err))
			return
		default:
			s.record(model.NewError(model.CodeIO, "Windows消息等待返回未知状态", nil))
			return
		}
	}
}

func (s *nativeSubscription) scan(ctx context.Context) (Snapshot, error) {
	devices, err := enumerateUSB(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	present, err := sessionPresent(s.session)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{ObservedAt: time.Now(), USBDevices: devices, USBKnown: true, SessionPresent: &present}, nil
}

func windowProcedure(hwnd, message, wParam, lParam uintptr) uintptr {
	value, ok := windowStates.Load(hwnd)
	if !ok {
		result, _, _ := pDefWindowProc.Call(hwnd, message, wParam, lParam)
		return result
	}
	s := value.(*nativeSubscription)
	if s.stopping.Load() {
		result, _, _ := pDefWindowProc.Call(hwnd, message, wParam, lParam)
		return result
	}
	n := notification{observedAt: time.Now()}
	switch message {
	case wmDeviceChange:
		if wParam != dbtDeviceArrival && wParam != dbtDeviceRemoved {
			return 0
		}
		path, relevant, err := copyNotificationPath(lParam)
		if err != nil {
			s.fail(err)
			return 0
		}
		if !relevant {
			return 0
		}
		n.kind, n.path = noticeUSB, path
	case wmPowerBroadcast:
		if !isAutomaticResume(uint32(wParam)) {
			return 1
		}
		n.kind = noticeResume
	case wmSessionChange:
		kind, relevant := classifySession(uint32(wParam), uint32(lParam), s.session)
		if !relevant {
			return 0
		}
		if kind == noticeLogoff {
			s.fail(sessionEnded())
			return 0
		}
		n.kind = kind
	case wmQueryEndSession:
		return 1
	case wmEndSession:
		if wParam != 0 {
			s.fail(sessionEnded())
		}
		return 0
	case wmClose:
		s.fail(model.NewError(model.CodeBackendUnavailable, "Windows监听窗口被关闭", nil))
		return 0
	case wmWake:
		return 0
	default:
		result, _, _ := pDefWindowProc.Call(hwnd, message, wParam, lParam)
		return result
	}
	// 回调不得执行设备扫描、等待消费者或持有系统内存。路径已复制为Go字符串，
	// 有界队列满时停止整轮监听并报错，不能静默丢掉登录或设备变化通知。
	if err := enqueueNotification(s.notices, n); err != nil {
		s.fail(err)
	}
	if message == wmPowerBroadcast || message == wmDeviceChange {
		return 1
	}
	return 0
}

func copyNotificationPath(pointer uintptr) (string, bool, error) {
	if pointer == 0 {
		return "", false, model.NewError(model.CodeIO, "Windows设备通知缺少数据", nil)
	}
	header := (*deviceBroadcastHeader)(unsafe.Pointer(pointer))
	if header.DeviceType != dbtDeviceInterface {
		return "", false, nil
	}
	var layout deviceBroadcastInterface
	offset := uint32(unsafe.Offsetof(layout.Name))
	if header.Size < offset+2 || header.Size > maxNativeBuffer || (header.Size-offset)%2 != 0 {
		return "", false, model.NewError(model.CodeIO, "Windows设备通知缓冲区大小无效", nil)
	}
	data := (*deviceBroadcastInterface)(unsafe.Pointer(pointer))
	if data.Class != usbInterfaceGUID {
		return "", false, nil
	}
	characters := unsafe.Slice((*uint16)(unsafe.Add(unsafe.Pointer(pointer), offset)), int((header.Size-offset)/2))
	for i, char := range characters {
		if char == 0 {
			if i == 0 {
				return "", false, model.NewError(model.CodeIO, "Windows USB设备通知路径为空", nil)
			}
			return syscall.UTF16ToString(characters[:i]), true, nil
		}
	}
	return "", false, model.NewError(model.CodeIO, "Windows设备通知路径没有终止符", nil)
}

func queryWTS(session, field uint32) ([]byte, error) {
	var buffer uintptr
	var size uint32
	ok, _, err := pWTSQuery.Call(0, uintptr(session), uintptr(field), uintptr(unsafe.Pointer(&buffer)), uintptr(unsafe.Pointer(&size)))
	if buffer != 0 {
		defer pWTSFree.Call(buffer)
	}
	if ok == 0 {
		return nil, winError("WTSQuerySessionInformationW", err)
	}
	if buffer == 0 || size == 0 || size > maxNativeBuffer {
		return nil, model.NewError(model.CodeIO, "Windows会话查询返回无效缓冲区", nil)
	}
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(buffer)), int(size))...), nil
}

func currentSessionID() (uint32, error) {
	data, err := queryWTS(0xffffffff, 4) // WTS_CURRENT_SESSION, WTSSessionId
	if err != nil {
		return 0, err
	}
	if len(data) != 4 {
		return 0, model.NewError(model.CodeIO, "Windows会话ID长度无效", nil)
	}
	return binary.LittleEndian.Uint32(data), nil
}

func sessionPresent(session uint32) (bool, error) {
	data, err := queryWTS(session, 5) // WTSUserName，仅用于确认是否登录，不输出用户名。
	if err != nil {
		return false, err
	}
	if len(data) < 2 || len(data)%2 != 0 || binary.LittleEndian.Uint16(data[len(data)-2:]) != 0 {
		return false, model.NewError(model.CodeIO, "Windows会话用户名缓冲区格式无效", nil)
	}
	return binary.LittleEndian.Uint16(data) != 0, nil
}

func winError(operation string, cause error) error {
	if cause == nil || errors.Is(cause, syscall.Errno(0)) {
		cause = errors.New("系统调用失败但未提供扩展错误码")
	}
	code := model.CodeIO
	if errors.Is(cause, syscall.Errno(5)) {
		code = model.CodePermissionDenied
	} else if errors.Is(cause, syscall.Errno(126)) || errors.Is(cause, syscall.Errno(127)) || errors.Is(cause, syscall.Errno(1702)) {
		code = model.CodeBackendUnavailable
	}
	return model.NewError(code, operation+"失败", cause)
}
