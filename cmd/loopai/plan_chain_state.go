package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/processor"
)

const planChainCheckpointVersion = 3

type planChainCheckpoint struct {
	Version              int                   `json:"version"`
	Mode                 string                `json:"mode"`
	Worktree             bool                  `json:"worktree"`
	Commit               bool                  `json:"commit"`
	MovePlanOnCompletion bool                  `json:"move_plan_on_completion"`
	Plans                []string              `json:"plans"`
	Completed            int                   `json:"completed"`
	Active               int                   `json:"active,omitempty"` // one-based member whose execution started but was not checkpointed
	ActiveStartTip       string                `json:"active_start_tip,omitempty"`
	ActivePrepared       bool                  `json:"active_prepared,omitempty"`
	ActiveFinalizing     bool                  `json:"active_finalizing,omitempty"`
	ResumePreparedTip    string                `json:"resume_prepared_tip,omitempty"`
	PreviousTip          string                `json:"previous_tip,omitempty"`
	SourcePlans          []git.PlanSourceState `json:"source_plans,omitempty"`
	SourceReconciled     bool                  `json:"source_reconciled,omitempty"`
}

func planChainModeFromOpts(o opts) string {
	if o.TasksOnly {
		return string(processor.ModeTasksOnly)
	}
	return string(processor.ModeFull)
}

func normalizePlanChainPaths(root string, plans []string) ([]string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve plan chain root: %w", err)
	}
	if resolvedRoot, resolveErr := filepath.EvalSymlinks(absRoot); resolveErr == nil {
		absRoot = resolvedRoot
	}
	normalized := make([]string, 0, len(plans))
	for _, planFile := range plans {
		path := planFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(absRoot, path)
		}
		path = filepath.Clean(path)
		if resolvedDir, resolveErr := filepath.EvalSymlinks(filepath.Dir(path)); resolveErr == nil {
			path = filepath.Join(resolvedDir, filepath.Base(path))
		}
		rel, relErr := filepath.Rel(absRoot, path)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			normalized = append(normalized, filepath.ToSlash(rel))
			continue
		}
		normalized = append(normalized, filepath.ToSlash(path))
	}
	return normalized, nil
}

func planChainCheckpointPath(root string, plans []string, mode string) (string, []string, error) {
	normalized, err := normalizePlanChainPaths(root, plans)
	if err != nil {
		return "", nil, err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(mode))
	for _, planFile := range normalized {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(planFile))
	}
	name := "plan-chain-" + hex.EncodeToString(h.Sum(nil)[:12]) + ".json"
	return filepath.Join(root, ".loopai", "progress", name), normalized, nil
}

func planChainRunLockIdentity(root string, plans []string) (string, error) {
	normalized, err := normalizePlanChainPaths(root, plans)
	if err != nil {
		return "", err
	}
	return strings.Join(normalized, "\x00"), nil
}

func loadPlanChainCheckpoint(root string, plans []string, mode string) (planChainCheckpoint, bool, error) {
	path, normalized, err := planChainCheckpointPath(root, plans, mode)
	if err != nil {
		return planChainCheckpoint{}, false, err
	}
	data, found, err := readPlanChainCheckpointFile(root, path)
	if err != nil || !found {
		return planChainCheckpoint{}, found, err
	}
	var state planChainCheckpoint
	if err := json.Unmarshal(data, &state); err != nil {
		return planChainCheckpoint{}, false, fmt.Errorf("parse plan chain checkpoint: %w", err)
	}
	if err := validatePlanChainCheckpoint(state, normalized, mode, len(plans)); err != nil {
		return planChainCheckpoint{}, false, err
	}
	return state, true, nil
}

func readPlanChainCheckpointFile(root, path string) ([]byte, bool, error) {
	for _, dir := range []string{filepath.Join(root, ".loopai"), filepath.Join(root, ".loopai", "progress")} {
		info, statErr := os.Lstat(dir)
		if os.IsNotExist(statErr) {
			return nil, false, nil
		}
		if statErr != nil {
			return nil, false, fmt.Errorf("inspect plan chain checkpoint directory: %w", statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("unsafe plan chain checkpoint directory %s", dir)
		}
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect plan chain checkpoint: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("plan chain checkpoint is not a regular file: %s", path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is fixed beneath validated runtime directories
	if err != nil {
		return nil, false, fmt.Errorf("read plan chain checkpoint: %w", err)
	}
	return data, true, nil
}

func validatePlanChainCheckpoint(state planChainCheckpoint, normalized []string, mode string, planCount int) error {
	if state.Version != planChainCheckpointVersion || state.Mode != mode || len(state.Plans) != len(normalized) {
		return errors.New("plan chain checkpoint does not match this invocation")
	}
	for i := range normalized {
		if state.Plans[i] != normalized[i] {
			return errors.New("plan chain checkpoint paths do not match this invocation")
		}
	}
	if state.Completed < 0 || state.Completed > planCount || (state.Completed > 0 && state.PreviousTip == "") {
		return errors.New("plan chain checkpoint has invalid completion state")
	}
	return validateActivePlanChainCheckpoint(state, planCount)
}

func validateActivePlanChainCheckpoint(state planChainCheckpoint, planCount int) error {
	if state.Active < 0 || state.Active > planCount || (state.Active != 0 && state.Active != state.Completed+1) {
		return errors.New("plan chain checkpoint has invalid active member")
	}
	if state.Active == 0 && (state.ActiveStartTip != "" || state.ActivePrepared) {
		return errors.New("plan chain checkpoint has active branch tip without an active member")
	}
	if state.ActivePrepared && state.ActiveStartTip == "" {
		return errors.New("plan chain checkpoint has prepared active member without a branch tip")
	}
	if state.ActiveFinalizing && !state.ActivePrepared {
		return errors.New("plan chain checkpoint has finalizing member that was not prepared")
	}
	return nil
}

func savePlanChainCheckpoint(root string, plans []string, state planChainCheckpoint) error {
	path, normalized, err := planChainCheckpointPath(root, plans, state.Mode)
	if err != nil {
		return err
	}
	state.Version = planChainCheckpointVersion
	state.Plans = normalized
	dir := filepath.Dir(path)
	if mkdirErr := os.MkdirAll(dir, 0o750); mkdirErr != nil {
		return fmt.Errorf("create plan chain checkpoint directory: %w", mkdirErr)
	}
	tmp, err := os.CreateTemp(dir, ".plan-chain-*.tmp")
	if err != nil {
		return fmt.Errorf("create plan chain checkpoint: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup after atomic replacement
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure plan chain checkpoint: %w", err)
	}
	encodeErr := json.NewEncoder(tmp).Encode(state)
	closeErr := tmp.Close()
	if encodeErr != nil {
		return fmt.Errorf("write plan chain checkpoint: %w", encodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close plan chain checkpoint: %w", closeErr)
	}
	if renameErr := os.Rename(tmpPath, path); renameErr != nil {
		return fmt.Errorf("replace plan chain checkpoint: %w", renameErr)
	}
	return nil
}

func removePlanChainCheckpoint(root string, plans []string, mode string) error {
	path, _, err := planChainCheckpointPath(root, plans, mode)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove completed plan chain checkpoint: %w", err)
	}
	return nil
}

func verifiedPlanChainCheckpoint(
	ctx context.Context, o opts, gitSvc *git.Service, worktree, movePlanOnCompletion bool,
) (planChainCheckpoint, bool, error) {
	state, found, err := loadPlanChainCheckpoint(gitSvc.Root(), o.PlanFiles, string(resolvePlanChainMode(o)))
	if err != nil || !found {
		return state, found, err
	}
	if state.Worktree != worktree {
		return planChainCheckpoint{}, false, errors.New("plan chain checkpoint was created with a different worktree setting")
	}
	if state.Commit != o.Commit {
		return planChainCheckpoint{}, false, errors.New("plan chain checkpoint was created with a different commit setting")
	}
	if state.MovePlanOnCompletion != movePlanOnCompletion {
		return planChainCheckpoint{}, false, errors.New("plan chain checkpoint was created with a different move-plan-on-completion setting")
	}
	if state.Active != 0 {
		state, err = recoverActivePlanChainMember(ctx, o, gitSvc, state, state.MovePlanOnCompletion)
		if err != nil {
			return planChainCheckpoint{}, false, err
		}
	}
	state, err = discardStaleResumePreparedTip(ctx, o, gitSvc, state)
	if err != nil {
		return planChainCheckpoint{}, false, err
	}
	if state.Completed == 0 {
		return state, true, nil
	}
	lastPlan := o.PlanFiles[state.Completed-1]
	branch := gitSvc.EffectiveBranchName(lastPlan, "")
	if !gitSvc.BranchExists(branch) {
		return planChainCheckpoint{}, false, fmt.Errorf("resume plan chain: completed branch %q no longer exists", branch)
	}
	tip, err := gitSvc.BranchHash(branch)
	if err != nil {
		return planChainCheckpoint{}, false, fmt.Errorf("resume plan chain: read completed branch %q tip: %w", branch, err)
	}
	if tip != state.PreviousTip {
		return planChainCheckpoint{}, false, fmt.Errorf("resume plan chain: completed branch %q tip changed from saved predecessor tip %s to %s", branch, state.PreviousTip, tip)
	}
	return state, true, nil
}

// recoverActivePlanChainMember closes the only non-atomic completion window after successful plan
// processing and before the coordinator advances its external checkpoint. The coordinator persists
// a finalization marker before optional archival. When archival is enabled, the archived plan is an
// additional completion guard so a stale completed copy cannot make an aborted member look successful.
func recoverActivePlanChainMember(
	ctx context.Context, o opts, gitSvc *git.Service, state planChainCheckpoint, movePlanOnCompletion bool,
) (planChainCheckpoint, error) {
	activeIndex := state.Active - 1
	planFile := o.PlanFiles[activeIndex]
	branch := gitSvc.EffectiveBranchName(planFile, "")
	if gitSvc.BranchExists(branch) {
		var err error
		state, err = recoverExistingActivePlanChainMember(
			ctx, gitSvc, state, planFile, branch, movePlanOnCompletion,
		)
		if err != nil {
			return planChainCheckpoint{}, err
		}
	}
	state.Active = 0
	state.ActiveStartTip = ""
	state.ActivePrepared = false
	state.ActiveFinalizing = false
	if err := savePlanChainCheckpoint(gitSvc.Root(), o.PlanFiles, state); err != nil {
		return planChainCheckpoint{}, err
	}
	return state, nil
}

func recoverExistingActivePlanChainMember(
	ctx context.Context, gitSvc *git.Service, state planChainCheckpoint,
	planFile, branch string, movePlanOnCompletion bool,
) (planChainCheckpoint, error) {
	tip, err := gitSvc.BranchHash(branch)
	if err != nil {
		return planChainCheckpoint{}, fmt.Errorf("recover active plan chain member: %w", err)
	}
	if state.ActiveStartTip != "" {
		containsStart, containsErr := gitSvc.BranchContainsRevisionContext(ctx, branch, state.ActiveStartTip)
		if containsErr != nil {
			return planChainCheckpoint{}, fmt.Errorf("recover active plan chain member: validate branch ancestry: %w", containsErr)
		}
		if !containsStart {
			return planChainCheckpoint{}, fmt.Errorf("recover active plan chain member: branch %q no longer contains saved start tip %s", branch, state.ActiveStartTip)
		}
	}
	completedAtTip, err := planChainMemberCompletedAtTip(gitSvc, tip, planFile, movePlanOnCompletion)
	if err != nil {
		return planChainCheckpoint{}, err
	}
	if state.ActiveFinalizing && completedAtTip && tip != state.ActiveStartTip {
		state.Completed = state.Active
		state.PreviousTip = tip
		state.ResumePreparedTip = ""
	} else if state.ActivePrepared {
		state.ResumePreparedTip = state.ActiveStartTip
	}
	return state, nil
}

func planChainMemberCompletedAtTip(
	gitSvc *git.Service, tip, planFile string, movePlanOnCompletion bool,
) (bool, error) {
	if !movePlanOnCompletion {
		return true, nil
	}
	archived, err := gitSvc.PlanArchivedAtRevision(tip, planFile)
	if err != nil {
		return false, fmt.Errorf("recover active plan chain member: %w", err)
	}
	return archived, nil
}

func discardStaleResumePreparedTip(
	ctx context.Context, o opts, gitSvc *git.Service, state planChainCheckpoint,
) (planChainCheckpoint, error) {
	if state.ResumePreparedTip == "" {
		return state, nil
	}
	valid, err := preparedResumeTipValid(ctx, o, gitSvc, state)
	if err != nil {
		return planChainCheckpoint{}, fmt.Errorf("validate prepared plan chain resume tip: %w", err)
	}
	if valid {
		return state, nil
	}
	state.ResumePreparedTip = ""
	if err := savePlanChainCheckpoint(gitSvc.Root(), o.PlanFiles, state); err != nil {
		return planChainCheckpoint{}, err
	}
	return state, nil
}

func preparedResumeTipValid(
	ctx context.Context, o opts, gitSvc *git.Service, state planChainCheckpoint,
) (bool, error) {
	if state.Completed >= len(o.PlanFiles) {
		return false, nil
	}
	branch := gitSvc.EffectiveBranchName(o.PlanFiles[state.Completed], "")
	if !gitSvc.BranchExists(branch) {
		return false, nil
	}
	exists, err := gitSvc.RevisionExistsContext(ctx, state.ResumePreparedTip)
	if err != nil {
		return false, fmt.Errorf("check prepared revision: %w", err)
	}
	if !exists {
		return false, nil
	}
	contains, err := gitSvc.BranchContainsRevisionContext(ctx, branch, state.ResumePreparedTip)
	if err != nil {
		return false, fmt.Errorf("check prepared branch ancestry: %w", err)
	}
	if !contains {
		return false, fmt.Errorf("branch %q no longer contains saved prepared tip %s", branch, state.ResumePreparedTip)
	}
	return true, nil
}

func resolvePlanChainMode(o opts) processor.Mode {
	if o.TasksOnly {
		return processor.ModeTasksOnly
	}
	return processor.ModeFull
}

func checkpointCompletedPrefix(root string, o opts) int {
	state, found, err := loadPlanChainCheckpoint(root, o.PlanFiles, planChainModeFromOpts(o))
	if err != nil || !found {
		return 0
	}
	if state.Active == state.Completed+1 {
		return state.Active
	}
	return state.Completed
}
