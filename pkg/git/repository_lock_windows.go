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

// Keep only the worktree run-lock byte beyond its diagnostic header. The long-standing repository
// creation lock must remain on byte zero so different loopai versions continue to exclude each
// other. Windows denies reads that overlap an exclusive byte-range lock, hence the separate range.
const worktreeRunLockByteOffset = uint32(1 << 20)

func lockOverlapped(offset uint32) *windows.Overlapped {
	return &windows.Overlapped{Offset: offset}
}

func tryLockRepositoryFile(f *os.File) (bool, error) {
	return tryLockFileByte(f, 0)
}

func tryLockWorktreeRunFile(f *os.File) (bool, error) {
	return tryLockFileByte(f, worktreeRunLockByteOffset)
}

func tryLockFileByte(f *os.File, offset uint32) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, lockOverlapped(offset))
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
	return unlockFileByte(f, 0)
}

func unlockWorktreeRunFile(f *os.File) error {
	return unlockFileByte(f, worktreeRunLockByteOffset)
}

func unlockFileByte(f *os.File, offset uint32) error {
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockOverlapped(offset)); err != nil {
		return fmt.Errorf("UnlockFileEx: %w", err)
	}
	return nil
}
