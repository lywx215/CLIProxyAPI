//go:build windows

package executor

import (
	"syscall"
	"unsafe"
)

var mixedPerformanceCounter = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
var mixedPerformanceFrequency = func() int64 {
	var frequency int64
	syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceFrequency").Call(uintptr(unsafe.Pointer(&frequency)))
	return frequency
}()

// QPC avoids the coarse shared clock that can quantize Windows p95 samples to zero.
func mixedPerformanceNow() int64 {
	var counter int64
	mixedPerformanceCounter.Call(uintptr(unsafe.Pointer(&counter)))
	return (counter/mixedPerformanceFrequency)*1e9 + (counter%mixedPerformanceFrequency)*1e9/mixedPerformanceFrequency
}
