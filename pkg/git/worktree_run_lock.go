package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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
		return fmt.Sprintf("worktree run lock %s is busy (%s)", e.Path, info)
	}
	return fmt.Sprintf("worktree run lock %s is busy", e.Path)
}

// Info returns the validated diagnostic metadata recorded by the lock holder.
func (e *ErrWorktreeBusy) Info() string {
	if e.PID <= 0 || e.Started.IsZero() {
		return ""
	}
	return fmt.Sprintf("pid=%d started=%s", e.PID, e.Started.Format(time.RFC3339))
}

// AcquireWorktreeRunLock acquires the non-blocking liveness lock for wtPath.
// The returned function releases the lock and closes its file descriptor.
func AcquireWorktreeRunLock(wtPath string) (func() error, error) {
	lockFile, lockPath, err := openWorktreeRunLock(wtPath)
	if err != nil {
		return nil, err
	}

	acquired, err := tryLockRepositoryFile(lockFile)
	if err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("acquire worktree run lock: %w", err)
	}
	if !acquired {
		busyErr := readWorktreeBusyError(lockFile, lockPath)
		_ = lockFile.Close()
		return nil, busyErr
	}

	metadata := fmt.Sprintf("pid=%d started=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := replaceLockFileContents(lockFile, metadata); err != nil {
		_ = unlockRepositoryFile(lockFile)
		_ = lockFile.Close()
		return nil, fmt.Errorf("write worktree run lock: %w", err)
	}

	return func() error {
		unlockErr := unlockRepositoryFile(lockFile)
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

// ProbeWorktreeRunLock reports whether wtPath's run lock is currently held.
// When busy, info contains validated pid and start-time diagnostics when available.
func ProbeWorktreeRunLock(wtPath string) (busy bool, info string, err error) {
	lockFile, lockPath, err := openWorktreeRunLock(wtPath)
	if err != nil {
		return false, "", err
	}

	acquired, err := tryLockRepositoryFile(lockFile)
	if err != nil {
		_ = lockFile.Close()
		return false, "", fmt.Errorf("probe worktree run lock: %w", err)
	}
	if !acquired {
		busyErr := readWorktreeBusyError(lockFile, lockPath)
		if closeErr := lockFile.Close(); closeErr != nil {
			return false, "", fmt.Errorf("close worktree run lock after probe: %w", closeErr)
		}
		return true, busyErr.Info(), nil
	}

	unlockErr := unlockRepositoryFile(lockFile)
	closeErr := lockFile.Close()
	if unlockErr != nil {
		return false, "", fmt.Errorf("release worktree run lock after probe: %w", unlockErr)
	}
	if closeErr != nil {
		return false, "", fmt.Errorf("close worktree run lock after probe: %w", closeErr)
	}
	return false, "", nil
}

func openWorktreeRunLock(wtPath string) (*os.File, string, error) {
	gitDir, err := worktreeGitDir(wtPath)
	if err != nil {
		return nil, "", err
	}
	lockPath := filepath.Join(gitDir, worktreeRunLockName)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // gitDir is resolved by Git
	if err != nil {
		return nil, "", fmt.Errorf("open worktree run lock: %w", err)
	}
	return lockFile, lockPath, nil
}

func worktreeGitDir(wtPath string) (string, error) {
	absPath, err := filepath.Abs(wtPath)
	if err != nil {
		return "", fmt.Errorf("resolve worktree path: %w", err)
	}
	cmd := exec.CommandContext(context.Background(), "git", "rev-parse", "--git-dir")
	cmd.Dir = absPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		if detail := strings.TrimSpace(string(out)); detail != "" {
			return "", fmt.Errorf("locate worktree Git directory: %s", detail)
		}
		return "", fmt.Errorf("locate worktree Git directory: %w", err)
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(absPath, gitDir)
	}
	return filepath.Clean(gitDir), nil
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
	if err := lockFile.Sync(); err != nil {
		return fmt.Errorf("sync lock file: %w", err)
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
