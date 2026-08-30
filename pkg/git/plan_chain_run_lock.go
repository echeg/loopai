package git

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// ErrPlanChainBusy reports that another process owns the same plan-chain lifecycle.
// The operating-system advisory lock is authoritative; the lock file is durable metadata
// and must not be removed while a run may be active.
type ErrPlanChainBusy struct { //nolint:errname // public name mirrors ErrWorktreeBusy
	Identity string
}

func (e *ErrPlanChainBusy) Error() string {
	return "plan chain is busy: loopai is already running this chain"
}

// AcquirePlanChainRunLock acquires a non-blocking lock in shared Git metadata for one chain
// identity. The caller must hold the returned lock across checkpoint recovery, every member, and
// final checkpoint removal so another invocation cannot mistake live state for crash residue.
func (s *Service) AcquirePlanChainRunLock(identity string) (func() error, error) {
	commonDir, err := s.repo.gitCommonDir()
	if err != nil {
		return nil, fmt.Errorf("locate plan chain lock directory: %w", err)
	}
	digest := sha256.Sum256([]byte(identity))
	name := "loopai-plan-chain-" + hex.EncodeToString(digest[:12]) + ".lock"
	lockFile, err := os.OpenFile(filepath.Join(commonDir, name), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // commonDir is resolved by Git
	if err != nil {
		return nil, fmt.Errorf("open plan chain lock: %w", err)
	}
	acquired, err := tryLockRepositoryFile(lockFile)
	if err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("acquire plan chain lock: %w", err)
	}
	if !acquired {
		_ = lockFile.Close()
		return nil, &ErrPlanChainBusy{Identity: identity}
	}
	return func() error {
		unlockErr := unlockRepositoryFile(lockFile)
		closeErr := lockFile.Close()
		if unlockErr != nil {
			return fmt.Errorf("release plan chain lock: %w", unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close plan chain lock: %w", closeErr)
		}
		return nil
	}, nil
}
