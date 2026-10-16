//go:build !windows

package cli

// InitConsole 非 Windows 平台无需处理控制台代码页（终端按 UTF-8 解释字节）。
func InitConsole() {}
