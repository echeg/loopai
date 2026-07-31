//go:build !windows

package claudeswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func acquireFileLock(ctx context.Context, f *os.File, poll time.Duration) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("flock: %w", err)
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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("flock unlock: %w", err)
	}
	return nil
}
