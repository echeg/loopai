package git

import (
	"context"
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
	createBranch(name string) error
	checkoutBranch(name string) error
	mergeBranch(ctx context.Context, name, expectedHead string) error
	deleteBranch(name string) error
	push(ctx context.Context, branch string) error
	worktrees() ([]Worktree, error)
	diffFingerprint() (string, error)
	isDirty() (bool, error)
	isDirtyAll() (bool, error)
	fileHasChanges(path string) (bool, error)
	hasChangesOtherThan(path string) ([]string, error)
	gitCommonDir() (string, error)
	ensureRuntimeExcludes(patterns ...string) error
	add(path string) error
	moveFile(src, dst string) error
	commit(msg string) error
	commitFiles(msg string, paths ...string) error
	autoCommitAll(msg string) (bool, error)
	createInitialCommit(msg string) error
	diffStats(baseBranch string) (DiffStats, error)
	addWorktree(path, branch string, createBranch bool) error
	removeWorktree(path string) error
	removeWorktreeSafe(path string) error
	pruneWorktrees() error
	isAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
}

// ErrMergeConflict identifies a merge that could not be completed because of conflicts.
// The repository is returned to its pre-merge state before this error is returned.
var ErrMergeConflict = errors.New("merge conflict")

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
	opened.trailer = s.trailer
	return opened, nil
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
	commonDir, err := s.repo.gitCommonDir()
	if err != nil {
		return nil, fmt.Errorf("locate repository lock directory: %w", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(commonDir, "loopai-worktree-create.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // commonDir is resolved by Git
	if err != nil {
		return nil, fmt.Errorf("open worktree creation lock: %w", err)
	}
	if err := lockRepositoryFile(lockFile); err != nil {
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

// EffectiveBranchName returns branchOverride when set, otherwise derives the branch name from planFile.
func (s *Service) EffectiveBranchName(planFile, branchOverride string) string {
	if branchOverride != "" {
		return branchOverride
	}
	return plan.ExtractBranchName(planFile)
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

// CreateWorktreeForPlan creates an isolated git worktree for plan execution from the current HEAD.
// It derives the branch name from the plan file and creates the worktree at
// .loopai/worktrees/<branch>.
// returns (worktree path, planNeedsCommit, error). when planNeedsCommit is true the caller
// must commit the plan file in the worktree context (via CommitPlanFile on the worktree's
// git service) so the commit lands on the feature branch rather than the default branch.
// branchOverride, when non-empty, is used directly instead of deriving the name from planFile.
func (s *Service) CreateWorktreeForPlan(planFile, branchOverride string) (string, bool, error) {
	planFile = s.resolveFilesystemCase(planFile)

	// prune stale worktree entries first
	if pruneErr := s.repo.pruneWorktrees(); pruneErr != nil {
		s.log.Printf("warning: prune worktrees: %v\n", pruneErr)
	}

	if err := s.PreflightWorktreeForPlan(planFile, branchOverride); err != nil {
		return "", false, err
	}
	branchName := s.EffectiveBranchName(planFile, branchOverride)
	wtPath := filepath.Join(s.repo.root(), ".loopai", "worktrees", branchName)

	_, planHasChanges, err := s.prepareWorktreePlan(planFile, branchOverride)
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
	if s.repo.branchExists(branchName) {
		if err := s.validateExistingPlanBranch(branchName); err != nil {
			return "", false, err
		}
		s.log.Printf("creating worktree with existing branch: %s\n", branchName)
		if err := s.repo.addWorktree(wtPath, branchName, false); err != nil {
			return "", false, fmt.Errorf("add worktree with existing branch: %w", err)
		}
	} else {
		s.log.Printf("creating worktree with new branch: %s (from %s)\n", branchName, source)
		if err := s.repo.addWorktree(wtPath, branchName, true); err != nil {
			return "", false, fmt.Errorf("add worktree with new branch: %w", err)
		}
	}

	// copy plan file into worktree so the caller can commit it on the feature branch.
	// without this, the plan file only exists in main's working tree (not committed to HEAD).
	if planHasChanges {
		if cpErr := s.copyToWorktree(planFile, wtPath); cpErr != nil {
			_ = s.repo.removeWorktree(wtPath)
			return "", false, fmt.Errorf("copy plan to worktree: %w", cpErr)
		}
	}

	return wtPath, planHasChanges, nil
}

// PreflightWorktreeForPlan rejects deterministic target conflicts without changing repository
// state. Callers that may mutate the source checkout must run this while holding the repository
// lock and before installing runtime ignores or creating an auto-commit.
func (s *Service) PreflightWorktreeForPlan(planFile, branchOverride string) error {
	planFile = s.resolveFilesystemCase(planFile)
	branchName := s.EffectiveBranchName(planFile, branchOverride)
	if branchName == "" {
		return errors.New("plan branch name is empty")
	}
	if err := s.repo.validateBranchName(branchName); err != nil {
		return fmt.Errorf("invalid plan branch %q: %w", branchName, err)
	}
	wtPath := filepath.Join(s.repo.root(), ".loopai", "worktrees", branchName)

	currentBranch, err := s.repo.currentBranch()
	if err != nil {
		return fmt.Errorf("check current branch: %w", err)
	}
	if currentBranch == branchName {
		return fmt.Errorf("plan branch %q is already checked out here; switch to the source branch or run without --worktree", branchName)
	}
	if _, statErr := os.Stat(wtPath); statErr == nil {
		return fmt.Errorf("worktree already exists at %s, another instance may be running", wtPath)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect worktree target %s: %w", wtPath, statErr)
	}

	worktrees, err := s.repo.worktrees()
	if err != nil {
		return fmt.Errorf("inspect registered worktrees: %w", err)
	}
	for _, worktree := range worktrees {
		if worktree.Branch != branchName {
			continue
		}
		if _, statErr := os.Stat(worktree.Path); statErr == nil {
			return fmt.Errorf("plan branch %q is already used by worktree at %s", branchName, worktree.Path)
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect registered worktree %s: %w", worktree.Path, statErr)
		}
	}

	if s.repo.branchExists(branchName) {
		return s.validateExistingPlanBranch(branchName)
	}
	return nil
}

// ValidateWorktreeAutoCommit rejects a dirty source when the target branch already exists.
// Committing would advance the source HEAD beyond that branch and make safe reuse impossible.
func (s *Service) ValidateWorktreeAutoCommit(planFile, branchOverride string) error {
	branchName := s.EffectiveBranchName(planFile, branchOverride)
	if !s.repo.branchExists(branchName) {
		return nil
	}
	dirty, err := s.repo.isDirtyAll()
	if err != nil {
		return fmt.Errorf("check source before reusing plan branch %q: %w", branchName, err)
	}
	if !dirty {
		return nil
	}
	return fmt.Errorf("cannot auto-commit before reusing existing plan branch %q; commit the source changes first, then merge or rebase them into the plan branch, or choose another --branch", branchName)
}

func (s *Service) validateExistingPlanBranch(branchName string) error {
	head, err := s.repo.headHash()
	if err != nil {
		return fmt.Errorf("identify current HEAD before reusing plan branch: %w", err)
	}
	containsHead, err := s.repo.isAncestor(context.Background(), head, "refs/heads/"+branchName)
	if err != nil {
		return fmt.Errorf("verify existing plan branch %q: %w", branchName, err)
	}
	if !containsHead {
		return fmt.Errorf("existing plan branch %q does not include current HEAD; merge or rebase the source changes into it, or choose another --branch", branchName)
	}
	return nil
}

// CommitPlanFile stages and commits a plan file on the current branch.
// mainRepoRoot is the root of the main repository, used to compute the plan file's
// relative path when the service operates inside a worktree.
// the plan file path is resolved to actual on-disk case before staging
// to handle case-insensitive filesystems (macOS APFS).
func (s *Service) CommitPlanFile(planFile, mainRepoRoot string) error {
	branchName, err := s.repo.currentBranch()
	if err != nil || branchName == "" {
		branchName = plan.ExtractBranchName(planFile)
	}

	// compute the plan file's relative path from the main repo root, then resolve
	// it inside this repo's root. this is needed because planFile is absolute and
	// may point to the main repo's working tree, which is outside the worktree.
	absPlan, err := filepath.Abs(planFile)
	if err != nil {
		return fmt.Errorf("resolve plan path: %w", err)
	}
	// resolve symlinks so both paths use the same prefix (macOS /var -> /private/var)
	if resolved, evalErr := filepath.EvalSymlinks(absPlan); evalErr == nil {
		absPlan = resolved
	}
	relPlan, err := filepath.Rel(mainRepoRoot, absPlan)
	if err != nil {
		return fmt.Errorf("relative plan path: %w", err)
	}
	localPlan := filepath.Join(s.repo.root(), relPlan)
	localPlan = s.resolveFilesystemCase(localPlan)

	if addErr := s.repo.add(localPlan); addErr != nil {
		return fmt.Errorf("stage plan file: %w", addErr)
	}
	hasChanges, err := s.repo.fileHasChanges(localPlan)
	if err != nil {
		return fmt.Errorf("check staged plan file: %w", err)
	}
	if !hasChanges {
		s.log.Printf("plan file already committed: %s\n", filepath.Base(planFile))
		return nil
	}

	s.log.Printf("committing plan file: %s\n", filepath.Base(planFile))
	if err := s.repo.commit(s.appendTrailer("add plan: " + branchName)); err != nil {
		return fmt.Errorf("commit plan file: %w", err)
	}
	return nil
}

// copyToWorktree copies a file from the main repo working tree into the worktree,
// preserving its relative path from the repo root.
func (s *Service) copyToWorktree(srcPath, wtPath string) error {
	absSrc, err := filepath.Abs(srcPath)
	if err != nil {
		return fmt.Errorf("resolve source path: %w", err)
	}
	// resolve symlinks to match s.repo.root() which is also resolved (via EvalSymlinks in NewService)
	absSrc, err = filepath.EvalSymlinks(absSrc)
	if err != nil {
		return fmt.Errorf("eval symlinks for source: %w", err)
	}
	relPath, err := filepath.Rel(s.repo.root(), absSrc)
	if err != nil {
		return fmt.Errorf("relative path: %w", err)
	}

	dstPath := filepath.Join(wtPath, relPath)
	if err = os.MkdirAll(filepath.Dir(dstPath), 0o750); err != nil {
		return fmt.Errorf("create directories: %w", err)
	}

	src, err := os.Open(absSrc)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(dstPath) //nolint:gosec // plan file doesn't need restricted perms
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy file: %w", err)
	}
	return nil
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
		return errors.New("no commits - please create initial commit manually")
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
	return s.repo.diffStats(baseBranch)
}

// EnsureLocalGitignore ensures progress and worktree artifacts are ignored without overwriting
// a project-owned .loopai/.gitignore. A missing file gets the standard self-contained rules;
// a custom existing file is preserved and the rules are added to Git's repository-local excludes.
func (s *Service) EnsureLocalGitignore() error {
	loopaiDir := filepath.Join(s.repo.root(), ".loopai")
	if err := os.MkdirAll(loopaiDir, 0o750); err != nil {
		return fmt.Errorf("create .loopai dir: %w", err)
	}

	gitignorePath := filepath.Join(loopaiDir, ".gitignore")
	const content = ".gitignore\nprogress/\nworktrees/\n"

	if existing, err := os.ReadFile(gitignorePath); err == nil { //nolint:gosec // .gitignore is world-readable
		if string(existing) == content {
			return nil
		}
		if excludeErr := s.repo.ensureRuntimeExcludes("/.loopai/progress/", "/.loopai/worktrees/"); excludeErr != nil {
			return fmt.Errorf("preserve custom .loopai/.gitignore: %w", excludeErr)
		}
		s.log.Printf("preserved custom .loopai/.gitignore and configured repository-local runtime excludes\n")
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read .loopai/.gitignore: %w", err)
	}

	if err := os.WriteFile(gitignorePath, []byte(content), 0o644); err != nil { //nolint:gosec // .gitignore needs world-readable
		return fmt.Errorf("write .loopai/.gitignore: %w", err)
	}

	s.log.Printf("created .loopai/.gitignore\n")
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
