//go:build !windows

package gemini

import "time"

func performanceNow() int64 { return time.Now().UnixNano() }
