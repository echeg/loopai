package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/umputun/ralphex/pkg/plan"
)

//go:generate moq -out mocks/logger.go -pkg mocks -skip-ensure -fmt goimports . Logger

// Logger provides logging for git operations output.
// Compatible with *color.Color and standard log.Logger.
// The return values from Printf are ignored by Service methods.
type Logger interface {
	Printf(format string, args ...any) (int, error)
}

// backend defines the low-level git operations interface.
type backend interface {
	root() string
	headHash() (string, error)
	hasCommits() (bool, error)
	currentBranch() (string, error)
	originURL() (string, error)
	originPushURLs() ([]string, error)
	getDefaultBranch() string
	validateBranchName(name string) error
	branchExists(name string) bool
	branchHash(name string) (string, error)
	revParse(ref string) (string, error)
	revisionExists(ctx context.Context, revision string) (bool, error)
	fileExistsAt(ref, path string) (bool, error)
	createBranch(name string) error
	checkoutBranch(name string) error
	mergeBranch(ctx context.Context, name, expectedHead string) error
	mergeRevision(ctx context.Context, revision, expectedHead string) error
	deleteBranch(name string) error
	push(ctx context.Context, branch string) error
	worktrees() ([]Worktree, error)
	diffFingerprint() (string, error)
	isDirty() (bool, error)
	isDirtyAll() (bool, error)
	fileHasChanges(path string) (bool, error)
	fileTracked(path string) (bool, error)
	fileStateFingerprint(path string) (string, error)
	hasChangesOtherThan(paths ...string) ([]string, error)
	gitCommonDir() (string, error)
	gitDir(ctx context.Context) (string, error)
	ensureRuntimeExcludes(patterns ...string) error
	add(path string) error
	moveFile(src, dst string) error
	commit(msg string) error
	commitFiles(msg string, paths ...string) error
	autoCommitAll(msg string) (bool, error)
	createInitialCommit(msg string) error
	diffStats(baseBranch, headRef string) (DiffStats, error)
	addWorktree(ctx context.Context, path, branch string, createBranch bool, startRef string) error
	removeWorktree(path string) error
	removeWorktreeSafe(path string) error
	pruneWorktrees() error
	isAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
	mergeWouldConflict(ctx context.Context, base, branch string) (bool, error)
	mergeWorkingTreeWouldConflict(ctx context.Context, base string) (bool, error)
	restoreFile(path string) error
}

// ErrMergeConflict identifies a merge that could not be completed because of conflicts.
// The repository is returned to its pre-merge state before this error is returned.
var ErrMergeConflict = errors.New("merge conflict")

// errMergeTreeUnsupported indicates that the installed Git does not support the
// merge-tree --write-tree form used for non-mutating conflict prediction.
var errMergeTreeUnsupported = errors.New("git merge-tree --write-tree unsupported")

// DiffStats holds statistics about changes between two commits.
type DiffStats struct {
	Files     int // number of files changed
	Additions int // lines added
	Deletions int // lines deleted
}

// Worktree describes one registered Git worktree and the local branch checked out there.
// Branch is empty for a detached worktree.
type Worktree struct {
	Path   string
	Branch string
}

// PlanSourceState records an initially dirty chain plan in the source checkout. Digest, GitState,
// and Mode protect concurrent content, index, tracking, and permission changes while a worktree
// chain runs; Tracked selects restore-to-HEAD versus removal.
type PlanSourceState struct {
	Path     string `json:"path"`
	Digest   string `json:"digest"`
	GitState string `json:"git_state"`
	Mode     uint32 `json:"mode"`
	Tracked  bool   `json:"tracked"`
}

// ErrNotSameRepository indicates that a path opened as a worktree belongs to another repository.
var ErrNotSameRepository = errors.New("repository does not share source Git metadata")

// Service provides git operations for loopai workflows.
// It is the single public API for the git package.
type Service struct {
	repo    backend
	log     Logger
	command string
	trailer string // optional trailer line appended to all commits
}

// NewService opens a git repository and returns a Service.
// path is the path to the repository (use "." for current directory).
// log is used for progress output during operations.
// vcsCmd optionally specifies the vcs command to use (default: "git").
func NewService(path string, log Logger, vcsCmd ...string) (*Service, error) {
	command := "git"
	if len(vcsCmd) > 0 && vcsCmd[0] != "" {
		command = vcsCmd[0]
	}
	b, err := newExternalBackend(path, command)
	if err != nil {
		return nil, err
	}
	return &Service{repo: b, log: log, command: command}, nil
}

// OpenWorktree opens another worktree from the same repository using the same Git command and logger.
func (s *Service) OpenWorktree(path string) (*Service, error) {
	opened, err := NewService(path, s.log, s.command)
	if err != nil {
		return nil, err
	}
	sourceCommonDir, err := s.repo.gitCommonDir()
	if err != nil {
		return nil, fmt.Errorf("locate source repository metadata: %w", err)
	}
	openedCommonDir, err := opened.repo.gitCommonDir()
	if err != nil {
		return nil, fmt.Errorf("locate opened repository metadata: %w", err)
	}
	if !sameGitDirectory(sourceCommonDir, openedCommonDir) {
		return nil, fmt.Errorf("open worktree %s: %w", path, ErrNotSameRepository)
	}
	opened.trailer = s.trailer
	return opened, nil
}

func sameGitDirectory(a, b string) bool {
	infoA, statErrA := os.Stat(a)
	infoB, statErrB := os.Stat(b)
	if statErrA == nil && statErrB == nil {
		return os.SameFile(infoA, infoB)
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		return filepath.Clean(resolvedA) == filepath.Clean(resolvedB)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// SetCommitTrailer sets an optional trailer line appended to all commit messages.
// when set, a blank line and the trailer are appended after the commit message.
func (s *Service) SetCommitTrailer(trailer string) {
	s.trailer = trailer
}

// appendTrailer appends the configured trailer to a commit message.
// returns the message unchanged when no trailer is configured.
func (s *Service) appendTrailer(msg string) string {
	if s.trailer == "" {
		return msg
	}
	return msg + "\n\n" + s.trailer
}

// AutoCommitAll stages all non-ignored changes and commits them with the given message.
// It returns false without creating a commit when the working tree is clean.
func (s *Service) AutoCommitAll(message string) (bool, error) {
	committed, err := s.repo.autoCommitAll(s.appendTrailer(message))
	if err != nil {
		return false, fmt.Errorf("auto-commit all: %w", err)
	}
	return committed, nil
}

// AcquireWorktreeCreationLock serializes source auto-commit and worktree creation across
// loopai processes using the repository's shared Git metadata directory.
func (s *Service) AcquireWorktreeCreationLock() (func() error, error) {
	return s.AcquireWorktreeCreationLockContext(context.Background())
}

// AcquireWorktreeCreationLockContext is AcquireWorktreeCreationLock with cancellation support.
func (s *Service) AcquireWorktreeCreationLockContext(ctx context.Context) (func() error, error) {
	commonDir, err := s.repo.gitCommonDir()
	if err != nil {
		return nil, fmt.Errorf("locate repository lock directory: %w", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(commonDir, "loopai-worktree-create.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // commonDir is resolved by Git
	if err != nil {
		return nil, fmt.Errorf("open worktree creation lock: %w", err)
	}
	if err := lockRepositoryFile(ctx, lockFile); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("acquire worktree creation lock: %w", err)
	}

	return func() error {
		unlockErr := unlockRepositoryFile(lockFile)
		closeErr := lockFile.Close()
		if unlockErr != nil {
			return fmt.Errorf("release worktree creation lock: %w", unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close worktree creation lock: %w", closeErr)
		}
		return nil
	}, nil
}

// Root returns the absolute path to the repository root.
func (s *Service) Root() string {
	return s.repo.root()
}

// HeadHash returns the current HEAD commit hash as a hex string.
func (s *Service) HeadHash() (string, error) {
	return s.repo.headHash()
}

// ContainsRevisionContext reports whether revision is an ancestor of this checkout's HEAD.
func (s *Service) ContainsRevisionContext(ctx context.Context, revision string) (bool, error) {
	contains, err := s.repo.isAncestor(ctx, revision, "HEAD")
	if err != nil {
		return false, fmt.Errorf("check whether HEAD contains %q: %w", revision, err)
	}
	return contains, nil
}

// BranchContainsRevisionContext reports whether revision is an ancestor of a local branch tip.
func (s *Service) BranchContainsRevisionContext(ctx context.Context, branch, revision string) (bool, error) {
	if branch == "" || revision == "" {
		return false, errors.New("branch ancestry check requires a branch and revision")
	}
	contains, err := s.repo.isAncestor(ctx, revision, "refs/heads/"+branch)
	if err != nil {
		return false, fmt.Errorf("check whether branch %q contains %q: %w", branch, revision, err)
	}
	return contains, nil
}

// DiffFingerprint returns a hash of the current working tree state (tracked diffs + untracked file content).
// used for stalemate detection - if the fingerprint changes between rounds, Claude made edits.
func (s *Service) DiffFingerprint() (string, error) {
	return s.repo.diffFingerprint()
}

// CurrentBranch returns the name of the current branch, or empty string for detached HEAD state.
func (s *Service) CurrentBranch() (string, error) {
	branch, err := s.repo.currentBranch()
	if err != nil {
		return "", fmt.Errorf("current branch: %w", err)
	}
	return branch, nil
}

// OriginURL returns the configured URL for the origin remote without applying Git URL rewrites.
func (s *Service) OriginURL() (string, error) {
	remoteURL, err := s.repo.originURL()
	if err != nil {
		return "", fmt.Errorf("read origin remote URL: %w", err)
	}
	return remoteURL, nil
}

// OriginPushURLs returns every effective URL Git will use when pushing to origin. Unlike
// OriginURL, these URLs include pushurl and URL-rewrite configuration.
func (s *Service) OriginPushURLs() ([]string, error) {
	remoteURLs, err := s.repo.originPushURLs()
	if err != nil {
		return nil, fmt.Errorf("read effective origin push URLs: %w", err)
	}
	return remoteURLs, nil
}

// CheckoutBranch switches the working tree to an existing local branch.
func (s *Service) CheckoutBranch(name string) error {
	if err := s.repo.checkoutBranch(name); err != nil {
		return fmt.Errorf("checkout branch %q: %w", name, err)
	}
	return nil
}

// IsDirty reports whether the working tree has staged or modified tracked files.
func (s *Service) IsDirty() (bool, error) {
	dirty, err := s.repo.isDirty()
	if err != nil {
		return false, fmt.Errorf("check working tree: %w", err)
	}
	return dirty, nil
}

// IsDirtyAll reports whether the working tree has any staged, unstaged, or untracked changes.
func (s *Service) IsDirtyAll() (bool, error) {
	dirty, err := s.repo.isDirtyAll()
	if err != nil {
		return false, fmt.Errorf("check complete working tree: %w", err)
	}
	return dirty, nil
}

// IsDefaultBranch returns true if the current branch matches the given default branch.
// strips "origin/" prefix from defaultBranch for comparison (auto-detect may return "origin/main").
// when defaultBranch is empty, falls back to checking "main" and "master".
func (s *Service) IsDefaultBranch(defaultBranch string) (bool, error) {
	branch, err := s.repo.currentBranch()
	if err != nil {
		return false, fmt.Errorf("is default branch: %w", err)
	}
	return s.matchesDefaultBranch(branch, defaultBranch), nil
}

// matchesDefaultBranch checks if branch matches the given default branch.
// strips "origin/" prefix from defaultBranch for comparison.
// when defaultBranch is empty, falls back to checking "main" and "master".
func (s *Service) matchesDefaultBranch(branch, defaultBranch string) bool {
	if defaultBranch == "" {
		return branch == "main" || branch == "master"
	}
	normalized := strings.TrimPrefix(defaultBranch, "origin/")
	return branch == normalized
}

// GetDefaultBranch returns the default branch name.
// detects from origin/HEAD or common branch names (main, master, trunk, develop).
func (s *Service) GetDefaultBranch() string {
	return s.repo.getDefaultBranch()
}

// HasCommits returns true if the repository has at least one commit.
func (s *Service) HasCommits() (bool, error) {
	has, err := s.repo.hasCommits()
	if err != nil {
		return false, fmt.Errorf("has commits: %w", err)
	}
	return has, nil
}

// CreateBranch creates a new branch and switches to it.
func (s *Service) CreateBranch(name string) error {
	if err := s.repo.createBranch(name); err != nil {
		return fmt.Errorf("create branch: %w", err)
	}
	return nil
}

// BranchExists reports whether a local branch with the given name exists.
// a commit hash or any other revision that is not a local branch returns false.
func (s *Service) BranchExists(name string) bool {
	if name == "" {
		return false
	}
	return s.repo.branchExists(name)
}

// BranchHash returns the commit hash a local branch points at, without requiring it to be
// checked out anywhere.
func (s *Service) BranchHash(name string) (string, error) {
	if name == "" {
		return "", errors.New("branch head: empty branch name")
	}
	hash, err := s.repo.branchHash(name)
	if err != nil {
		return "", fmt.Errorf("branch head: %w", err)
	}
	return hash, nil
}

// RevisionExistsContext reports whether revision resolves to a commit and honors cancellation.
// Missing or malformed revisions return false without an error; repository and command failures
// are returned to the caller.
func (s *Service) RevisionExistsContext(ctx context.Context, revision string) (bool, error) {
	if revision == "" {
		return false, nil
	}
	exists, err := s.repo.revisionExists(ctx, revision)
	if err != nil {
		return false, fmt.Errorf("check revision %q: %w", revision, err)
	}
	return exists, nil
}

// PlanArchivedAtRevision reports whether planFile has been removed from its active path and exists
// in completed/ at revision. Both supported date-prefix spellings are checked because plan agents
// may normalize the filename before MovePlanToCompleted commits the archive.
func (s *Service) PlanArchivedAtRevision(revision, planFile string) (bool, error) {
	planFile = s.resolveFilesystemCase(planFile)
	if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(planFile)); err == nil {
		planFile = filepath.Join(resolvedDir, filepath.Base(planFile))
	}
	activePaths := []string{planFile}
	archivePaths := []string{filepath.Join(filepath.Dir(planFile), "completed", filepath.Base(planFile))}
	if altBase := plan.AltDateBasename(filepath.Base(planFile)); altBase != "" {
		activePaths = append(activePaths, filepath.Join(filepath.Dir(planFile), altBase))
		archivePaths = append(archivePaths, filepath.Join(filepath.Dir(planFile), "completed", altBase))
	}
	for _, path := range activePaths {
		exists, err := s.repo.fileExistsAt(revision, path)
		if err != nil {
			return false, fmt.Errorf("inspect active plan %q at %q: %w", path, revision, err)
		}
		if exists {
			return false, nil
		}
	}
	for _, path := range archivePaths {
		exists, err := s.repo.fileExistsAt(revision, path)
		if err != nil {
			return false, fmt.Errorf("inspect archived plan %q at %q: %w", path, revision, err)
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

// ResolveBaseBranch validates an explicit local base branch or auto-detects main/master.
func (s *Service) ResolveBaseBranch(explicit string) (string, error) {
	if explicit != "" {
		if !s.repo.branchExists(explicit) {
			return "", fmt.Errorf("base branch %q does not exist", explicit)
		}
		return explicit, nil
	}

	for _, name := range []string{"main", "master"} {
		if s.repo.branchExists(name) {
			return name, nil
		}
	}
	return "", errors.New("base branch not found: neither main nor master exists")
}

// MergeBranch merges branch into the currently checked-out branch.
func (s *Service) MergeBranch(branch string) error {
	return s.MergeBranchCommitContext(context.Background(), branch, "refs/heads/"+branch)
}

// MergeBranchContext merges branch into the current branch and honors cancellation.
func (s *Service) MergeBranchContext(ctx context.Context, branch string) error {
	return s.MergeBranchCommitContext(ctx, branch, "refs/heads/"+branch)
}

// MergeBranchCommitContext merges branch and verifies that expectedHead is incorporated into the
// resulting HEAD. A successful Git command that does not satisfy that invariant is rolled back.
func (s *Service) MergeBranchCommitContext(ctx context.Context, branch, expectedHead string) error {
	if err := s.repo.mergeBranch(ctx, branch, expectedHead); err != nil {
		return fmt.Errorf("merge branch %q: %w", branch, err)
	}
	return nil
}

// DeleteBranch safely deletes a fully merged local branch.
func (s *Service) DeleteBranch(name string) error {
	if err := s.repo.deleteBranch(name); err != nil {
		return fmt.Errorf("delete branch %q: %w", name, err)
	}
	return nil
}

// Push pushes branch to origin and configures it as the upstream branch.
func (s *Service) Push(branch string) error {
	return s.PushContext(context.Background(), branch)
}

// PushContext pushes branch to origin, configures its upstream, and honors cancellation.
func (s *Service) PushContext(ctx context.Context, branch string) error {
	if err := s.repo.push(ctx, branch); err != nil {
		return fmt.Errorf("push branch %q: %w", branch, err)
	}
	return nil
}

// Worktrees returns every registered worktree. Git lists the primary worktree first.
func (s *Service) Worktrees() ([]Worktree, error) {
	worktrees, err := s.repo.worktrees()
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	return worktrees, nil
}

// RemoveWorktreeSafe removes a clean registered worktree without forcing deletion.
func (s *Service) RemoveWorktreeSafe(path string) error {
	if err := s.repo.removeWorktreeSafe(path); err != nil {
		return fmt.Errorf("remove clean worktree: %w", err)
	}
	return nil
}

// EffectiveBranchName returns branchOverride when set, otherwise derives the branch name from
// planFile after resolving its actual on-disk filename case. Keeping this consistent with
// worktree creation ensures progress paths and resume validation use the branch Git created.
func (s *Service) EffectiveBranchName(planFile, branchOverride string) string {
	if branchOverride != "" {
		return branchOverride
	}
	return plan.ExtractBranchName(s.resolveFilesystemCase(planFile))
}

// ValidatePlanChain verifies repository-dependent chain invariants before any plan branch is
// created. The CLI's filesystem validation cannot tell whether an untracked plan is ignored or
// whether a derived name is a valid literal Git branch.
func (s *Service) ValidatePlanChain(planFiles []string) error {
	for _, planFile := range planFiles {
		resolved := s.resolveFilesystemCase(planFile)
		if err := s.validateWorktreePlanFile(resolved); err != nil {
			return fmt.Errorf("validate plan chain entry %q: %w", planFile, err)
		}
		branchName := s.EffectiveBranchName(resolved, "")
		if err := s.repo.validateBranchName(branchName); err != nil {
			return fmt.Errorf("invalid plan branch %q for %q: %w", branchName, planFile, err)
		}
	}
	return nil
}

// ValidatePlanFile verifies that a plan is a regular, repository-contained path that Git can
// carry. It is used again inside a prepared worktree so a tree-entry substitution cannot reach the
// plan parser.
func (s *Service) ValidatePlanFile(planFile string) error {
	return s.validateWorktreePlanFile(planFile)
}

// inspectPlanChanges returns dirty files other than the plan and whether the plan itself changed.
func (s *Service) inspectPlanChanges(planFile string) ([]string, bool, error) {
	dirtyFiles, err := s.repo.hasChangesOtherThan(planFile)
	if err != nil {
		return nil, false, fmt.Errorf("check uncommitted files: %w", err)
	}
	planHasChanges, err := s.repo.fileHasChanges(planFile)
	if err != nil {
		return nil, false, fmt.Errorf("check plan file status: %w", err)
	}
	return dirtyFiles, planHasChanges, nil
}

func (s *Service) inspectPlanChainChanges(planFiles []string) ([]string, []string, error) {
	dirtyFiles, err := s.repo.hasChangesOtherThan(planFiles...)
	if err != nil {
		return nil, nil, fmt.Errorf("check uncommitted files: %w", err)
	}
	changedPlans := make([]string, 0, len(planFiles))
	for _, planFile := range planFiles {
		changed, changeErr := s.repo.fileHasChanges(planFile)
		if changeErr != nil {
			return nil, nil, fmt.Errorf("check plan file status %q: %w", planFile, changeErr)
		}
		if changed {
			changedPlans = append(changedPlans, planFile)
		}
	}
	return dirtyFiles, changedPlans, nil
}

// validateWorktreePlanFile rejects plans that cannot be carried into the new worktree.
// In particular, ignored untracked files and paths outside the repository must fail before
// --commit is allowed to mutate the source checkout.
func (s *Service) validateWorktreePlanFile(planFile string) error {
	if !filepath.IsAbs(planFile) {
		planFile = filepath.Join(s.repo.root(), planFile)
	}
	planFile = s.resolveFilesystemCase(planFile)
	info, err := os.Lstat(planFile)
	if err != nil {
		return fmt.Errorf("inspect plan file %q: %w", planFile, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("plan file %q is not a regular file", planFile)
	}

	changed, err := s.repo.fileHasChanges(planFile)
	if err != nil {
		return fmt.Errorf("validate plan file %q: %w", planFile, err)
	}
	if changed {
		return nil
	}
	tracked, err := s.repo.fileTracked(planFile)
	if err != nil {
		return fmt.Errorf("check tracked plan file %q: %w", planFile, err)
	}
	if !tracked {
		return fmt.Errorf("plan file %q is ignored or otherwise unavailable to Git", planFile)
	}
	return nil
}

// prepareBranchPlan validates the non-worktree branch-creation path.
func (s *Service) prepareBranchPlan(planFile, defaultBranch, branchOverride string) (string, bool, error) {
	currentBranch, err := s.repo.currentBranch()
	if err != nil {
		return "", false, fmt.Errorf("check current branch: %w", err)
	}
	if !s.matchesDefaultBranch(currentBranch, defaultBranch) {
		return "", false, nil // already on feature branch, caller should skip
	}

	branchName := s.EffectiveBranchName(planFile, branchOverride)
	dirtyFiles, planHasChanges, err := s.inspectPlanChanges(planFile)
	if err != nil {
		return "", false, err
	}
	if len(dirtyFiles) > 0 {
		fileList := s.formatDirtyFiles(dirtyFiles)
		return "", false, fmt.Errorf("cannot create branch %q: worktree has uncommitted changes\n\n"+
			"uncommitted files:\n%s\n\n"+
			"loopai needs to create a feature branch from %s to isolate plan work.\n\n"+
			"options:\n"+
			"  git stash && loopai %s && git stash pop   # stash changes temporarily\n"+
			"  git commit -am \"wip\"                       # commit changes first\n"+
			"  loopai --review                            # skip branch creation (review-only mode)",
			branchName, fileList, currentBranch, planFile)
	}
	return branchName, planHasChanges, nil
}

// prepareWorktreePlan validates worktree creation from the current HEAD.
func (s *Service) prepareWorktreePlan(planFile, branchOverride string) (string, bool, error) {
	currentBranch, err := s.repo.currentBranch()
	if err != nil {
		return "", false, fmt.Errorf("check current branch: %w", err)
	}
	branchName := s.EffectiveBranchName(planFile, branchOverride)
	if currentBranch == branchName {
		return "", false, fmt.Errorf("plan branch %q is already checked out here; switch to the source branch or run without --worktree", branchName)
	}
	dirtyFiles, planHasChanges, err := s.inspectPlanChanges(planFile)
	if err != nil {
		return "", false, err
	}
	if len(dirtyFiles) > 0 {
		return "", false, fmt.Errorf("cannot create worktree: worktree has uncommitted changes other than the plan file\n\n"+
			"uncommitted files:\n%s", s.formatDirtyFiles(dirtyFiles))
	}
	return branchName, planHasChanges, nil
}

// CreateBranchForPlan creates or switches to a feature branch for plan execution.
// If already on a feature branch (not the default branch), returns nil immediately.
// If on the default branch, extracts branch name from plan file and creates/switches to it.
// If plan file has uncommitted changes and is the only dirty file, auto-commits it.
// defaultBranch is the resolved default branch name (e.g. "main", "develop").
// branchOverride, when non-empty, is used directly instead of deriving the name from planFile.
func (s *Service) CreateBranchForPlan(planFile, defaultBranch, branchOverride string) error {
	planFile = s.resolveFilesystemCase(planFile)
	branchName, planHasChanges, err := s.prepareBranchPlan(planFile, defaultBranch, branchOverride)
	if err != nil {
		return err
	}
	if branchName == "" {
		return nil // already on feature branch
	}

	// create or switch to branch
	if s.repo.branchExists(branchName) {
		s.log.Printf("switching to existing branch: %s\n", branchName)
		if err := s.repo.checkoutBranch(branchName); err != nil {
			return fmt.Errorf("checkout branch %s: %w", branchName, err)
		}
	} else {
		s.log.Printf("creating branch: %s\n", branchName)
		if err := s.repo.createBranch(branchName); err != nil {
			return fmt.Errorf("create branch %s: %w", branchName, err)
		}
	}

	// auto-commit plan file if it was the only uncommitted file
	if planHasChanges {
		s.log.Printf("committing plan file: %s\n", filepath.Base(planFile))
		if err := s.repo.add(planFile); err != nil {
			return fmt.Errorf("stage plan file: %w", err)
		}
		if err := s.repo.commit(s.appendTrailer("add plan: " + branchName)); err != nil {
			return fmt.Errorf("commit plan file: %w", err)
		}
	}

	return nil
}

// CreateBranchForPlanFromCurrentHEADContext creates or reuses a chained plan branch from the
// currently checked-out plan branch. Unlike CreateBranchForPlan, it intentionally does not skip
// when HEAD is already on a feature branch. Chained plan phases must commit everything before the
// hand-off, so any dirty state is rejected rather than auto-committed onto the successor branch.
func (s *Service) CreateBranchForPlanFromCurrentHEADContext(
	ctx context.Context, planFile, branchOverride string,
) error {
	return s.createBranchForPlanFromCurrentHEADContext(ctx, planFile, branchOverride, "", nil)
}

// CreateBranchForPlanChainContext creates the first non-worktree chain branch from the current
// HEAD even when the source checkout is already on a feature branch. The listed plan files are the
// only dirty paths allowed and are committed together on the new chain branch.
func (s *Service) CreateBranchForPlanChainContext(
	ctx context.Context, planFile, branchOverride string, chainPlanFiles []string,
) error {
	return s.createBranchForPlanFromCurrentHEADContext(ctx, planFile, branchOverride, "", chainPlanFiles)
}

// CreateBranchForPlanFromExpectedHEADContext creates a successor branch only when the checkout is
// still at the immutable predecessor tip captured by the chain coordinator.
func (s *Service) CreateBranchForPlanFromExpectedHEADContext(
	ctx context.Context, planFile, branchOverride, expectedHead string,
) error {
	return s.createBranchForPlanFromCurrentHEADContext(ctx, planFile, branchOverride, expectedHead, nil)
}

func (s *Service) createBranchForPlanFromCurrentHEADContext(
	ctx context.Context, planFile, branchOverride, expectedHead string, chainPlanFiles []string,
) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start chained plan branch: %w", err)
	}
	planFile = s.resolveFilesystemCase(planFile)
	branchName := s.EffectiveBranchName(planFile, branchOverride)
	changedPlans, currentBranch, startCommit, err := s.prepareChainedBranchSource(
		branchName, expectedHead, chainPlanFiles,
	)
	if err != nil {
		return err
	}
	reusingBranch := s.repo.branchExists(branchName)
	var planSnapshots []planFileSnapshot
	if reusingBranch && len(chainPlanFiles) > 0 {
		planSnapshots, err = s.snapshotPlanFiles(chainPlanFiles)
		if err != nil {
			return fmt.Errorf("snapshot chain plans before reusing branch %q: %w", branchName, err)
		}
	}
	if switchErr := s.switchToChainedPlanBranch(ctx, branchName, currentBranch, startCommit); switchErr != nil {
		return switchErr
	}
	if reusingBranch && len(planSnapshots) > 0 {
		var restoredPlans []string
		restoredPlans, err = s.restorePlanSnapshots(planSnapshots)
		if err != nil {
			return fmt.Errorf("restore chain plans on reused branch %q: %w", branchName, err)
		}
		// Checkout can carry a dirty or untracked source plan onto the reused branch
		// byte-for-byte. Keep those source changes even when no snapshot rewrite was needed;
		// commitChangedChainPlans asks Git which candidates still differ from the new HEAD.
		changedPlans = append(changedPlans, restoredPlans...)
	}
	return s.commitChangedChainPlans(branchName, changedPlans)
}

type planFileSnapshot struct {
	path string
	data []byte
	mode os.FileMode
}

func (s *Service) snapshotPlanFiles(planFiles []string) ([]planFileSnapshot, error) {
	snapshots := make([]planFileSnapshot, 0, len(planFiles))
	for _, planFile := range planFiles {
		path := s.resolveFilesystemCase(planFile)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect plan file %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("plan file %q is not a regular file", path)
		}
		data, err := os.ReadFile(path) //nolint:gosec // validated repository plan path
		if err != nil {
			return nil, fmt.Errorf("read plan file %q: %w", path, err)
		}
		snapshots = append(snapshots, planFileSnapshot{path: path, data: data, mode: info.Mode()})
	}
	return snapshots, nil
}

func (s *Service) restorePlanSnapshots(snapshots []planFileSnapshot) ([]string, error) {
	changedPlans := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		relPath, err := filepath.Rel(s.repo.root(), snapshot.path)
		if err != nil || filepath.IsAbs(relPath) || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("plan file escapes repository root: %s", snapshot.path)
		}
		dstDir, err := mkdirAllNoSymlinks(s.repo.root(), filepath.Dir(relPath))
		if err != nil {
			return nil, err
		}
		dstPath := filepath.Join(dstDir, filepath.Base(relPath))
		if err := validateCopyDestination(dstPath); err != nil {
			return nil, err
		}
		if current, readErr := os.ReadFile(dstPath); //nolint:gosec // root-contained path validated above
		readErr == nil && bytes.Equal(current, snapshot.data) {
			continue
		} else if readErr != nil && !os.IsNotExist(readErr) {
			return nil, fmt.Errorf("read destination plan %q: %w", dstPath, readErr)
		}
		if err := writeFileReplacing(snapshot.data, snapshot.mode.Perm(), dstDir, dstPath); err != nil {
			return nil, err
		}
		changedPlans = append(changedPlans, dstPath)
	}
	return changedPlans, nil
}

func (s *Service) prepareChainedBranchSource(
	branchName, expectedHead string, chainPlanFiles []string,
) ([]string, string, string, error) {
	if branchName == "" {
		return nil, "", "", errors.New("plan branch name is empty")
	}
	if err := s.repo.validateBranchName(branchName); err != nil {
		return nil, "", "", fmt.Errorf("invalid plan branch %q: %w", branchName, err)
	}
	changedPlans, err := s.validateChainedBranchDirtyState(branchName, chainPlanFiles)
	if err != nil {
		return nil, "", "", err
	}
	currentBranch, err := s.repo.currentBranch()
	if err != nil {
		return nil, "", "", fmt.Errorf("identify current branch before starting chained plan %q: %w", branchName, err)
	}
	if currentBranch == branchName || (currentBranch != "" && strings.EqualFold(currentBranch, branchName)) {
		return nil, "", "", fmt.Errorf("cannot start chained plan branch %q: previous and next plans resolve to the same branch", branchName)
	}
	startCommit, err := s.repo.headHash()
	if err != nil {
		return nil, "", "", fmt.Errorf("identify current HEAD before starting chained plan %q: %w", branchName, err)
	}
	if expectedHead != "" && startCommit != expectedHead {
		return nil, "", "", fmt.Errorf("cannot start chained plan branch %q: previous plan tip changed from %s to %s", branchName, expectedHead, startCommit)
	}
	return changedPlans, currentBranch, startCommit, nil
}

func (s *Service) validateChainedBranchDirtyState(branchName string, chainPlanFiles []string) ([]string, error) {
	if len(chainPlanFiles) == 0 {
		dirty, err := s.repo.isDirtyAll()
		if err != nil {
			return nil, fmt.Errorf("check working tree before starting chained plan %q: %w", branchName, err)
		}
		if dirty {
			return nil, fmt.Errorf("cannot start chained plan branch %q: working tree has uncommitted changes; each plan must leave a clean tree before the next plan starts", branchName)
		}
		return nil, nil
	}
	resolvedPlans := make([]string, 0, len(chainPlanFiles))
	for _, path := range chainPlanFiles {
		resolvedPlans = append(resolvedPlans, s.resolveFilesystemCase(path))
	}
	dirtyFiles, changedPlans, err := s.inspectPlanChainChanges(resolvedPlans)
	if err != nil {
		return nil, err
	}
	if len(dirtyFiles) > 0 {
		return nil, fmt.Errorf("cannot start chained plan branch %q: working tree has uncommitted changes outside the plan chain\n\nuncommitted files:\n%s",
			branchName, s.formatDirtyFiles(dirtyFiles))
	}
	return changedPlans, nil
}

func (s *Service) switchToChainedPlanBranch(
	ctx context.Context, branchName, currentBranch, startCommit string,
) error {
	if !s.repo.branchExists(branchName) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("start chained plan branch: %w", err)
		}
		s.log.Printf("creating chained plan branch: %s (from current HEAD)\n", branchName)
		if err := s.repo.createBranch(branchName); err != nil {
			return fmt.Errorf("create chained plan branch %s: %w", branchName, err)
		}
		return nil
	}
	startRef := "current HEAD"
	if currentBranch != "" {
		startRef = "refs/heads/" + currentBranch
	}
	if err := s.validateExistingPlanBranchAt(ctx, branchName, startCommit, startRef); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start chained plan branch: %w", err)
	}
	s.log.Printf("switching to existing chained plan branch: %s\n", branchName)
	if err := s.repo.checkoutBranch(branchName); err != nil {
		return fmt.Errorf("checkout chained plan branch %s: %w", branchName, err)
	}
	containsStart, err := s.repo.isAncestor(ctx, startCommit, "HEAD")
	if err != nil {
		return s.restoreChainedPlanSource(currentBranch, startCommit,
			fmt.Errorf("check chained plan branch %q against previous plan tip: %w", branchName, err))
	}
	if containsStart {
		return nil
	}
	if err := s.repo.mergeRevision(ctx, startCommit, startCommit); err != nil {
		return s.restoreChainedPlanSource(currentBranch, startCommit,
			fmt.Errorf("merge previous plan tip into chained plan branch %q: %w", branchName, err))
	}
	return nil
}

func (s *Service) restoreChainedPlanSource(currentBranch, startCommit string, cause error) error {
	target := currentBranch
	if target == "" {
		target = startCommit
	}
	if err := s.repo.checkoutBranch(target); err != nil {
		return errors.Join(cause, fmt.Errorf("restore chained plan source %q: %w", target, err))
	}
	return cause
}

// ResumePlanChainBranchContext switches to an already-created successor branch and verifies that
// it still contains the immutable completed predecessor tip. Dirty state is accepted only when the
// target branch is already checked out, matching ordinary interrupted non-worktree resume behavior.
func (s *Service) ResumePlanChainBranchContext(ctx context.Context, branch, predecessorTip string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("resume chained plan branch: %w", err)
	}
	if !s.repo.branchExists(branch) {
		return fmt.Errorf("resume chained plan branch %q: branch does not exist", branch)
	}
	current, err := s.repo.currentBranch()
	if err != nil {
		return fmt.Errorf("identify current branch before resuming chained plan %q: %w", branch, err)
	}
	startCommit, err := s.repo.headHash()
	if err != nil {
		return fmt.Errorf("identify current HEAD before resuming chained plan %q: %w", branch, err)
	}
	switched := current != branch
	if switched {
		dirty, dirtyErr := s.repo.isDirtyAll()
		if dirtyErr != nil {
			return fmt.Errorf("check working tree before resuming chained plan %q: %w", branch, dirtyErr)
		}
		if dirty {
			return fmt.Errorf("cannot resume chained plan branch %q from another branch with uncommitted changes", branch)
		}
		if checkoutErr := s.repo.checkoutBranch(branch); checkoutErr != nil {
			return fmt.Errorf("checkout chained plan branch %q for resume: %w", branch, checkoutErr)
		}
	}
	contains, err := s.repo.isAncestor(ctx, predecessorTip, "HEAD")
	if err != nil {
		cause := fmt.Errorf("validate resumed chained plan branch %q: %w", branch, err)
		if switched {
			return s.restoreChainedPlanSource(current, startCommit, cause)
		}
		return cause
	}
	if !contains {
		cause := fmt.Errorf("cannot resume chained plan branch %q: HEAD does not contain predecessor tip %s", branch, predecessorTip)
		if switched {
			return s.restoreChainedPlanSource(current, startCommit, cause)
		}
		return cause
	}
	return nil
}

// ResumeFirstPlanChainBranchContext resumes an interrupted first non-worktree member. If branch is
// already checked out, dirty chain-plan inputs are accepted and committed so a crash during initial
// branch preparation can finish safely; unrelated dirty files remain an error.
func (s *Service) ResumeFirstPlanChainBranchContext(
	ctx context.Context, branch string, chainPlanFiles []string, preparedTip string,
) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("resume first chained plan branch: %w", err)
	}
	if !s.repo.branchExists(branch) {
		return fmt.Errorf("resume first chained plan branch %q: branch does not exist", branch)
	}
	current, err := s.repo.currentBranch()
	if err != nil {
		return fmt.Errorf("identify current branch before resuming first chained plan %q: %w", branch, err)
	}
	if preparedTip != "" {
		return s.resumePreparedPlanChainBranch(ctx, branch, current, preparedTip)
	}
	if current != branch && (current == "" || !strings.EqualFold(current, branch)) {
		dirty, dirtyErr := s.repo.isDirtyAll()
		if dirtyErr != nil {
			return fmt.Errorf("check working tree before resuming first chained plan %q: %w", branch, dirtyErr)
		}
		if dirty {
			return fmt.Errorf("cannot resume first chained plan branch %q from another branch with uncommitted changes", branch)
		}
		if checkoutErr := s.repo.checkoutBranch(branch); checkoutErr != nil {
			return fmt.Errorf("checkout first chained plan branch %q for resume: %w", branch, checkoutErr)
		}
	}
	changedPlans, err := s.validateChainedBranchDirtyState(branch, chainPlanFiles)
	if err != nil {
		return err
	}
	return s.commitChangedChainPlans(branch, changedPlans)
}

func (s *Service) resumePreparedPlanChainBranch(
	ctx context.Context, branch, current, preparedTip string,
) error {
	if current != branch && (current == "" || !strings.EqualFold(current, branch)) {
		dirty, err := s.repo.isDirtyAll()
		if err != nil {
			return fmt.Errorf("check working tree before resuming prepared chain branch %q: %w", branch, err)
		}
		if dirty {
			return fmt.Errorf("cannot resume prepared chain branch %q from another branch with uncommitted changes", branch)
		}
		if err := s.repo.checkoutBranch(branch); err != nil {
			return fmt.Errorf("checkout prepared chain branch %q for resume: %w", branch, err)
		}
	}
	contains, err := s.repo.isAncestor(ctx, preparedTip, "HEAD")
	if err != nil {
		return fmt.Errorf("validate prepared chain branch %q: %w", branch, err)
	}
	if !contains {
		return fmt.Errorf("cannot resume prepared chain branch %q: HEAD does not contain saved prepared tip %s", branch, preparedTip)
	}
	return nil
}

func (s *Service) commitChangedChainPlans(branchName string, changedPlans []string) error {
	committablePlans := make([]string, 0, len(changedPlans))
	seen := make(map[string]struct{}, len(changedPlans))
	for _, changedPlan := range changedPlans {
		if _, exists := seen[changedPlan]; exists {
			continue
		}
		seen[changedPlan] = struct{}{}
		if err := s.repo.add(changedPlan); err != nil {
			return fmt.Errorf("stage chain plan file %q: %w", changedPlan, err)
		}
		changed, err := s.repo.fileHasChanges(changedPlan)
		if err != nil {
			return fmt.Errorf("check staged chain plan file %q: %w", changedPlan, err)
		}
		if changed {
			committablePlans = append(committablePlans, changedPlan)
		}
	}
	if len(committablePlans) == 0 {
		return nil
	}
	if err := s.repo.commitFiles(s.appendTrailer("add plan chain: "+branchName), committablePlans...); err != nil {
		return fmt.Errorf("commit chain plan files: %w", err)
	}
	return nil
}

// CreateWorktreeForPlan creates an isolated git worktree for plan execution from the current HEAD.
// It derives the branch name from the plan file and creates the worktree at
// .loopai/worktrees/<branch>.
// returns (worktree path, planNeedsCommit, error). when planNeedsCommit is true the caller
// must commit the plan file in the worktree context (via CommitPlanFile on the worktree's
// git service) so the commit lands on the feature branch rather than the default branch.
// branchOverride, when non-empty, is used directly instead of deriving the name from planFile.
func (s *Service) CreateWorktreeForPlan(planFile, branchOverride string) (string, bool, error) {
	return s.CreateWorktreeForPlanContext(context.Background(), planFile, branchOverride)
}

// CreateWorktreeForPlanContext is CreateWorktreeForPlan with caller cancellation.
func (s *Service) CreateWorktreeForPlanContext(
	ctx context.Context, planFile, branchOverride string,
) (string, bool, error) {
	return s.createWorktreeForPlan(ctx, planFile, branchOverride, "", "", nil, false)
}

// CreateWorktreeForPlanFromRefContext creates a plan worktree whose new branch starts at
// startRef. An empty startRef preserves CreateWorktreeForPlanContext's current-HEAD behavior.
// Existing plan branches are synchronized with startRef before they are reused.
func (s *Service) CreateWorktreeForPlanFromRefContext(
	ctx context.Context, planFile, branchOverride, startRef string,
) (string, bool, error) {
	return s.createWorktreeForPlan(ctx, planFile, branchOverride, "", startRef, nil, false)
}

// CreateWorktreeForPlanAfterAutoCommit creates a plan worktree after sourceHeadBefore was
// auto-committed. An existing plan branch that contained the previous source HEAD is brought
// forward by merging the new source commit after the worktree is created.
func (s *Service) CreateWorktreeForPlanAfterAutoCommit(
	ctx context.Context, planFile, branchOverride, sourceHeadBefore string,
) (string, bool, error) {
	if sourceHeadBefore == "" {
		return "", false, errors.New("source HEAD before auto-commit is empty")
	}
	return s.createWorktreeForPlan(ctx, planFile, branchOverride, sourceHeadBefore, "", nil, false)
}

// CreateWorktreeForPlanChainContext creates a chain worktree while treating every listed plan as
// an allowed source-checkout change. The first worktree carries all changed plans into the stacked
// branch; successors preserve the predecessor's committed copy when it already exists.
func (s *Service) CreateWorktreeForPlanChainContext(
	ctx context.Context, planFile, branchOverride, startRef string, chainPlanFiles []string,
) (string, bool, error) {
	return s.createWorktreeForPlan(ctx, planFile, branchOverride, "", startRef, chainPlanFiles, false)
}

// CreateWorktreeForResumedPlanChainContext recreates a removed first-member worktree without
// replacing or resurrecting plan state already committed on its fully prepared branch.
func (s *Service) CreateWorktreeForResumedPlanChainContext(
	ctx context.Context, planFile, branchOverride, startRef, preparedTip string, chainPlanFiles []string,
) (string, bool, error) {
	return s.createWorktreeForPlan(ctx, planFile, branchOverride, preparedTip, startRef, chainPlanFiles, true)
}

func (s *Service) createWorktreeForPlan(
	ctx context.Context, planFile, branchOverride, existingBranchAncestor, startRef string,
	chainPlanFiles []string, preserveExistingChainPlans bool,
) (string, bool, error) {
	planFile = s.resolveFilesystemCase(planFile)
	startCommit, err := s.resolveWorktreeStartCommit(startRef)
	if err != nil {
		return "", false, err
	}

	// prune stale worktree entries first
	if pruneErr := s.repo.pruneWorktrees(); pruneErr != nil {
		s.log.Printf("warning: prune worktrees: %v\n", pruneErr)
	}

	branchName := s.EffectiveBranchName(planFile, branchOverride)
	if validateErr := s.validatePreparedPlanChainBranch(
		ctx, branchName, existingBranchAncestor, preserveExistingChainPlans,
	); validateErr != nil {
		return "", false, validateErr
	}

	validationAncestor := existingBranchAncestor
	if validationAncestor == "" {
		validationAncestor = startCommit
	}
	if preflightErr := s.preflightWorktreeForPlan(ctx, planFile, branchOverride, validationAncestor, startRef); preflightErr != nil {
		return "", false, preflightErr
	}
	wtPath := filepath.Join(s.repo.root(), ".loopai", "worktrees", branchName)

	planHasChanges, changedChainPlans, err := s.inspectWorktreePlanChanges(
		planFile, branchOverride, chainPlanFiles,
	)
	if err != nil {
		return "", false, err
	}
	source, err := s.repo.currentBranch()
	if err != nil {
		return "", false, fmt.Errorf("identify worktree source: %w", err)
	}
	if source == "" {
		head, headErr := s.repo.headHash()
		if headErr != nil {
			return "", false, fmt.Errorf("identify worktree source commit: %w", headErr)
		}
		const shortHashLength = 7
		if len(head) > shortHashLength {
			head = head[:shortHashLength]
		}
		source = head
	}

	// create worktree with branch
	reusingBranch := s.repo.branchExists(branchName)
	mergeStartCommit := startCommit
	if preserveExistingChainPlans {
		// A prepared first-member branch is an immutable resume target. It has already
		// been validated against the saved tip, so never synchronize it with a source
		// checkout that may have advanced while the worktree was absent.
		mergeStartCommit = existingBranchAncestor
	}
	if reusingBranch {
		if err := s.createExistingPlanWorktree(
			ctx, wtPath, branchName, validationAncestor, mergeStartCommit, startRef,
		); err != nil {
			return "", false, err
		}
	} else {
		if startRef != "" {
			source = startRef
		}
		s.log.Printf("creating worktree with new branch: %s (from %s)\n", branchName, source)
		if err := s.repo.addWorktree(ctx, wtPath, branchName, true, startCommit); err != nil {
			addErr := fmt.Errorf("add worktree with new branch: %w", err)
			return "", false, s.cleanupFailedWorktreeAdd(wtPath, addErr)
		}
	}

	// A first chain worktree freezes every uncommitted plan into the stacked branch. A successor
	// preserves the plan from its predecessor when present, so plan N can intentionally edit plan
	// N+1. The source copy is used only when the explicit start commit predates the plan.
	filesToCopy, copyErr := s.worktreePlanFilesToCopy(
		planFile, startCommit, startRef, planHasChanges, changedChainPlans,
		len(chainPlanFiles) > 0, reusingBranch, chainPlanFiles,
	)
	if copyErr == nil && preserveExistingChainPlans {
		filesToCopy = nil
	}
	if copyErr != nil {
		_ = s.repo.removeWorktree(wtPath)
		return "", false, copyErr
	}
	if copyErr = s.copyPlanFilesToWorktree(filesToCopy, wtPath); copyErr != nil {
		_ = s.repo.removeWorktree(wtPath)
		return "", false, copyErr
	}
	planHasChanges = len(filesToCopy) > 0

	return wtPath, planHasChanges, nil
}

func (s *Service) validatePreparedPlanChainBranch(
	ctx context.Context, branchName, preparedTip string, required bool,
) error {
	if !required {
		return nil
	}
	if !s.repo.branchExists(branchName) {
		return fmt.Errorf("resume prepared plan branch %q: branch does not exist", branchName)
	}
	containsPrepared, err := s.repo.isAncestor(ctx, preparedTip, "refs/heads/"+branchName)
	if err != nil {
		return fmt.Errorf("validate prepared plan branch %q: %w", branchName, err)
	}
	if !containsPrepared {
		return fmt.Errorf("resume prepared plan branch %q: branch does not contain saved prepared tip %s", branchName, preparedTip)
	}
	return nil
}

func (s *Service) inspectWorktreePlanChanges(
	planFile, branchOverride string, chainPlanFiles []string,
) (bool, []string, error) {
	if len(chainPlanFiles) == 0 {
		_, changed, err := s.prepareWorktreePlan(planFile, branchOverride)
		return changed, nil, err
	}
	resolvedPlans := make([]string, 0, len(chainPlanFiles))
	for _, path := range chainPlanFiles {
		resolvedPlans = append(resolvedPlans, s.resolveFilesystemCase(path))
	}
	dirtyFiles, changedPlans, err := s.inspectPlanChainChanges(resolvedPlans)
	if err != nil {
		return false, nil, err
	}
	if len(dirtyFiles) > 0 {
		return false, nil, fmt.Errorf("cannot create worktree: worktree has uncommitted changes outside the plan chain\n\nuncommitted files:\n%s",
			s.formatDirtyFiles(dirtyFiles))
	}
	return false, changedPlans, nil
}

func (s *Service) worktreePlanFilesToCopy(
	planFile, startCommit, startRef string, planHasChanges bool, changedChainPlans []string,
	chain, reusingBranch bool, chainPlanFiles []string,
) ([]string, error) {
	if startRef == "" {
		if chain {
			if reusingBranch {
				return chainPlanFiles, nil
			}
			return changedChainPlans, nil
		}
		if planHasChanges {
			return []string{planFile}, nil
		}
		return nil, nil
	}
	existsAtStart, err := s.repo.fileExistsAt(startCommit, planFile)
	if err != nil {
		return nil, fmt.Errorf("inspect plan at worktree start: %w", err)
	}
	if chain && !existsAtStart {
		return nil, fmt.Errorf("chained plan %q is missing or not a regular file at predecessor tip %s", planFile, startCommit)
	}
	if !existsAtStart || (!chain && planHasChanges) {
		return []string{planFile}, nil
	}
	return nil, nil
}

func (s *Service) copyPlanFilesToWorktree(planFiles []string, wtPath string) error {
	for _, planFile := range planFiles {
		if err := s.copyToWorktree(planFile, wtPath); err != nil {
			return fmt.Errorf("copy plan to worktree: %w", err)
		}
	}
	return nil
}

func (s *Service) createExistingPlanWorktree(
	ctx context.Context, wtPath, branchName, validationAncestor, startCommit, startRef string,
) error {
	if err := s.validateExistingPlanBranchAt(ctx, branchName, validationAncestor, startRef); err != nil {
		return err
	}
	s.log.Printf("creating worktree with existing branch: %s\n", branchName)
	if err := s.repo.addWorktree(ctx, wtPath, branchName, false, ""); err != nil {
		addErr := fmt.Errorf("add worktree with existing branch: %w", err)
		return s.cleanupFailedWorktreeAdd(wtPath, addErr)
	}
	mergeErr := s.mergeWorktreeStart(ctx, wtPath, branchName, startCommit, startRef)
	if mergeErr == nil {
		return nil
	}
	if removeErr := s.repo.removeWorktree(wtPath); removeErr != nil {
		return errors.Join(mergeErr, fmt.Errorf("remove worktree after source merge failure: %w", removeErr))
	}
	return mergeErr
}

func (s *Service) cleanupFailedWorktreeAdd(wtPath string, cause error) error {
	if _, err := os.Stat(wtPath); err != nil {
		return cause
	}
	if removeErr := s.repo.removeWorktree(wtPath); removeErr != nil {
		return errors.Join(cause, fmt.Errorf("remove partial worktree after creation failure: %w", removeErr))
	}
	return cause
}

func (s *Service) mergeWorktreeStart(
	ctx context.Context, wtPath, branchName, startCommit, startRef string,
) error {
	if startCommit == "" {
		var err error
		startCommit, err = s.repo.headHash()
		if err != nil {
			return fmt.Errorf("identify source HEAD: %w", err)
		}
	}
	wtSvc, err := s.OpenWorktree(wtPath)
	if err != nil {
		return fmt.Errorf("open existing plan worktree: %w", err)
	}
	containsSource, err := wtSvc.repo.isAncestor(ctx, startCommit, "HEAD")
	if err != nil {
		return fmt.Errorf("check existing plan branch against %s: %w", worktreeStartLabel(startRef), err)
	}
	if containsSource {
		return nil
	}
	shortHead := startCommit
	const shortHashLength = 7
	if len(shortHead) > shortHashLength {
		shortHead = shortHead[:shortHashLength]
	}
	s.log.Printf("merging %s %s into existing plan branch %s\n", worktreeStartLabel(startRef), shortHead, branchName)
	if err := wtSvc.repo.mergeRevision(ctx, startCommit, startCommit); err != nil {
		return fmt.Errorf("merge %s into existing plan branch: %w", worktreeStartLabel(startRef), err)
	}
	return nil
}

func (s *Service) resolveWorktreeStartCommit(startRef string) (string, error) {
	if startRef == "" {
		return "", nil
	}
	commit, err := s.repo.revParse(startRef + "^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve worktree start ref %q: %w", startRef, err)
	}
	return commit, nil
}

func worktreeStartLabel(startRef string) string {
	if startRef == "" {
		return "source HEAD"
	}
	return fmt.Sprintf("start ref %q", startRef)
}

// PreflightWorktreeForPlan rejects deterministic target conflicts without changing repository
// state. Callers that may mutate the source checkout must run this while holding the repository
// lock before installing runtime ignores, and again afterward before creating an auto-commit.
func (s *Service) PreflightWorktreeForPlan(planFile, branchOverride string) error {
	return s.PreflightWorktreeForPlanContext(context.Background(), planFile, branchOverride)
}

// PreflightWorktreeForPlanContext is PreflightWorktreeForPlan with caller cancellation.
func (s *Service) PreflightWorktreeForPlanContext(ctx context.Context, planFile, branchOverride string) error {
	return s.preflightWorktreeForPlan(ctx, planFile, branchOverride, "", "")
}

// PreflightWorktreeForPlanFromRefContext validates worktree creation from startRef without
// changing repository state. An empty startRef preserves the current-HEAD behavior.
func (s *Service) PreflightWorktreeForPlanFromRefContext(
	ctx context.Context, planFile, branchOverride, startRef string,
) error {
	startCommit, err := s.resolveWorktreeStartCommit(startRef)
	if err != nil {
		return err
	}
	return s.preflightWorktreeForPlan(ctx, planFile, branchOverride, startCommit, startRef)
}

// PreflightWorktreeForPlanAutoCommitContext validates the tree that AutoCommitAll would
// create, so conflicts introduced by dirty source files are rejected before source mutation.
func (s *Service) PreflightWorktreeForPlanAutoCommitContext(
	ctx context.Context, planFile, branchOverride string,
) error {
	if err := s.preflightWorktreeForPlan(ctx, planFile, branchOverride, "", ""); err != nil {
		return err
	}
	branchName := s.EffectiveBranchName(s.resolveFilesystemCase(planFile), branchOverride)
	if !s.repo.branchExists(branchName) {
		return nil
	}
	wouldConflict, err := s.repo.mergeWorkingTreeWouldConflict(ctx, "refs/heads/"+branchName)
	if errors.Is(err, errMergeTreeUnsupported) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("predict auto-committed source merge into existing plan branch %q: %w", branchName, err)
	}
	if wouldConflict {
		return autoCommittedSourceConflictError(branchName)
	}
	return nil
}

func (s *Service) preflightWorktreeForPlan(
	ctx context.Context, planFile, branchOverride, existingBranchAncestor, startRef string,
) error {
	planFile = s.resolveFilesystemCase(planFile)
	branchName := s.EffectiveBranchName(planFile, branchOverride)
	if branchName == "" {
		return errors.New("plan branch name is empty")
	}
	if err := s.repo.validateBranchName(branchName); err != nil {
		return fmt.Errorf("invalid plan branch %q: %w", branchName, err)
	}
	wtPath := filepath.Join(s.repo.root(), ".loopai", "worktrees", branchName)
	if err := s.validateRuntimeDirectoryPath(filepath.Dir(wtPath)); err != nil {
		return fmt.Errorf("invalid worktree target parent: %w", err)
	}

	currentBranch, err := s.repo.currentBranch()
	if err != nil {
		return fmt.Errorf("check current branch: %w", err)
	}
	if currentBranch == branchName || (currentBranch != "" && strings.EqualFold(currentBranch, branchName)) {
		return fmt.Errorf("plan branch %q is already checked out here; switch to the source branch or run without --worktree", branchName)
	}
	if _, statErr := os.Lstat(wtPath); statErr == nil {
		return fmt.Errorf("worktree target already exists at %s", wtPath)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect worktree target %s: %w", wtPath, statErr)
	}

	worktrees, err := s.repo.worktrees()
	if err != nil {
		return fmt.Errorf("inspect registered worktrees: %w", err)
	}
	for _, worktree := range worktrees {
		if worktree.Branch != branchName && !strings.EqualFold(worktree.Branch, branchName) {
			continue
		}
		if _, statErr := os.Lstat(worktree.Path); statErr == nil {
			return fmt.Errorf("plan branch %q is already used by worktree at %s", branchName, worktree.Path)
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect registered worktree %s: %w", worktree.Path, statErr)
		}
	}

	if s.repo.branchExists(branchName) {
		if err := s.validateExistingPlanBranchAt(ctx, branchName, existingBranchAncestor, startRef); err != nil {
			return err
		}
	}
	return s.validateWorktreePlanFile(planFile)
}

func (s *Service) validateExistingPlanBranchAt(
	ctx context.Context, branchName, requiredAncestor, startRef string,
) error {
	if requiredAncestor == "" {
		var err error
		requiredAncestor, err = s.repo.headHash()
		if err != nil {
			return fmt.Errorf("identify current HEAD before reusing plan branch: %w", err)
		}
	}
	branchRef := "refs/heads/" + branchName
	containsHead, err := s.repo.isAncestor(ctx, requiredAncestor, branchRef)
	if err != nil {
		return fmt.Errorf("verify existing plan branch %q: %w", branchName, err)
	}
	if containsHead {
		return nil
	}
	wouldConflict, err := s.repo.mergeWouldConflict(ctx, branchRef, requiredAncestor)
	if errors.Is(err, errMergeTreeUnsupported) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("predict source merge into existing plan branch %q: %w", branchName, err)
	}
	if wouldConflict {
		return existingPlanBranchConflictError(branchName, startRef)
	}
	return nil
}

func existingPlanBranchConflictError(branchName, startRef string) error {
	if startRef == "" {
		return fmt.Errorf("existing plan branch %q does not include current HEAD and merging the source changes would conflict; merge or rebase the source changes into it, or choose another --branch", branchName)
	}
	return fmt.Errorf("existing plan branch %q does not include start ref %q and merging the start-ref changes would conflict; merge or rebase the start-ref changes into it, or choose another plan", branchName, startRef)
}

func autoCommittedSourceConflictError(branchName string) error {
	return fmt.Errorf("auto-committing the source changes and merging the resulting commit into existing plan branch %q would conflict; commit and merge or rebase the source changes into it manually, or choose another --branch", branchName)
}

// CommitPlanFile stages and commits a plan file on the current branch.
// mainRepoRoot is the root of the main repository, used to compute the plan file's
// relative path when the service operates inside a worktree.
// the plan file path is resolved to actual on-disk case before staging
// to handle case-insensitive filesystems (macOS APFS).
func (s *Service) CommitPlanFile(planFile, mainRepoRoot string) error {
	return s.CommitPlanFiles([]string{planFile}, mainRepoRoot)
}

// CommitPlanFiles stages and commits plan files on the current branch. Paths are interpreted
// relative to mainRepoRoot so callers operating in a linked worktree can pass source paths.
func (s *Service) CommitPlanFiles(planFiles []string, mainRepoRoot string) error {
	if len(planFiles) == 0 {
		return errors.New("commit plan files: no paths provided")
	}
	branchName, err := s.repo.currentBranch()
	if err != nil || branchName == "" {
		branchName = plan.ExtractBranchName(planFiles[0])
	}

	localPlans := make([]string, 0, len(planFiles))
	hasChanges := false
	for _, planFile := range planFiles {
		// Compute the plan file's relative path from the main repo root, then resolve it inside
		// this repo's root. The caller may be operating in a linked worktree.
		absPlan, absErr := filepath.Abs(planFile)
		if absErr != nil {
			return fmt.Errorf("resolve plan path: %w", absErr)
		}
		if resolved, evalErr := filepath.EvalSymlinks(absPlan); evalErr == nil {
			absPlan = resolved
		}
		relPlan, relErr := filepath.Rel(mainRepoRoot, absPlan)
		if relErr != nil {
			return fmt.Errorf("relative plan path: %w", relErr)
		}
		localPlan := s.resolveFilesystemCase(filepath.Join(s.repo.root(), relPlan))
		if addErr := s.repo.add(localPlan); addErr != nil {
			return fmt.Errorf("stage plan file %q: %w", planFile, addErr)
		}
		changed, changeErr := s.repo.fileHasChanges(localPlan)
		if changeErr != nil {
			return fmt.Errorf("check staged plan file %q: %w", planFile, changeErr)
		}
		hasChanges = hasChanges || changed
		localPlans = append(localPlans, localPlan)
	}
	if !hasChanges {
		if len(planFiles) == 1 {
			s.log.Printf("plan file already committed: %s\n", filepath.Base(planFiles[0]))
		} else {
			s.log.Printf("plan files already committed: %s\n", filepath.Base(planFiles[0]))
		}
		return nil
	}

	if len(planFiles) == 1 {
		s.log.Printf("committing plan file: %s\n", filepath.Base(planFiles[0]))
	} else {
		s.log.Printf("committing %d plan files\n", len(planFiles))
	}
	if err := s.repo.commitFiles(s.appendTrailer("add plan: "+branchName), localPlans...); err != nil {
		return fmt.Errorf("commit plan files: %w", err)
	}
	return nil
}

// copyToWorktree copies a file from the main repo working tree into the worktree,
// preserving its relative path from the repo root.
func (s *Service) copyToWorktree(srcPath, wtPath string) error {
	absSrc, dstDir, dstPath, err := s.resolveWorktreeCopyPaths(srcPath, wtPath)
	if err != nil {
		return err
	}
	if err := validateCopyDestination(dstPath); err != nil {
		return err
	}
	return copyFileReplacing(absSrc, dstDir, dstPath)
}

func (s *Service) resolveWorktreeCopyPaths(srcPath, wtPath string) (string, string, string, error) {
	absSrc, err := filepath.Abs(srcPath)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve source path: %w", err)
	}
	// resolve symlinks to match s.repo.root() which is also resolved (via EvalSymlinks in NewService)
	absSrc, err = filepath.EvalSymlinks(absSrc)
	if err != nil {
		return "", "", "", fmt.Errorf("eval symlinks for source: %w", err)
	}
	relPath, err := filepath.Rel(s.repo.root(), absSrc)
	if err != nil {
		return "", "", "", fmt.Errorf("relative path: %w", err)
	}
	if filepath.IsAbs(relPath) || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return "", "", "", fmt.Errorf("source plan escapes repository root: %s", srcPath)
	}

	resolvedWorktree, err := filepath.EvalSymlinks(wtPath)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve worktree path: %w", err)
	}
	dstDir, err := mkdirAllNoSymlinks(resolvedWorktree, filepath.Dir(relPath))
	if err != nil {
		return "", "", "", err
	}
	return absSrc, dstDir, filepath.Join(dstDir, filepath.Base(relPath)), nil
}

func validateCopyDestination(dstPath string) error {
	if info, statErr := os.Lstat(dstPath); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("destination plan path is a symbolic link: %s", dstPath)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("destination plan path is not a regular file: %s", dstPath)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect destination plan path: %w", statErr)
	}
	return nil
}

func copyFileReplacing(absSrc, dstDir, dstPath string) error {
	src, err := os.Open(absSrc) //nolint:gosec // validated repository-contained plan path
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	dst, err := os.CreateTemp(dstDir, ".loopai-plan-*")
	if err != nil {
		return fmt.Errorf("create temporary destination: %w", err)
	}
	tmpPath := dst.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup after errors or rename

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("copy file: %w", err)
	}
	if closeErr := dst.Close(); closeErr != nil {
		return fmt.Errorf("close copied plan: %w", closeErr)
	}
	if renameErr := os.Rename(tmpPath, dstPath); renameErr != nil {
		// Windows cannot atomically replace an existing file. The destination was lstat-validated
		// above, so remove only that regular file and retry without following a symlink.
		if removeErr := os.Remove(dstPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("replace destination plan: %w", removeErr)
		}
		if retryErr := os.Rename(tmpPath, dstPath); retryErr != nil {
			return fmt.Errorf("install copied plan: %w", retryErr)
		}
	}
	return nil
}

func writeFileReplacing(data []byte, mode os.FileMode, dstDir, dstPath string) error {
	dst, err := os.CreateTemp(dstDir, ".loopai-plan-*")
	if err != nil {
		return fmt.Errorf("create temporary destination: %w", err)
	}
	tmpPath := dst.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup after errors or rename
	if err := dst.Chmod(mode); err != nil {
		_ = dst.Close()
		return fmt.Errorf("set copied plan permissions: %w", err)
	}
	if _, err := dst.Write(data); err != nil {
		_ = dst.Close()
		return fmt.Errorf("write copied plan: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close copied plan: %w", err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		if removeErr := os.Remove(dstPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("replace destination plan: %w", removeErr)
		}
		if retryErr := os.Rename(tmpPath, dstPath); retryErr != nil {
			return fmt.Errorf("install copied plan: %w", retryErr)
		}
	}
	return nil
}

func mkdirAllNoSymlinks(root, relDir string) (string, error) {
	current := root
	for component := range strings.SplitSeq(filepath.Clean(relDir), string(filepath.Separator)) {
		if component == "." || component == "" {
			continue
		}
		if component == ".." {
			return "", fmt.Errorf("destination directory escapes worktree root: %s", relDir)
		}
		next := filepath.Join(current, component)
		info, err := os.Lstat(next)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("destination directory is a symbolic link: %s", next)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("destination path component is not a directory: %s", next)
			}
		case os.IsNotExist(err):
			if mkdirErr := os.Mkdir(next, 0o750); mkdirErr != nil {
				return "", fmt.Errorf("create destination directory: %w", mkdirErr)
			}
		default:
			return "", fmt.Errorf("inspect destination directory: %w", err)
		}
		current = next
	}
	return current, nil
}

// resolveFilesystemCase returns the path with the actual on-disk filename case.
// reads the parent directory and finds a case-insensitive match for the basename.
// falls back to the original path if the directory can't be read or no match is found.
// this handles macOS APFS case-insensitive filesystems where git tracks one case
// but the caller may provide a different case.
func (s *Service) resolveFilesystemCase(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return path
	}

	var foldMatch string
	for _, entry := range entries {
		if entry.Name() == base {
			return path // exact match, no case resolution needed
		}
		if foldMatch == "" && strings.EqualFold(entry.Name(), base) {
			foldMatch = filepath.Join(dir, entry.Name())
		}
	}
	if foldMatch != "" {
		return foldMatch
	}
	return path
}

// RemoveWorktree removes a git worktree at the given path.
// no-op if the worktree directory doesn't exist or was already removed.
func (s *Service) RemoveWorktree(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // already removed
	}
	if err := s.repo.removeWorktree(path); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	s.log.Printf("removed worktree: %s\n", path)
	return nil
}

// MovePlanToCompleted moves a plan file to the completed/ subdirectory and commits.
// Creates the completed/ directory if it doesn't exist.
// Uses git mv if the file is tracked, falls back to os.Rename for untracked files.
// If the source file doesn't exist but the destination does, logs a message and returns nil.
// Also returns nil when the source is missing and a basename with the alternate date-prefix
// convention (YYYY-MM-DD ↔ YYYYMMDD) exists in completed/, treating an LLM-driven
// date-format rename as already-moved.
// When the source is missing but a file with the alternate date-prefix exists alongside it
// (in-place rename, e.g. git mv 2026-05-12-foo.md 20260512-foo.md), the renamed file is
// used as the source so the move can complete with its current name. If an alternate-date
// copy already exists at the destination (e.g. from a prior same-day run with the same slug),
// the move is skipped so neither file is clobbered.
func (s *Service) MovePlanToCompleted(planFile string) error {
	// create completed directory
	completedDir := filepath.Join(filepath.Dir(planFile), "completed")
	if err := os.MkdirAll(completedDir, 0o750); err != nil {
		return fmt.Errorf("create completed dir: %w", err)
	}

	sourceFile, destPath, done := s.resolvePlanMoveTargets(planFile, completedDir)
	if done {
		return nil
	}

	// use git mv
	if err := s.repo.moveFile(sourceFile, destPath); err != nil {
		// fallback to regular move for untracked files
		if renameErr := os.Rename(sourceFile, destPath); renameErr != nil {
			return fmt.Errorf("move plan: %w", renameErr)
		}
		// stage the new location - log if fails but continue
		if addErr := s.repo.add(destPath); addErr != nil {
			s.log.Printf("warning: failed to stage moved plan: %v\n", addErr)
		}
	}

	// commit the move
	commitMsg := "move completed plan: " + filepath.Base(sourceFile)
	if err := s.repo.commit(s.appendTrailer(commitMsg)); err != nil {
		return fmt.Errorf("commit plan move: %w", err)
	}

	s.log.Printf("moved plan to %s\n", destPath)
	return nil
}

// resolvePlanMoveTargets determines the source and destination for MovePlanToCompleted,
// accounting for files already moved to completed/ or renamed between the dashed
// (YYYY-MM-DD) and compact (YYYYMMDD) date-prefix conventions. Returns done=true in
// two cases: the file is already in completed/ (with either basename), or there is a
// collision between an active in-place rename and a stale completed/<altBase> copy
// that the move should not clobber.
// Probe order mirrors resolvePlanFilePath in pkg/processor/prompts.go: the in-place
// alternate source is checked before any completed/ probe so a current renamed file
// wins over a stale completed/ copy left from a prior run.
func (s *Service) resolvePlanMoveTargets(planFile, completedDir string) (sourceFile, destPath string, done bool) {
	destPath = filepath.Join(completedDir, filepath.Base(planFile))
	sourceFile = planFile

	if _, err := os.Stat(planFile); !os.IsNotExist(err) {
		return sourceFile, destPath, false
	}

	altBase := plan.AltDateBasename(filepath.Base(planFile))

	// file may have been renamed in place (same dir, alt basename) — use it as source.
	// checked before completed/ probes so a current renamed file wins over a stale
	// completed/<original> copy from a prior run.
	if altBase != "" {
		altSourcePath := filepath.Join(filepath.Dir(planFile), altBase)
		if _, altSrcErr := os.Stat(altSourcePath); altSrcErr == nil {
			altDestPath := filepath.Join(completedDir, altBase)
			// collision: a stale completed/<altBase> exists alongside the active in-place
			// renamed source (e.g. same slug ran twice on the same day). git mv would refuse
			// because dest exists, and the os.Rename fallback would clobber the stale copy
			// while leaving the source's deletion unstaged — repo ends up dirty or commit
			// fails entirely. surface as already-completed instead and preserve both files
			// for manual resolution.
			if _, altDestErr := os.Stat(altDestPath); altDestErr == nil {
				s.log.Printf("plan already in completed/ (renamed: %s); active copy at %s left in place for manual cleanup\n",
					altBase, altSourcePath)
				return altSourcePath, altDestPath, true
			}
			return altSourcePath, altDestPath, false
		}
	}

	if _, destErr := os.Stat(destPath); destErr == nil {
		s.log.Printf("plan already in completed/\n")
		return sourceFile, destPath, true
	}

	if altBase == "" {
		return sourceFile, destPath, false
	}

	altDestPath := filepath.Join(completedDir, altBase)
	if _, altErr := os.Stat(altDestPath); altErr == nil {
		s.log.Printf("plan already in completed/ (renamed: %s)\n", altBase)
		return sourceFile, destPath, true
	}

	return sourceFile, destPath, false
}

// ErrInitialCommitDeclined is returned when an empty repository's initial-commit prompt is declined.
var ErrInitialCommitDeclined = errors.New("no commits - please create initial commit manually")

// EnsureHasCommits checks that the repository has at least one commit.
// If the repository is empty, calls promptFn to ask user whether to create initial commit.
// promptFn should return true to create the commit, false to abort.
// Returns error if repo is empty and user declined or promptFn returned false.
func (s *Service) EnsureHasCommits(promptFn func() bool) error {
	hasCommits, err := s.repo.hasCommits()
	if err != nil {
		return fmt.Errorf("check commits: %w", err)
	}
	if hasCommits {
		return nil
	}

	// prompt user to create initial commit
	if !promptFn() {
		return ErrInitialCommitDeclined
	}

	// create the commit
	if err := s.repo.createInitialCommit(s.appendTrailer("initial commit")); err != nil {
		return fmt.Errorf("create initial commit: %w", err)
	}
	return nil
}

// DiffStats returns change statistics between baseBranch and HEAD.
// returns zero stats if baseBranch doesn't exist or HEAD equals baseBranch.
func (s *Service) DiffStats(baseBranch string) (DiffStats, error) {
	return s.repo.diffStats(baseBranch, "HEAD")
}

// BranchDiffStats returns change statistics between baseBranch and a named branch, without
// requiring that branch to be checked out. returns zero stats for an unknown branch. the branch is
// fully qualified because Git resolves a bare name to a same-named tag first, which would silently
// report the tag's diff instead of the branch's.
func (s *Service) BranchDiffStats(baseBranch, branch string) (DiffStats, error) {
	if branch == "" {
		return DiffStats{}, errors.New("branch diff stats: empty branch name")
	}
	return s.repo.diffStats(baseBranch, "refs/heads/"+branch)
}

// EnsureLocalGitignore ensures progress and worktree artifacts are ignored without overwriting
// project-owned ignore files. Missing files get self-contained rules; custom existing files are
// preserved and repository-local excludes provide the best available fallback.
func (s *Service) EnsureLocalGitignore() error {
	loopaiDir := filepath.Join(s.repo.root(), ".loopai")
	if err := s.validateRuntimeDirectoryPath(loopaiDir); err != nil {
		return fmt.Errorf("validate .loopai dir: %w", err)
	}
	if err := os.MkdirAll(loopaiDir, 0o750); err != nil {
		return fmt.Errorf("create .loopai dir: %w", err)
	}
	runtimePaths := loopaiRuntimePaths()
	runtimeDirs := make([]string, 0, len(runtimePaths))
	runtimeExcludes := make([]string, 0, len(runtimePaths))
	var content strings.Builder
	content.WriteString(".gitignore\n")
	for _, runtimePath := range runtimePaths {
		runtimeDir, ok := strings.CutPrefix(runtimePath, ".loopai/")
		if !ok || runtimeDir == "" {
			return fmt.Errorf("invalid loopai runtime path %q", runtimePath)
		}
		runtimeDirs = append(runtimeDirs, runtimeDir)
		runtimeExcludes = append(runtimeExcludes, "/"+runtimePath+"/")
		content.WriteString(runtimeDir + "/\n")
		if err := s.validateRuntimeDirectoryPath(filepath.Join(loopaiDir, filepath.FromSlash(runtimeDir))); err != nil {
			return fmt.Errorf("validate .loopai runtime directories: %w", err)
		}
	}

	gitignorePath := filepath.Join(loopaiDir, ".gitignore")
	gitignoreContent := content.String()

	if existing, err := os.ReadFile(gitignorePath); err == nil { //nolint:gosec // .gitignore is world-readable
		if string(existing) == gitignoreContent {
			return nil
		}
		if excludeErr := s.repo.ensureRuntimeExcludes(runtimeExcludes...); excludeErr != nil {
			return fmt.Errorf("preserve custom .loopai/.gitignore: %w", excludeErr)
		}
		for _, runtimeDir := range runtimeDirs {
			if ignoreErr := ensureRuntimeDirectoryIgnored(filepath.Join(loopaiDir, filepath.FromSlash(runtimeDir))); ignoreErr != nil {
				return fmt.Errorf("preserve custom .loopai/.gitignore: %w", ignoreErr)
			}
		}
		s.log.Printf("preserved custom .loopai/.gitignore and configured repository-local runtime excludes\n")
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read .loopai/.gitignore: %w", err)
	}

	if err := os.WriteFile(gitignorePath, []byte(gitignoreContent), 0o644); err != nil { //nolint:gosec // .gitignore needs world-readable
		return fmt.Errorf("write .loopai/.gitignore: %w", err)
	}

	s.log.Printf("created .loopai/.gitignore\n")
	return nil
}

// validateRuntimeDirectoryPath rejects symlinked and non-directory components beneath the
// canonical repository root. Runtime paths are repository-controlled, so following one of their
// symlinks could otherwise make loopai create ignore files or worktrees outside the checkout.
// Missing components are safe: their nearest existing parent has already been validated.
func (s *Service) validateRuntimeDirectoryPath(path string) error {
	root := filepath.Clean(s.repo.root())
	target := filepath.Clean(path)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("resolve runtime path %s: %w", target, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("runtime path %s is outside repository %s", target, root)
	}

	current := root
	for component := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		switch {
		case os.IsNotExist(statErr):
			return nil
		case statErr != nil:
			return fmt.Errorf("inspect runtime path %s: %w", current, statErr)
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("runtime path %s is a symbolic link", current)
		case !info.IsDir():
			return fmt.Errorf("runtime path %s is not a directory", current)
		}
	}
	return nil
}

// ensureRuntimeDirectoryIgnored installs a higher-precedence ignore rule inside a runtime
// directory only when that file does not already exist. Existing files may be tracked project
// configuration and must never be rewritten by loopai.
func ensureRuntimeDirectoryIgnored(dir string) error {
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("runtime path %s is not a directory", dir)
		}
	} else if os.IsNotExist(err) {
		if mkdirErr := os.MkdirAll(dir, 0o750); mkdirErr != nil {
			return fmt.Errorf("create runtime directory %s: %w", dir, mkdirErr)
		}
	} else {
		return fmt.Errorf("inspect runtime directory %s: %w", dir, err)
	}

	ignorePath := filepath.Join(dir, ".gitignore")
	if info, err := os.Lstat(ignorePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime ignore file %s is a symbolic link", ignorePath)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect runtime ignore file %s: %w", ignorePath, err)
	}

	if _, err := os.Lstat(ignorePath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect runtime ignore file %s: %w", ignorePath, err)
	}
	if writeErr := os.WriteFile(ignorePath, []byte("*\n"), 0o600); writeErr != nil {
		return fmt.Errorf("write runtime ignore file %s: %w", ignorePath, writeErr)
	}
	return nil
}

// FileHasChanges returns true if the given file has uncommitted changes (staged or unstaged).
func (s *Service) FileHasChanges(path string) (bool, error) {
	changed, err := s.repo.fileHasChanges(path)
	if err != nil {
		return false, fmt.Errorf("file has changes %q: %w", path, err)
	}
	return changed, nil
}

// CapturePlanChainSourceState snapshots only changed plan inputs. Clean tracked plans already allow
// close-out merges; dirty tracked and untracked plans must be reconciled after a successful chain.
func (s *Service) CapturePlanChainSourceState(planFiles []string) ([]PlanSourceState, error) {
	states := make([]PlanSourceState, 0, len(planFiles))
	for _, planFile := range planFiles {
		path := planFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.repo.root(), path)
		}
		if resolvedDir, resolveErr := filepath.EvalSymlinks(filepath.Dir(path)); resolveErr == nil {
			path = filepath.Join(resolvedDir, filepath.Base(path))
		}
		changed, err := s.repo.fileHasChanges(path)
		if err != nil {
			return nil, fmt.Errorf("inspect chain source plan %q: %w", planFile, err)
		}
		if !changed {
			continue
		}
		rel, err := filepath.Rel(s.repo.root(), path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("resolve chain source plan %q inside repository", planFile)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect chain source plan %q: %w", planFile, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("chain source plan %q is not a regular file", planFile)
		}
		data, err := os.ReadFile(path) //nolint:gosec // validated regular plan path inside repository
		if err != nil {
			return nil, fmt.Errorf("read chain source plan %q: %w", planFile, err)
		}
		tracked, err := s.repo.fileTracked(path)
		if err != nil {
			return nil, fmt.Errorf("inspect chain source tracking for %q: %w", planFile, err)
		}
		gitState, err := s.repo.fileStateFingerprint(path)
		if err != nil {
			return nil, fmt.Errorf("capture chain source Git state for %q: %w", planFile, err)
		}
		digest := sha256.Sum256(data)
		states = append(states, PlanSourceState{
			Path: filepath.ToSlash(rel), Digest: hex.EncodeToString(digest[:]), GitState: gitState,
			Mode: uint32(info.Mode().Perm()), Tracked: tracked,
		})
	}
	return states, nil
}

// ReconcilePlanChainSourceState makes the source checkout merge-ready after a successful worktree
// chain. It refuses to touch any input whose contents changed since capture.
func (s *Service) ReconcilePlanChainSourceState(states []PlanSourceState) error {
	var reconcileErrs []error
	for _, state := range states {
		if err := s.reconcilePlanChainSourceState(state); err != nil {
			reconcileErrs = append(reconcileErrs, err)
		}
	}
	return errors.Join(reconcileErrs...)
}

func (s *Service) reconcilePlanChainSourceState(state PlanSourceState) error {
	path, err := s.planChainSourcePath(state.Path)
	if err != nil {
		return err
	}
	if err = s.validatePlanChainSourceParents(path); err != nil {
		return fmt.Errorf("refuse unsafe chain source plan %q: %w", state.Path, err)
	}
	if state.Tracked {
		changed, changeErr := s.repo.fileHasChanges(path)
		if changeErr != nil {
			return fmt.Errorf("inspect tracked chain source plan %q: %w", state.Path, changeErr)
		}
		if !changed {
			return nil // already restored by an earlier reconciliation attempt
		}
	}
	exists, err := s.capturedPlanChainSourceStillMatches(path, state)
	if err != nil {
		return err
	}
	if !exists {
		if state.Tracked {
			return fmt.Errorf("tracked chain source plan %q was deleted during execution; left untouched", state.Path)
		}
		return nil
	}
	if err = s.validatePlanChainSourceParents(path); err != nil {
		return fmt.Errorf("refuse unsafe chain source plan %q: %w", state.Path, err)
	}
	if state.Tracked {
		if err = s.repo.restoreFile(path); err != nil {
			return fmt.Errorf("restore tracked chain source plan %q: %w", state.Path, err)
		}
		return nil
	}
	if err = os.Remove(path); err != nil {
		return fmt.Errorf("remove consumed untracked chain source plan %q: %w", state.Path, err)
	}
	return nil
}

func (s *Service) planChainSourcePath(storedPath string) (string, error) {
	path := filepath.Join(s.repo.root(), filepath.FromSlash(storedPath))
	rel, err := filepath.Rel(s.repo.root(), path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("refuse chain source path outside repository: %q", storedPath)
	}
	return path, nil
}

func (s *Service) capturedPlanChainSourceStillMatches(path string, state PlanSourceState) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect chain source plan %q: %w", state.Path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("refuse changed non-regular chain source plan %q", state.Path)
	}
	if uint32(info.Mode().Perm()) != state.Mode {
		return false, fmt.Errorf("chain source plan %q mode changed during execution; left untouched", state.Path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is constrained to the repository root
	if err != nil {
		return false, fmt.Errorf("read chain source plan %q: %w", state.Path, err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != state.Digest {
		return false, fmt.Errorf("chain source plan %q changed during execution; left untouched", state.Path)
	}
	return true, s.validatePlanChainSourceGitState(path, state)
}

func (s *Service) validatePlanChainSourceGitState(path string, state PlanSourceState) error {
	gitState, err := s.repo.fileStateFingerprint(path)
	if err != nil {
		return fmt.Errorf("inspect chain source Git state for %q: %w", state.Path, err)
	}
	if gitState == state.GitState {
		return nil
	}
	if !state.Tracked {
		tracked, trackErr := s.repo.fileTracked(path)
		if trackErr != nil {
			return fmt.Errorf("inspect chain source tracking for %q: %w", state.Path, trackErr)
		}
		if tracked {
			return fmt.Errorf("chain source plan %q became tracked during execution; left untouched", state.Path)
		}
	}
	return fmt.Errorf("chain source plan %q Git state changed during execution; left untouched", state.Path)
}

// validatePlanChainSourceParents rejects path-topology changes before source reconciliation.
// A lexical containment check is insufficient because an intermediate symlink can redirect
// os.Remove or git restore outside the checkout.
func (s *Service) validatePlanChainSourceParents(path string) error {
	root := filepath.Clean(s.repo.root())
	parent := filepath.Dir(filepath.Clean(path))
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("parent path is outside repository")
	}
	current := root
	for component := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		switch {
		case os.IsNotExist(statErr):
			return nil
		case statErr != nil:
			return fmt.Errorf("inspect parent %s: %w", current, statErr)
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("parent %s is a symbolic link", current)
		case !info.IsDir():
			return fmt.Errorf("parent %s is not a directory", current)
		}
	}
	return nil
}

// formatDirtyFiles formats a list of dirty file paths for display in error messages.
// truncates to 10 files with "and N more" suffix.
func (s *Service) formatDirtyFiles(files []string) string {
	const maxFiles = 10
	var b strings.Builder
	for i, f := range files {
		if i >= maxFiles {
			fmt.Fprintf(&b, "  ... and %d more", len(files)-maxFiles)
			break
		}
		fmt.Fprintf(&b, "  %s\n", f)
	}
	return strings.TrimRight(b.String(), "\n")
}
