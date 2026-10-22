package runs

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// Status is the state of a run as observed from outside its process.
type Status string

// Run statuses. Succeeded, failed, and stopped are also the statuses an outcome records.
const (
	StatusStarting  Status = "starting"  // a launcher holds the launch lock; the run holds no lock yet
	StatusRunning   Status = "running"   // the run process holds its lifetime lock
	StatusSucceeded Status = "succeeded" // the current attempt recorded a successful outcome
	StatusFailed    Status = "failed"    // the current attempt recorded a failed outcome
	StatusStopped   Status = "stopped"   // the current attempt was stopped, with or without an outcome
	StatusCrashed   Status = "crashed"   // the run ended without an outcome or a stop request
)

// Live reports whether the run is starting or running, the two statuses in which another
// launch, a merge of its branch, or a prune must leave it alone.
func (s Status) Live() bool {
	return s == StatusStarting || s == StatusRunning
}

// Classify returns the status of the run in directory dir under checkout root:
//   - running when its lifetime lock is held;
//   - the outcome's status when the lock is free and outcome.json belongs to the descriptor's
//     current attempt (an outcome left by an earlier attempt does not count);
//   - stopped when there is no such outcome and the stop marker is present;
//   - starting when there is no such outcome and the launch lock of its plan and branch is held;
//   - crashed otherwise.
//
// Classify is the observers' query. A launcher that holds the launch lock of a plan and branch
// must use LatestFor instead: a probe from the holder's own process reads that lock as held.
//
// A missing or corrupt descriptor, a corrupt outcome, and a failed lock probe are errors.
func Classify(root, dir string) (Status, error) {
	desc, err := LoadDescriptor(dir)
	if err != nil {
		return "", fmt.Errorf("classify run %s: %w", filepath.Base(dir), err)
	}
	return classify(root, dir, desc, classifyOptions{})
}

// classifyOptions adjusts classify for locks the caller holds itself.
type classifyOptions struct {
	// ownLaunchLock is the path of a launch lock the caller holds, or empty. flock locks belong
	// to an open file description, so a probe from the holder's own process conflicts with its
	// lock and reads it as held. A run keyed to this lock skips the launch-lock tier instead of
	// reading as starting; no other launch of its plan and branch can be in progress while the
	// caller holds the lock.
	ownLaunchLock string
}

// probeLock is Probe; tests replace it to change lock state between classify's probes.
var probeLock = Probe

// classify is Classify for a run whose descriptor is already loaded.
//
// The launch lock is probed before the lifetime lock although its tier ranks lower. A launcher
// releases the launch lock only after its child holds run.lock, so a run that starts between
// the two probes is seen by one of them. Probing run.lock first can find it still free, then
// find the launch lock already released, and report a live run as crashed.
func classify(root, dir string, desc Descriptor, opts classifyOptions) (Status, error) {
	launching := false
	if launchLock := launchLockPath(root, desc.Plan, desc.Branch); launchLock != opts.ownLaunchLock {
		held, err := probeLock(launchLock)
		if err != nil {
			return "", fmt.Errorf("classify run %s: probe launch lock: %w", desc.ID, err)
		}
		launching = held
	}

	held, err := probeLock(filepath.Join(dir, LockFile))
	if err != nil {
		return "", fmt.Errorf("classify run %s: %w", desc.ID, err)
	}
	if held {
		return StatusRunning, nil
	}

	outcome, err := LoadOutcome(dir)
	switch {
	case err == nil && outcome.Attempt == desc.Attempt:
		return outcome.Status, nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("classify run %s: %w", desc.ID, err)
	}

	stop, err := StopRequested(dir)
	if err != nil {
		return "", fmt.Errorf("classify run %s: %w", desc.ID, err)
	}
	if stop {
		return StatusStopped, nil
	}

	if launching {
		return StatusStarting, nil
	}
	return StatusCrashed, nil
}
