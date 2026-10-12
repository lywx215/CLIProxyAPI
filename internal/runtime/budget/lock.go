package budget

import (
	"context"
	"errors"
	"os"
	"time"
)

func acquireLock(ctx context.Context, path string, timeout time.Duration) (*os.File, error) {
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	lockCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		if err = lockFile(file); err == nil {
			return file, nil
		}
		if !lockBusy(err) {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-lockCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, errors.Join(admissionError("budget_lock_unavailable", 503, 0), lockCtx.Err())
		case <-timer.C:
		}
	}
}
