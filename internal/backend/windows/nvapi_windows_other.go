//go:build windows && !amd64

package windows

// NVAPI的调用约定与结构布局仅核验Win64；其他架构不得因系统安装了DLL
// 就宣称可写。既有ADL也保持自身架构限制，不尝试模拟或跨架构加载驱动。
func (windowsNativeAPI) displayVendor(string) (uint32, error) {
	return 0, lgUnsupported("NVIDIA LG输入通路仅支持Windows amd64")
}

func (windowsNativeAPI) openNVAPI() (nvapiSession, error) {
	return nil, lgUnsupported("NVIDIA LG输入通路仅支持Windows amd64")
}
