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
// A missing or corrupt descriptor, a corrupt outcome, and a failed lock probe are errors.
func Classify(root, dir string) (Status, error) {
	desc, err := LoadDescriptor(dir)
	if err != nil {
		return "", fmt.Errorf("classify run %s: %w", filepath.Base(dir), err)
	}
	return classify(root, dir, desc)
}

// classify is Classify for a run whose descriptor is already loaded.
func classify(root, dir string, desc Descriptor) (Status, error) {
	held, err := Probe(filepath.Join(dir, LockFile))
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

	launching, err := LaunchInProgress(root, desc.Plan, desc.Branch)
	if err != nil {
		return "", fmt.Errorf("classify run %s: %w", desc.ID, err)
	}
	if launching {
		return StatusStarting, nil
	}
	return StatusCrashed, nil
}
