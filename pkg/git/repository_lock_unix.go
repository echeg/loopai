//go:build !windows

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func tryLockRepositoryFile(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, fmt.Errorf("flock: %w", err)
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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("flock unlock: %w", err)
	}
	return nil
}
