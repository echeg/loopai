//go:build windows

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func tryLockRepositoryFile(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, fmt.Errorf("LockFileEx: %w", err)
}

func lockRepositoryFile(ctx context.Context, f *os.File) error {
	for {
		acquired, err := tryLockRepositoryFile(f)
		if err != nil {
			return err
		}
		if acquired {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for repository lock: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func unlockRepositoryFile(f *os.File) error {
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped)); err != nil {
		return fmt.Errorf("UnlockFileEx: %w", err)
	}
	return nil
}
