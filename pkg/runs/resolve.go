package runs

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrNotFound is returned when no run matches an identifier, or the checkout has no runs.
var ErrNotFound = errors.New("no matching detached run")

// Run is one run directory with its descriptor and its status at the time it was listed.
type Run struct {
	Dir        string
	Descriptor Descriptor
	Status     Status
}

// List returns the runs recorded under root, newest first by created_at. Entries of the runs
// directory that are not run directories (lock files, names that are not valid ids, symlinks)
// are skipped, as is a run directory whose descriptor is not written yet. A corrupt descriptor
// or outcome, or a failed lock probe, is an error naming the run.
func List(root string) ([]Run, error) {
	return list(root, classifyOptions{})
}

// list is List with classification adjusted by opts.
func list(root string, opts classifyOptions) ([]Run, error) {
	runsDir := RunsDir(root)
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	result := make([]Run, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !ValidID(entry.Name()) {
			continue
		}
		dir := filepath.Join(runsDir, entry.Name())
		desc, err := LoadDescriptor(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}
		status, err := classify(root, dir, desc, opts)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}
		result = append(result, Run{Dir: dir, Descriptor: desc, Status: status})
	}
	slices.SortFunc(result, func(a, b Run) int {
		if c := b.Descriptor.CreatedAt.Compare(a.Descriptor.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.Descriptor.ID, a.Descriptor.ID)
	})
	return result, nil
}

// Resolve finds the run ident names under root: a run with that exact id first, then a run of
// a plan ident names (a path relative to the current directory or to root, or the plan's file
// name with or without its extension), then a run on branch ident. Within each match kind the
// newest run wins. No match is ErrNotFound.
func Resolve(root, ident string) (Run, error) {
	if ident == "" {
		return Run{}, fmt.Errorf("%w: empty run identifier", ErrNotFound)
	}
	all, err := List(root)
	if err != nil {
		return Run{}, err
	}
	matchers := []func(Descriptor) bool{
		func(d Descriptor) bool { return d.ID == ident },
		func(d Descriptor) bool { return planMatches(root, d.Plan, ident) },
		func(d Descriptor) bool { return d.Branch == ident },
	}
	for _, match := range matchers {
		for _, run := range all {
			if match(run.Descriptor) {
				return run, nil
			}
		}
	}
	return Run{}, fmt.Errorf("%w: %s", ErrNotFound, ident)
}

// DefaultRun returns the run a command without an identifier means: the single live run under
// root, else the newest run. A checkout without runs is ErrNotFound.
func DefaultRun(root string) (Run, error) {
	all, err := List(root)
	if err != nil {
		return Run{}, err
	}
	if len(all) == 0 {
		return Run{}, ErrNotFound
	}
	var live []Run
	for _, run := range all {
		if run.Status.Live() {
			live = append(live, run)
		}
	}
	if len(live) == 1 {
		return live[0], nil
	}
	return all[0], nil
}

// LatestFor returns the newest run of plan planRel on branch under root, and false when there
// is none. Plans are compared in their planKey form, the one launch locks hash.
//
// LatestFor is the launcher's query: the caller must hold AcquireLaunchLock(root, planRel,
// branch) while it calls LatestFor and acts on the result. Its classification therefore skips
// the launch-lock tier for that plan and branch. A probe from the holder's own process reads its
// own launch lock as held, which would report a crashed run as starting, a live status, so it
// could never be resumed. While the caller holds the lock no other launch of the plan and
// branch can be in progress, so the run returned is running, finished, stopped, or crashed,
// never starting. Called without the lock, LatestFor would report another launcher's starting
// run as crashed; observers use Classify, List, or LiveRunForBranch instead.
func LatestFor(root, planRel, branch string) (Run, bool, error) {
	all, err := list(root, classifyOptions{ownLaunchLock: launchLockPath(root, planRel, branch)})
	if err != nil {
		return Run{}, false, err
	}
	key := planKey(planRel)
	for _, run := range all {
		if run.Descriptor.Branch == branch && planKey(run.Descriptor.Plan) == key {
			return run, true, nil
		}
	}
	return Run{}, false, nil
}

// LiveRunForBranch returns the newest starting or running run on branch under root, and false
// when there is none.
func LiveRunForBranch(root, branch string) (Run, bool, error) {
	all, err := List(root)
	if err != nil {
		return Run{}, false, err
	}
	for _, run := range all {
		if run.Descriptor.Branch == branch && run.Status.Live() {
			return run, true, nil
		}
	}
	return Run{}, false, nil
}

// PruneSucceeded deletes the directories of succeeded runs under root beyond the newest keep.
// Only runs classified as succeeded are touched, which requires their lifetime lock to be free;
// live, failed, stopped, and crashed runs are never removed.
func PruneSucceeded(root string, keep int) error {
	all, err := List(root)
	if err != nil {
		return fmt.Errorf("prune runs: %w", err)
	}
	var errs []error
	kept := 0
	for _, run := range all {
		if run.Status != StatusSucceeded {
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		if err := os.RemoveAll(run.Dir); err != nil {
			errs = append(errs, fmt.Errorf("prune run %s: %w", run.Descriptor.ID, err))
		}
	}
	return errors.Join(errs...)
}

// planMatches reports whether ident names plan, which is recorded relative to root: as the
// same path relative to the current directory or to root, or as the plan's file name with or
// without its extension.
func planMatches(root, plan, ident string) bool {
	if plan == "" {
		return false
	}
	base := filepath.Base(plan)
	if ident == base || ident == strings.TrimSuffix(base, filepath.Ext(base)) {
		return true
	}
	planPath := plan
	if !filepath.IsAbs(planPath) {
		planPath = filepath.Join(absPath(root), plan)
	}
	planPath = filepath.Clean(planPath)
	if absPath(ident) == planPath {
		return true
	}
	return !filepath.IsAbs(ident) && filepath.Join(absPath(root), ident) == planPath
}
