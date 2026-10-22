package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var (
	// ErrUnsupported is returned by the lock and process identity helpers on platforms where
	// detached runs are not supported.
	ErrUnsupported = errors.New("detached runs are not supported on this platform")
	// ErrBusy is returned by AcquireBounded when another process still holds the lock at the
	// deadline.
	ErrBusy = errors.New("lock is held by another process")
	// ErrLaunchBusy is returned by AcquireLaunchLock when another launch of the same plan and
	// branch holds the launch lock.
	ErrLaunchBusy = errors.New("another launch of this plan is in progress")
)

// launchLockKeyLen is the number of hex characters of the launch lock key.
const launchLockKeyLen = 16

// AcquireLaunchLock takes the launch lock of planRel on branch under root, waiting at most
// timeout, and returns its release function. The lock serializes launchers of one plan and
// branch while they decide and start a run; Classify reports a run without a lifetime lock or
// outcome as starting while its launch lock is held. planRel names the plan as the descriptor's
// Plan records it, relative to the checkout; spellings that clean to the same path, such as
// ./docs/plans/x.md and docs/plans/x.md, share one lock. A lock still held at the deadline is
// reported as ErrLaunchBusy.
func AcquireLaunchLock(root, planRel, branch string, timeout time.Duration) (func() error, error) {
	if err := os.MkdirAll(RunsDir(root), 0o750); err != nil {
		return nil, fmt.Errorf("create runs directory: %w", err)
	}
	release, err := AcquireBounded(launchLockPath(root, planRel, branch), timeout)
	if errors.Is(err, ErrBusy) {
		return nil, fmt.Errorf("%w: %s on branch %s", ErrLaunchBusy, planRel, branch)
	}
	if err != nil {
		return nil, fmt.Errorf("acquire launch lock: %w", err)
	}
	return release, nil
}

// LaunchInProgress reports whether a launcher holds the launch lock of planRel on branch under
// root. It probes without waiting and never creates the lock file.
func LaunchInProgress(root, planRel, branch string) (bool, error) {
	held, err := Probe(launchLockPath(root, planRel, branch))
	if err != nil {
		return false, fmt.Errorf("probe launch lock: %w", err)
	}
	return held, nil
}

// launchLockPath returns <runs dir>/.launch-<first 16 hex of sha256(planRel + "\x00" + branch)>.lock,
// hashing planRel in its planKey form so every spelling of one plan maps to the same lock. The
// name starts with a dot and is not a valid run id, so List never mistakes it for a run.
func launchLockPath(root, planRel, branch string) string {
	sum := sha256.Sum256([]byte(planKey(planRel) + "\x00" + branch))
	key := hex.EncodeToString(sum[:])[:launchLockKeyLen]
	return filepath.Join(RunsDir(root), ".launch-"+key+".lock")
}

// planKey is the normalized form of a checkout-relative plan path that launch locks hash and
// LatestFor compares: cleaned, with forward slashes. Two spellings of one plan, such as
// ./docs/plans/x.md and docs/plans/x.md, have the same key.
func planKey(planRel string) string {
	return filepath.ToSlash(filepath.Clean(planRel))
}
