package host

import (
	"fmt"
	"syscall"
	"unsafe"
)

func platformLanguages() ([]string, error) {
	// kernel32是Windows系统DLL；直接调用系统接口，不执行命令或搜索外部工具。
	procedure := syscall.NewLazyDLL("kernel32.dll").NewProc("GetUserPreferredUILanguages")
	if err := procedure.Find(); err != nil {
		return nil, fmt.Errorf("定位GetUserPreferredUILanguages失败：%w", err)
	}
	const languageNames = 0x8 // MUI_LANGUAGE_NAME，返回语言名而非数值语言ID。
	var count, size uint32
	result, _, callErr := procedure.Call(languageNames, uintptr(unsafe.Pointer(&count)), 0, uintptr(unsafe.Pointer(&size)))
	if result == 0 {
		return nil, fmt.Errorf("读取Windows首选语言缓冲大小失败：%v", callErr)
	}
	if size < 2 || size > 1024*1024 {
		return nil, fmt.Errorf("Windows首选语言缓冲大小无效：%d", size)
	}
	buffer := make([]uint16, int(size))
	result, _, callErr = procedure.Call(languageNames, uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)))
	if result == 0 {
		return nil, fmt.Errorf("读取Windows首选语言失败：%v", callErr)
	}
	if size > uint32(len(buffer)) {
		return nil, fmt.Errorf("Windows首选语言缓冲在读取期间发生变化：%d", size)
	}
	return windowsLanguageList(buffer[:size], count)
}
