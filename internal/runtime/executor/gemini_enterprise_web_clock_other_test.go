//go:build !windows

package executor

import "time"

func mixedPerformanceNow() int64 { return time.Now().UnixNano() }
