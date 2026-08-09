package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The file is advisory infrastructure and must not be deleted while a run is active. Removing it
// does not unlock the holder's open descriptor, but another process could create a replacement
// inode at the same path and lock that independently.
const worktreeRunLockName = "loopai-run.lock"

// ErrWorktreeBusy reports that another process holds a worktree's run lock.
// PID and Started are diagnostic only; the operating-system lock is authoritative.
type ErrWorktreeBusy struct { //nolint:errname // public name is part of the worktree-lock API
	Path    string
	PID     int
	Started time.Time
}

func (e *ErrWorktreeBusy) Error() string {
	if info := e.Info(); info != "" {
		return fmt.Sprintf("plan worktree %s is busy: loopai is already running in it (%s)", e.Path, info)
	}
	return fmt.Sprintf("plan worktree %s is busy: loopai is already running in it (holder details unavailable)", e.Path)
}

// Info returns the validated diagnostic metadata recorded by the lock holder.
func (e *ErrWorktreeBusy) Info() string {
	if e.PID <= 0 || e.Started.IsZero() {
		return ""
	}
	return fmt.Sprintf("pid=%d started=%s", e.PID, e.Started.Format(time.RFC3339))
}

// AcquireWorktreeRunLock acquires the non-blocking liveness lock for this worktree.
// The returned function releases the lock and closes its file descriptor.
func (s *Service) AcquireWorktreeRunLock() (func() error, error) {
	return s.AcquireWorktreeRunLockContext(context.Background())
}

// AcquireWorktreeRunLockContext is AcquireWorktreeRunLock with cancellation support while
// resolving the worktree's private Git directory through the configured VCS command.
func (s *Service) AcquireWorktreeRunLockContext(ctx context.Context) (func() error, error) {
	lockFile, err := s.openWorktreeRunLock(ctx)
	if err != nil {
		return nil, err
	}
	return acquireWorktreeRunLockFile(lockFile, s.repo.root(), replaceLockFileContents)
}

func acquireWorktreeRunLockFile(
	lockFile *os.File, worktreePath string, writeContents func(*os.File, string) error,
) (func() error, error) {
	acquired, err := tryLockWorktreeRunFile(lockFile)
	if err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("acquire worktree run lock: %w", err)
	}
	if !acquired {
		busyErr := readWorktreeBusyError(lockFile, worktreePath)
		_ = lockFile.Close()
		return nil, busyErr
	}

	metadata := fmt.Sprintf("pid=%d started=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := writeContents(lockFile, metadata); err != nil {
		_ = unlockWorktreeRunFile(lockFile)
		_ = lockFile.Close()
		return nil, fmt.Errorf("write worktree run lock: %w", err)
	}

	return func() error {
		unlockErr := unlockWorktreeRunFile(lockFile)
		closeErr := lockFile.Close()
		if unlockErr != nil {
			return fmt.Errorf("release worktree run lock: %w", unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close worktree run lock: %w", closeErr)
		}
		return nil
	}, nil
}

func (s *Service) openWorktreeRunLock(ctx context.Context) (*os.File, error) {
	gitDir, err := s.repo.gitDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("locate worktree Git directory: %w", err)
	}
	lockPath := filepath.Join(gitDir, worktreeRunLockName)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // gitDir is resolved by Git
	if err != nil {
		return nil, fmt.Errorf("open worktree run lock: %w", err)
	}
	return lockFile, nil
}

func replaceLockFileContents(lockFile *os.File, contents string) error {
	if err := lockFile.Truncate(0); err != nil {
		return fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := lockFile.Seek(0, 0); err != nil {
		return fmt.Errorf("seek lock file: %w", err)
	}
	if _, err := lockFile.WriteString(contents); err != nil {
		return fmt.Errorf("write lock file: %w", err)
	}
	return nil
}

func readWorktreeBusyError(lockFile *os.File, lockPath string) *ErrWorktreeBusy {
	busyErr := &ErrWorktreeBusy{Path: lockPath}
	if _, err := lockFile.Seek(0, 0); err != nil {
		return busyErr
	}
	var contents bytes.Buffer
	if _, err := contents.ReadFrom(lockFile); err != nil {
		return busyErr
	}
	fields := strings.Fields(contents.String())
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "pid=") || !strings.HasPrefix(fields[1], "started=") {
		return busyErr
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(fields[0], "pid="))
	if err != nil || pid <= 0 {
		return busyErr
	}
	started, err := time.Parse(time.RFC3339, strings.TrimPrefix(fields[1], "started="))
	if err != nil {
		return busyErr
	}
	busyErr.PID = pid
	busyErr.Started = started
	return busyErr
}
