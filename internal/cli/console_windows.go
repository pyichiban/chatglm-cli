//go:build windows

package cli

import (
	"syscall"
)

// InitConsole 把 Windows 控制台输出代码页切到 UTF-8 (65001)。
//
// 中文 Windows 默认 GBK：Go 直接写 UTF-8 字节在控制台会显示乱码。这里等价于先执行 `chcp 65001`，
// 省掉用户手工设置。输出重定向到文件/管道时本来就是字节透传，不受影响。
func InitConsole() {
	k := syscall.NewLazyDLL("kernel32.dll")
	p := k.NewProc("SetConsoleOutputCP")
	if p.Find() != nil {
		return // 非 Windows 控制台（或找不到导出）直接跳过
	}
	_, _, _ = p.Call(65001)
}
