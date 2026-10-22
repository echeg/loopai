package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/umputun/ralphex/pkg/git"
)

// detachedSourceState is a detached run's source-plans.json: the invoking checkout's plan as the
// run's first attempt found it. Only a plan with uncommitted changes is recorded, as for a chain.
type detachedSourceState struct {
	SourcePlans []git.PlanSourceState `json:"source_plans"`
}

// captureDetachedSourcePlan records the invoking checkout's copy of planFile at statePath, so a
// successful detached run can make that checkout merge-ready the way a successful worktree chain
// does. An existing state file is kept: a resume reuses the first attempt's capture, since the
// copy it would see now may be one the user has edited since.
func captureDetachedSourcePlan(svc *git.Service, planFile, statePath string) error {
	_, statErr := os.Lstat(statePath)
	if statErr == nil {
		return nil
	}
	if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("inspect source plan state: %w", statErr)
	}
	states, err := svc.CapturePlanChainSourceState([]string{planFile})
	if err != nil {
		return fmt.Errorf("capture source plan: %w", err)
	}
	return saveJSONState(statePath, "source plan state", detachedSourceState{SourcePlans: states})
}

// reconcileDetachedSourcePlan removes an unchanged untracked source plan and restores an unchanged
// tracked one to HEAD, never touching a copy edited since capture. Its errors are warnings for the
// caller to report: the run has already succeeded. A missing state file has nothing to reconcile.
func reconcileDetachedSourcePlan(svc *git.Service, statePath string) error {
	data, err := os.ReadFile(statePath) //nolint:gosec // run-owned state path under .loopai/progress
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read source plan state: %w", err)
	}
	var state detachedSourceState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse source plan state: %w", err)
	}
	if err := svc.ReconcilePlanChainSourceState(state.SourcePlans); err != nil {
		return fmt.Errorf("reconcile source plan: %w", err)
	}
	return nil
}
