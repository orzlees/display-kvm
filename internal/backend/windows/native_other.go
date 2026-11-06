//go:build !windows

package windows

// 非Windows宿主仍保留可注入算法测试，不加载或宣称存在Windows原生传输。
func newPlatformTransport() Transport { return nil }
