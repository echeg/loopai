//go:build windows

package git

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func lockRepositoryFile(f *os.File) error {
	for {
		err := windows.LockFileEx(windows.Handle(f.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0, 1, 0, new(windows.Overlapped))
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return fmt.Errorf("LockFileEx: %w", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func unlockRepositoryFile(f *os.File) error {
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped)); err != nil {
		return fmt.Errorf("UnlockFileEx: %w", err)
	}
	return nil
}
