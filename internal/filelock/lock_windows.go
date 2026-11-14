//go:build windows

// Package filelock provides process-scoped advisory file ownership.
package filelock

import (
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// Try exclusively locks the first byte until release or process termination.
// The lock file remains as a stable inode; callers must not unlink it.
func Try(path string) (*os.File, func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	overlapped := &windows.Overlapped{}
	if err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	var once sync.Once
	return file, func() {
		once.Do(func() { _ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped); _ = file.Close() })
	}, nil
}
