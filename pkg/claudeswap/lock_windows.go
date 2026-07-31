//go:build windows

package claudeswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func acquireFileLock(ctx context.Context, f *os.File, poll time.Duration) error {
	overlapped := new(windows.Overlapped)
	for {
		err := windows.LockFileEx(
			windows.Handle(f.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0, 1, 0, overlapped,
		)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return fmt.Errorf("LockFileEx: %w", err)
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("lock context: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func releaseFileLock(f *os.File) error {
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped)); err != nil {
		return fmt.Errorf("UnlockFileEx: %w", err)
	}
	return nil
}
