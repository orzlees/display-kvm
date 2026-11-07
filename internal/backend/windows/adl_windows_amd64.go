//go:build windows && amd64

package windows

import (
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/model"
)

type adlHeap struct {
	heap        uintptr
	alloc, free *syscall.LazyProc
	callback    uintptr
}

var (
	adlHeapOnce   sync.Once
	adlNativeHeap *adlHeap
	adlHeapErr    error
)

// 回调桩在进程内只创建一次，避免耗尽Go有限的Windows callback槽位。
// 返回的是同一进程堆的原生内存，不把Go堆地址交给驱动跨调用保留；析构结束前
// 堆和回调始终有效，输出数组也必须用该堆释放，不能使用Go GC或另一种free。
func adlAllocate(size uintptr) uintptr {
	count := int32(uint32(size))
	if count <= 0 || count > 64<<20 || adlNativeHeap == nil {
		return 0
	}
	pointer, _, _ := adlNativeHeap.alloc.Call(adlNativeHeap.heap, 0, uintptr(count))
	return pointer
}

func windowsADLHeap() (*adlHeap, error) {
	adlHeapOnce.Do(func() {
		kernel := syscall.NewLazyDLL("kernel32.dll")
		getHeap, alloc, free := kernel.NewProc("GetProcessHeap"), kernel.NewProc("HeapAlloc"), kernel.NewProc("HeapFree")
		for _, proc := range []*syscall.LazyProc{getHeap, alloc, free} {
			if err := proc.Find(); err != nil {
				adlHeapErr = nativeError("查找ADL原生堆入口", err)
				return
			}
		}
		heap, _, callErr := getHeap.Call()
		if heap == 0 {
			adlHeapErr = nativeBOOLResult("GetProcessHeap", heap, callErr)
			return
		}
		adlNativeHeap = &adlHeap{heap: heap, alloc: alloc, free: free}
		adlNativeHeap.callback = syscall.NewCallback(adlAllocate)
	})
	return adlNativeHeap, adlHeapErr
}

type windowsADLSession struct {
	dll                                                             *syscall.DLL
	heap                                                            *adlHeap
	create, destroy, count, adapterInfo, displayInfo, edidData, ddc *syscall.Proc
	createAttempted                                                 bool
	closed                                                          bool
}

func openPlatformADL() (result adlSession, err error) {
	if err := requireADLArchitecture(runtime.GOARCH); err != nil {
		return nil, err
	}
	directory, err := windowsSystemDirectory()
	if err != nil {
		return nil, err
	}
	// 仅从真实系统目录加载用户已安装的AMD驱动；不搜索当前目录/PATH，不下载或分发SDK/DLL。
	dll, err := syscall.LoadDLL(filepath.Join(directory, "atiadlxx.dll"))
	if err != nil {
		return nil, model.NewError(model.CodeUnsupportedOperation, "已安装的AMD atiadlxx.dll不可用："+err.Error(), err)
	}
	session := &windowsADLSession{dll: dll}
	defer func() {
		if err != nil {
			err = errors.Join(err, session.close())
		}
	}()
	entries := []struct {
		name        string
		destination **syscall.Proc
	}{
		{"ADL_Main_Control_Create", &session.create}, {"ADL_Main_Control_Destroy", &session.destroy},
		{"ADL_Adapter_NumberOfAdapters_Get", &session.count}, {"ADL_Adapter_AdapterInfo_Get", &session.adapterInfo},
		{"ADL_Display_DisplayInfo_Get", &session.displayInfo}, {"ADL_Display_EdidData_Get", &session.edidData},
		{"ADL_Display_DDCBlockAccess_Get", &session.ddc},
	}
	for _, entry := range entries {
		proc, findErr := dll.FindProc(entry.name)
		if findErr != nil {
			return nil, model.NewError(model.CodeUnsupportedOperation, "AMD驱动缺少"+entry.name+"："+findErr.Error(), findErr)
		}
		*entry.destination = proc
	}
	session.heap, err = windowsADLHeap()
	if err != nil {
		return nil, err
	}
	session.createAttempted = true
	status, _, _ := session.create.Call(session.heap.callback, 1)
	if err = adlStatus("ADL_Main_Control_Create", status); err != nil {
		return nil, err
	}
	return session, nil
}

func (s *windowsADLSession) adapters() ([]adlAdapterInfo, error) {
	var count int32
	status, _, _ := s.count.Call(uintptr(unsafe.Pointer(&count)))
	if err := adlStatus("ADL_Adapter_NumberOfAdapters_Get", status); err != nil {
		return nil, err
	}
	if count < 0 || count > maxNativeDisplayItems {
		return nil, nativeInvalid("ADL显卡数量无效")
	}
	if count == 0 {
		return nil, nil
	}
	buffer := make([]adlAdapterInfo, int(count))
	for index := range buffer {
		buffer[index].Size = int32(unsafe.Sizeof(adlAdapterInfo{}))
	}
	status, _, _ = s.adapterInfo.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer))*unsafe.Sizeof(adlAdapterInfo{}))
	runtime.KeepAlive(buffer)
	return buffer, adlStatus("ADL_Adapter_AdapterInfo_Get", status)
}

func (s *windowsADLSession) free(pointer uintptr) error {
	if pointer == 0 {
		return nil
	}
	ok, _, callErr := s.heap.free.Call(s.heap.heap, 0, pointer)
	return nativeBOOLResult("HeapFree(ADL输出数组)", ok, callErr)
}

func (s *windowsADLSession) displays(adapter int32) (result []adlDisplayInfo, err error) {
	var count int32
	// 传入的输出指针必须先清零。即使ADL返回错误，也先登记其非空分配再清理；
	// 数组复制到Go后立即释放，保证所有路径都是数组free→Control_Destroy→FreeLibrary。
	var pointer uintptr
	status, _, _ := s.displayInfo.Call(uintptr(adapter), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&pointer)), 0)
	return copyADLDisplays(count, unsafe.Pointer(pointer), adlStatus("ADL_Display_DisplayInfo_Get", status), func(address unsafe.Pointer) error { return s.free(uintptr(address)) })
}

func (s *windowsADLSession) edid(adapter, display int32) ([]byte, error) {
	var data adlEDIDData
	data.Size = int32(unsafe.Sizeof(data))
	// flag=0、blockIndex=0仅请求公开ABI的首个有界缓冲区；不猜测后续块索引的偏移。
	status, _, _ := s.edidData.Call(uintptr(adapter), uintptr(display), uintptr(unsafe.Pointer(&data)))
	if err := adlStatus("ADL_Display_EdidData_Get", status); err != nil {
		return nil, err
	}
	if data.Size != int32(unsafe.Sizeof(data)) || data.DataSize < 128 || data.DataSize > int32(len(data.Data)) || data.Block != 0 || data.Flag != 0 {
		return nil, lgUnsupported("ADL首块EDID长度或返回标记无法可靠解释")
	}
	return append([]byte(nil), data.Data[:data.DataSize]...), nil
}

func (s *windowsADLSession) writeLG(adapter, display int32, packet [8]byte) error {
	// 再核对固定包格式，防止今后内部调用把该私有入口变成任意raw I²C发送器。
	expected, err := lgInputPacket(uint16(packet[5])<<8 | uint16(packet[6]))
	if err != nil || packet != expected {
		return lgUnsupported("AMD入口仅接受固定LG输入报文")
	}
	var receiveLength int32
	status, _, _ := s.ddc.Call(uintptr(adapter), uintptr(display), 0, 0, uintptr(len(packet)),
		uintptr(unsafe.Pointer(&packet[0])), uintptr(unsafe.Pointer(&receiveLength)), 0)
	runtime.KeepAlive(packet)
	return adlStatus("ADL_Display_DDCBlockAccess_Get(LG输入提交)", status)
}

func (s *windowsADLSession) close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.createAttempted {
		status, _, _ := s.destroy.Call()
		err = adlStatus("ADL_Main_Control_Destroy", status)
	}
	// 销毁失败也继续卸载DLL，但保留两阶段错误；同步驱动已返回才会到达这里。
	return errors.Join(err, nativeError("FreeLibrary(atiadlxx.dll)", s.dll.Release()))
}
