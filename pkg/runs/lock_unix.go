//go:build !windows

package runs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// lockPollInterval is how often AcquireBounded retries a busy lock.
const lockPollInterval = 25 * time.Millisecond

// AcquireBounded takes an exclusive flock on path, creating the file with mode 0600 when it is
// missing, and returns the function that releases it. It retries LOCK_EX|LOCK_NB every 25 ms
// until timeout, so the momentary hold of an observer's Probe never makes the caller fail; a
// lock still held at the deadline is reported as ErrBusy. The descriptor is opened by
// os.OpenFile and therefore carries O_CLOEXEC: processes the holder starts never inherit the
// lock. The OS releases it when the holder exits, so a holder may also keep it for its whole
// life without calling release.
func AcquireBounded(path string, timeout time.Duration) (func() error, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // lock path under the runs directory
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		acquired, err := tryFlock(f, syscall.LOCK_EX)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if acquired {
			return func() error { return releaseFlock(f) }, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ErrBusy)
		}
		time.Sleep(min(lockPollInterval, remaining))
	}
}

// Probe reports whether another process holds the lock at path. It takes and immediately
// releases a shared non-blocking flock, so concurrent probes never see each other as holders
// and only an exclusive holder reads as held. A missing lock file is not held; Probe never
// creates one.
func Probe(path string) (held bool, err error) {
	f, err := os.Open(path) //nolint:gosec // lock path under the runs directory
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open lock %s: %w", path, err)
	}
	defer f.Close()
	acquired, err := tryFlock(f, syscall.LOCK_SH)
	if err != nil {
		return false, fmt.Errorf("probe lock %s: %w", path, err)
	}
	if !acquired {
		return true, nil
	}
	if err := flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return false, fmt.Errorf("release probe of lock %s: %w", path, err)
	}
	return false, nil
}

// tryFlock attempts a non-blocking flock of kind on f. A lock held elsewhere reports false with
// no error. An attempt a signal interrupted is retried by flock rather than reported as not
// acquired, which would make Probe read a free lock as held.
func tryFlock(f *os.File, kind int) (bool, error) {
	err := flock(int(f.Fd()), kind|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
		return false, nil
	default:
		return false, fmt.Errorf("flock: %w", err)
	}
}

// flockSyscall is syscall.Flock; tests replace it to interrupt calls.
var flockSyscall = syscall.Flock

// flock applies how to fd, repeating a call a signal interrupted (EINTR).
func flock(fd, how int) error {
	for {
		if err := flockSyscall(fd, how); !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// releaseFlock unlocks and closes a lock file taken by AcquireBounded.
func releaseFlock(f *os.File) error {
	unlockErr := flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock %s: %w", f.Name(), unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock %s: %w", f.Name(), closeErr)
	}
	return nil
}
