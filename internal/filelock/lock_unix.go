//go:build !windows

// Package filelock provides process-scoped advisory file ownership.
package filelock

import (
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Try exclusively locks the file until release or process termination.
// The lock file remains as a stable inode; callers must not unlink it.
func Try(path string) (*os.File, func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	var once sync.Once
	return file, func() { once.Do(func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }) }, nil
}
