package infrastructure

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

func nativeArchitecture() string {
	procedure := syscall.NewLazyDLL("kernel32.dll").NewProc("IsWow64Process2")
	// 区分 Windows 原生架构与模拟运行的程序架构。
	if procedure.Find() == nil {
		var processMachine, nativeMachine uint16
		ok, _, _ := procedure.Call(^uintptr(0), uintptr(unsafe.Pointer(&processMachine)), uintptr(unsafe.Pointer(&nativeMachine)))
		if ok != 0 {
			switch nativeMachine {
			case 0x8664:
				return "amd64"
			case 0xaa64:
				return "arm64"
			case 0x014c:
				return "386"
			}
		}
	}
	// 兼容不提供 IsWow64Process2 的旧系统。
	for _, name := range []string{"PROCESSOR_ARCHITEW6432", "PROCESSOR_ARCHITECTURE"} {
		if value := os.Getenv(name); value != "" {
			return normalizeArchitecture(value)
		}
	}
	return runtime.GOARCH
}
