//go:build windows

package gemini

import (
	"syscall"
	"unsafe"
)

var performanceCounter = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
var performanceFrequency = func() int64 {
	var frequency int64
	syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceFrequency").Call(uintptr(unsafe.Pointer(&frequency)))
	return frequency
}()

// QPC avoids the coarse shared clock that can quantize Windows p95 samples to zero.
func performanceNow() int64 {
	var counter int64
	performanceCounter.Call(uintptr(unsafe.Pointer(&counter)))
	return (counter/performanceFrequency)*1e9 + (counter%performanceFrequency)*1e9/performanceFrequency
}
