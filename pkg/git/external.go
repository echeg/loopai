package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	mergeCleanupTimeout = 5 * time.Second
	commandWaitDelay    = 250 * time.Millisecond
)

type commandCancellation uint8

const (
	commandCancellationNone commandCancellation = iota
	commandCancellationDirect
	commandCancellationGroup
)

// externalBackend implements the backend interface by shelling out to the git CLI.
type externalBackend struct {
	path    string // absolute path to repository root
	command string // vcs command to use (default: "git")
}

// newExternalBackend creates an externalBackend that shells out to the given vcs command.
// validates the path is inside a repository using rev-parse.
func newExternalBackend(path, command string) (*externalBackend, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	// validate path is a repo and get the toplevel
	cmd := exec.CommandContext(context.Background(), command, "rev-parse", "--show-toplevel")
	cmd.Dir = absPath
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("open git repository %s: %s", absPath, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("open git repository %s: %w", absPath, err)
	}

	root := strings.TrimSpace(string(out))

	// resolve symlinks for consistent path comparison (macOS /var -> /private/var)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("eval symlinks: %w", err)
	}

	return &externalBackend{path: root, command: command}, nil
}

// run executes a git command and returns combined stdout+stderr with trailing whitespace removed.
// leading whitespace is preserved (important for porcelain format parsing).
// on failure, returns error with the combined output for diagnostics.
func (e *externalBackend) run(args ...string) (string, error) {
	return e.runCommand(context.Background(), commandCancellationNone, args...)
}

func (e *externalBackend) runContext(ctx context.Context, args ...string) (string, error) {
	return e.runCommand(ctx, commandCancellationGroup, args...)
}

func (e *externalBackend) runContextWithEnv(ctx context.Context, env []string, args ...string) (string, error) {
	return e.runCommandWithEnv(ctx, commandCancellationGroup, env, args...)
}

func (e *externalBackend) runContextWithTerminal(ctx context.Context, args ...string) (string, error) {
	return e.runCommand(ctx, commandCancellationDirect, args...)
}

func configureDirectCommandCancellation(cmd *exec.Cmd) {
	// Keep Git in the caller's session so terminal credential prompts remain available.
	// WaitDelay still bounds waits on pipes inherited by a credential helper after cancel.
	cmd.WaitDelay = commandWaitDelay
}

func (e *externalBackend) runCommand(ctx context.Context, cancellation commandCancellation, args ...string) (string, error) {
	return e.runCommandWithEnv(ctx, cancellation, nil, args...)
}

func (e *externalBackend) runCommandWithEnv(
	ctx context.Context, cancellation commandCancellation, env []string, args ...string,
) (string, error) {
	cmd := exec.CommandContext(ctx, e.command, args...)
	cmd.Dir = e.path
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	switch cancellation {
	case commandCancellationNone:
	case commandCancellationDirect:
		configureDirectCommandCancellation(cmd)
	case commandCancellationGroup:
		configureCommandCancellation(cmd)
	}
	// Capture stdout and stderr together so command failures retain Git's diagnostics.
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	out := output.Bytes()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("%s %s: %s", e.command, args[0], msg)
		}
		return "", fmt.Errorf("%s %s: %w", e.command, args[0], err)
	}
	return strings.TrimRight(string(out), " \t\n\r"), nil
}

// compile-time check: externalBackend must satisfy the backend interface
var _ backend = (*externalBackend)(nil)

// root returns the absolute path to the repository root.
func (e *externalBackend) root() string {
	return e.path
}

func (e *externalBackend) gitCommonDir() (string, error) {
	out, err := e.run("rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("get Git common directory: %w", err)
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(e.path, out)
	}
	return filepath.Clean(out), nil
}

func (e *externalBackend) ensureRuntimeExcludes(patterns ...string) error {
	out, err := e.run("rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return fmt.Errorf("locate Git exclude file: %w", err)
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(e.path, out)
	}
	excludePath := filepath.Clean(out)
	data, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read Git exclude file: %w", err)
	}
	existing := make(map[string]struct{})
	for line := range strings.SplitSeq(string(data), "\n") {
		existing[strings.TrimSpace(line)] = struct{}{}
	}
	missing := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if _, ok := existing[pattern]; !ok {
			missing = append(missing, pattern)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(excludePath), 0o750); mkdirErr != nil {
		return fmt.Errorf("create Git info directory: %w", mkdirErr)
	}
	f, err := os.OpenFile(excludePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open Git exclude file: %w", err)
	}
	prefix := ""
	if len(data) > 0 && data[len(data)-1] != '\n' {
		prefix = "\n"
	}
	_, writeErr := fmt.Fprintf(f, "%s%s\n", prefix, strings.Join(missing, "\n"))
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write Git exclude file: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close Git exclude file: %w", closeErr)
	}
	return nil
}

// headHash returns the current HEAD commit hash.
func (e *externalBackend) headHash() (string, error) {
	out, err := e.run("rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("get HEAD: %w", err)
	}
	return out, nil
}

// revParse resolves any revision to its commit hash.
func (e *externalBackend) revParse(ref string) (string, error) {
	out, err := e.run("rev-parse", ref)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", ref, err)
	}
	return out, nil
}

// diffFingerprint returns a sha256 hash of the working tree state (tracked diffs + untracked file content).
// includes untracked file content hashes so that edits to existing untracked files are detected,
// not just new file creation.
func (e *externalBackend) diffFingerprint() (string, error) {
	out, err := e.run("diff", "HEAD")
	if err != nil {
		return "", fmt.Errorf("diff fingerprint: %w", err)
	}
	untracked, err := e.run("ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return "", fmt.Errorf("diff fingerprint (untracked): %w", err)
	}

	h := sha256.New()
	h.Write([]byte(out))
	h.Write([]byte{0})

	// for each untracked file, include both name and content hash so that edits
	// to existing untracked files change the fingerprint (not just new file creation).
	// -z flag produces null-terminated output, safe for filenames with special characters
	if untracked != "" {
		for name := range strings.SplitSeq(untracked, "\x00") {
			if name == "" {
				continue
			}
			h.Write([]byte(name))
			h.Write([]byte{0})
			// git hash-object computes blob hash from file content
			if blobHash, hashErr := e.run("hash-object", "--", name); hashErr == nil {
				h.Write([]byte(blobHash))
			}
			h.Write([]byte{0})
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// hasCommits returns true if the repository has at least one commit.
func (e *externalBackend) hasCommits() (bool, error) {
	cmd := exec.CommandContext(context.Background(), e.command, "rev-parse", "HEAD")
	cmd.Dir = e.path
	cmd.Env = append(os.Environ(), "LC_ALL=C") // force English stderr for reliable parsing
	if _, err := cmd.Output(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
			// git outputs "ambiguous argument 'HEAD'" when HEAD doesn't exist (empty repo);
			// other exit-128 causes (corruption, permission errors) propagate as errors.
			// note: must use cmd.Output() (not cmd.Run()) so ExitError.Stderr is populated.
			stderr := strings.ToLower(string(exitErr.Stderr))
			if strings.Contains(stderr, "ambiguous argument") {
				return false, nil // no commits (empty repo, HEAD not found)
			}
			return false, fmt.Errorf("check HEAD: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return false, fmt.Errorf("check HEAD: %w", err) // unexpected exit code or exec failure
	}
	return true, nil
}

// currentBranch returns the name of the current branch, or empty string for detached HEAD.
func (e *externalBackend) currentBranch() (string, error) {
	return e.currentBranchContext(context.Background())
}

func (e *externalBackend) currentBranchContext(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, e.command, "symbolic-ref", "HEAD")
	cmd.Dir = e.path
	cmd.Env = append(os.Environ(), "LC_ALL=C") // force English stderr for reliable parsing
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
			// only treat as "detached HEAD" when stderr indicates symbolic-ref failure;
			// other exit-128 causes (corruption, permission errors) should propagate as errors
			stderr := strings.ToLower(string(exitErr.Stderr))
			if strings.Contains(stderr, "not a symbolic ref") {
				return "", nil // detached HEAD (symbolic-ref fails when not on a branch)
			}
			return "", fmt.Errorf("get current branch: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("get current branch: %w", err) // unexpected exit code or exec failure
	}
	ref := strings.TrimSpace(string(out))
	const headsPrefix = "refs/heads/"
	if !strings.HasPrefix(ref, headsPrefix) {
		return "", fmt.Errorf("get current branch: HEAD points to unexpected symbolic ref %q", ref)
	}
	return strings.TrimPrefix(ref, headsPrefix), nil
}

func (e *externalBackend) originURL() (string, error) {
	out, err := e.run("config", "--get", "remote.origin.url")
	if err != nil {
		return "", fmt.Errorf("get origin URL: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return "", errors.New("origin URL is empty")
	}
	return strings.TrimSpace(out), nil
}

func (e *externalBackend) originPushURLs() ([]string, error) {
	out, err := e.run("remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return nil, fmt.Errorf("get effective origin push URLs: %w", err)
	}
	var urls []string
	for line := range strings.SplitSeq(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			urls = append(urls, trimmed)
		}
	}
	if len(urls) == 0 {
		return nil, errors.New("effective origin push URL is empty")
	}
	return urls, nil
}

// getDefaultBranch returns the default branch name.
// detects from origin/HEAD symbolic reference, falls back to checking common branch names.
func (e *externalBackend) getDefaultBranch() string {
	// try origin/HEAD first
	cmd := exec.CommandContext(context.Background(), e.command, "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = e.path
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		// ref is like "refs/remotes/origin/main"
		if strings.HasPrefix(ref, "refs/remotes/origin/") {
			branchName := ref[len("refs/remotes/origin/"):]

			// check if local branch exists
			if e.refExists("refs/heads/" + branchName) {
				return branchName
			}
			// local branch doesn't exist, return remote-tracking ref
			return "origin/" + branchName
		}
	}

	// fallback: check which common branch names exist
	for _, name := range []string{"main", "master", "trunk", "develop"} {
		if e.refExists("refs/heads/" + name) {
			return name
		}
	}

	return "master"
}

// validateBranchName checks a prospective local branch name without changing repository state.
func (e *externalBackend) validateBranchName(name string) error {
	canonical, err := e.run("check-ref-format", "--branch", name)
	if err != nil {
		return fmt.Errorf("check branch name: %w", err)
	}
	// --branch expands reflog shorthand such as @{-1}. Those expressions are valid
	// inputs to Git, but they are not literal branch names and would make later
	// existence checks and worktree creation operate on different refs.
	if canonical != name {
		return fmt.Errorf("check branch name: resolves to %q instead of naming a literal branch", canonical)
	}
	return nil
}

// branchExists checks if a branch with the given name exists.
func (e *externalBackend) branchExists(name string) bool {
	return e.refExists("refs/heads/" + name)
}

// branchHash returns the commit hash a local branch points at.
func (e *externalBackend) branchHash(name string) (string, error) {
	out, err := e.run("rev-parse", "--verify", "refs/heads/"+name)
	if err != nil {
		return "", fmt.Errorf("get branch %q head: %w", name, err)
	}
	return out, nil
}

// createBranch creates a new branch and switches to it.
func (e *externalBackend) createBranch(name string) error {
	_, err := e.run("checkout", "-b", name)
	if err != nil {
		return fmt.Errorf("create branch: %w", err)
	}
	return nil
}

// checkoutBranch switches to an existing branch.
func (e *externalBackend) checkoutBranch(name string) error {
	args := []string{"switch", "--no-overwrite-ignore"}
	if !e.branchExists(name) {
		args = append(args, "--detach")
	}
	_, err := e.run(append(args, "--", name)...)
	if err != nil {
		return fmt.Errorf("checkout branch: %w", err)
	}
	return nil
}

// mergeBranch merges the named branch into the current HEAD. Command-line overrides prevent the
// current branch's mergeOptions from silently changing close-out semantics. Any merge left in
// progress is aborted; ErrMergeConflict is returned only when unmerged paths exist. A nominally
// successful merge is rolled back unless it incorporates expectedHead.
func (e *externalBackend) mergeBranch(ctx context.Context, name, expectedHead string) error {
	return e.mergeRevision(ctx, "refs/heads/"+name, expectedHead)
}

func (e *externalBackend) mergeRevision(ctx context.Context, revision, expectedHead string) error {
	preMergeHead, err := e.headHash()
	if err != nil {
		return fmt.Errorf("read pre-merge HEAD: %w", err)
	}
	currentBranch, err := e.currentBranchContext(ctx)
	if err != nil {
		return fmt.Errorf("read current branch before merge: %w", err)
	}
	mergeArgs := []string{"merge", "--commit", "--no-squash", "--no-overwrite-ignore", revision}
	var mergeEnv []string
	if currentBranch != "" {
		// Branch mergeOptions can select a strategy such as "ours", which creates a merge
		// commit and passes the ancestry check while discarding the feature tree. Clear the
		// option at command scope so close-out always uses Git's ordinary merge semantics.
		// GIT_CONFIG_PARAMETERS avoids -c's first-'=' ambiguity for valid names such as a=b.
		configKey := "branch." + currentBranch + ".mergeOptions"
		configOverride := quoteGitConfigParameter(configKey) + "=" + quoteGitConfigParameter("")
		if inherited := os.Getenv("GIT_CONFIG_PARAMETERS"); inherited != "" {
			configOverride = inherited + " " + configOverride
		}
		mergeEnv = []string{"GIT_CONFIG_PARAMETERS=" + configOverride}
	}
	_, err = e.runContextWithEnv(ctx, mergeEnv, mergeArgs...)
	if err == nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), mergeCleanupTimeout)
		defer cancel()
		incorporated, verifyErr := e.isAncestor(cleanupCtx, expectedHead, "HEAD")
		if verifyErr == nil && incorporated {
			return nil
		}
		if verifyErr != nil {
			return e.rollbackSuccessfulMerge(cleanupCtx, preMergeHead,
				fmt.Errorf("verify merged commit %q: %w", expectedHead, verifyErr))
		}
		return e.rollbackSuccessfulMerge(cleanupCtx, preMergeHead,
			fmt.Errorf("merge completed without incorporating expected commit %q", expectedHead))
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), mergeCleanupTimeout)
	defer cancel()
	postMergeHead, inspectHeadErr := e.runContext(cleanupCtx, "rev-parse", "HEAD")
	if inspectHeadErr == nil && postMergeHead != preMergeHead {
		return e.rollbackSuccessfulMerge(cleanupCtx, preMergeHead, fmt.Errorf("merge: %w", err))
	}
	mergeHead := exec.CommandContext(cleanupCtx, e.command, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	mergeHead.Dir = e.path
	if mergeHead.Run() == nil {
		return e.abortFailedMerge(cleanupCtx, err)
	}
	if inspectHeadErr != nil {
		return errors.Join(fmt.Errorf("merge: %w", err), fmt.Errorf("inspect HEAD after failed merge: %w", inspectHeadErr))
	}
	return fmt.Errorf("merge: %w", err)
}

func (e *externalBackend) isAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	cmd := exec.CommandContext(ctx, e.command, "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = e.path
	configureCommandCancellation(cmd)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("git merge-base: %w", ctxErr)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	details := strings.TrimSpace(output.String())
	if details != "" {
		return false, fmt.Errorf("git merge-base: %s", details)
	}
	return false, fmt.Errorf("git merge-base: %w", err)
}

// mergeWouldConflict predicts whether merging branch into base would conflict without
// changing the index or a worktree. Git 2.38 added the --write-tree form; callers may
// fall back to attempting the real merge when an older Git reports it as unsupported.
func (e *externalBackend) mergeWouldConflict(ctx context.Context, base, branch string) (bool, error) {
	cmd := exec.CommandContext(ctx, e.command, "merge-tree", "--write-tree", base, branch)
	cmd.Dir = e.path
	cmd.Env = append(os.Environ(), "LC_ALL=C") // force English stderr for reliable parsing
	configureCommandCancellation(cmd)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("git merge-tree: %w", ctxErr)
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}

	details := strings.TrimSpace(output.String())
	if mergeTreeWriteTreeUnsupported(details) {
		return false, errMergeTreeUnsupported
	}
	if details != "" {
		return false, fmt.Errorf("git merge-tree: %s", details)
	}
	return false, fmt.Errorf("git merge-tree: %w", err)
}

// mergeWorkingTreeWouldConflict predicts the merge using the tree AutoCommitAll would
// commit. A temporary index lets git add -A model that commit without changing the real
// index, worktree, or source ref.
func (e *externalBackend) mergeWorkingTreeWouldConflict(ctx context.Context, base string) (bool, error) {
	indexFile, err := os.CreateTemp("", "ralphex-merge-index-*")
	if err != nil {
		return false, fmt.Errorf("create temporary Git index: %w", err)
	}
	indexPath := indexFile.Name()
	if closeErr := indexFile.Close(); closeErr != nil {
		_ = os.Remove(indexPath)
		return false, fmt.Errorf("close temporary Git index: %w", closeErr)
	}
	if removeErr := os.Remove(indexPath); removeErr != nil {
		return false, fmt.Errorf("prepare temporary Git index: %w", removeErr)
	}
	defer os.Remove(indexPath)           //nolint:errcheck // best-effort cleanup of an isolated temporary file
	defer os.Remove(indexPath + ".lock") //nolint:errcheck // cancellation can leave the temporary index lock behind

	env := []string{"GIT_INDEX_FILE=" + indexPath}
	run := func(args ...string) (string, error) {
		out, runErr := e.runContextWithEnv(ctx, env, args...)
		if runErr != nil && ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		return out, runErr
	}
	if _, err = run("read-tree", "HEAD"); err != nil {
		return false, fmt.Errorf("initialize temporary Git index: %w", err)
	}
	if _, err = run("add", "-A"); err != nil {
		return false, fmt.Errorf("stage source snapshot in temporary Git index: %w", err)
	}
	if _, err = run("reset", "--quiet", "HEAD", "--", ".loopai/progress", ".loopai/worktrees"); err != nil {
		return false, fmt.Errorf("exclude runtime artifacts from source snapshot: %w", err)
	}
	tree, err := run("write-tree")
	if err != nil {
		return false, fmt.Errorf("write source snapshot tree: %w", err)
	}
	headTree, err := run("rev-parse", "HEAD^{tree}")
	if err != nil {
		return false, fmt.Errorf("identify committed source tree: %w", err)
	}
	if tree == headTree {
		return e.mergeWouldConflict(ctx, base, "HEAD")
	}
	snapshot, err := run("commit-tree", tree, "-p", "HEAD", "-m", "loopai preflight source snapshot")
	if err != nil {
		return false, fmt.Errorf("create source snapshot commit: %w", err)
	}
	return e.mergeWouldConflict(ctx, base, snapshot)
}

func quoteGitConfigParameter(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func mergeTreeWriteTreeUnsupported(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "write-tree") &&
		(strings.Contains(lower, "unknown option") || strings.Contains(lower, "unknown switch") ||
			strings.Contains(lower, "unknown rev") || strings.Contains(lower, "not a valid object name"))
}

func (e *externalBackend) rollbackSuccessfulMerge(ctx context.Context, preMergeHead string, cause error) error {
	if _, err := e.runContext(ctx, "reset", "--hard", preMergeHead); err != nil {
		return fmt.Errorf("%w; additionally failed to restore pre-merge HEAD %q: %w", cause, preMergeHead, err)
	}
	return cause
}

func (e *externalBackend) abortFailedMerge(ctx context.Context, mergeErr error) error {
	unmerged, inspectErr := e.runContext(ctx, "ls-files", "-u")
	_, abortErr := e.runContext(ctx, "merge", "--abort")
	switch {
	case inspectErr != nil && abortErr != nil:
		return fmt.Errorf("merge: %w (inspect unmerged paths: %w; abort failed: %w)", mergeErr, inspectErr, abortErr)
	case inspectErr != nil:
		return fmt.Errorf("merge: %w (inspect unmerged paths: %w)", mergeErr, inspectErr)
	case unmerged != "" && abortErr != nil:
		return fmt.Errorf("%w: %w (abort failed: %w)", ErrMergeConflict, mergeErr, abortErr)
	case unmerged != "":
		return fmt.Errorf("%w: %w", ErrMergeConflict, mergeErr)
	case abortErr != nil:
		return fmt.Errorf("merge: %w (abort failed: %w)", mergeErr, abortErr)
	default:
		return fmt.Errorf("merge: %w", mergeErr)
	}
}

// deleteBranch safely deletes a local branch, refusing unmerged branches.
func (e *externalBackend) deleteBranch(name string) error {
	if _, err := e.run("branch", "-d", "--", name); err != nil {
		return fmt.Errorf("delete branch: %w", err)
	}
	return nil
}

// push pushes a branch to origin and records the upstream relationship.
func (e *externalBackend) push(ctx context.Context, branch string) error {
	ref := "refs/heads/" + branch
	if _, err := e.runContext(ctx, "check-ref-format", ref); err != nil {
		return fmt.Errorf("validate branch ref: %w", err)
	}
	if _, err := e.runContextWithTerminal(ctx, "push", "-u", "origin", ref+":"+ref); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// worktrees returns registered worktrees in Git's stable porcelain format.
func (e *externalBackend) worktrees() ([]Worktree, error) {
	out, err := e.run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}

	var result []Worktree
	var current Worktree
	flush := func() {
		if current.Path != "" {
			result = append(result, current)
		}
		current = Worktree{}
	}
	for line := range strings.SplitSeq(out+"\n", "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			current.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	return result, nil
}

// isDirty returns true if the worktree has uncommitted changes (staged or modified tracked files).
func (e *externalBackend) isDirty() (bool, error) {
	out, err := e.run("status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("get status: %w", err)
	}
	if out == "" {
		return false, nil
	}

	// check each line - only count tracked changes (not untracked files marked with ??)
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		// untracked files (lines starting with "??") don't count as dirty
		if strings.HasPrefix(line, "??") {
			continue
		}
		return true, nil
	}
	return false, nil
}

// isDirtyAll reports every porcelain entry, including untracked files.
func (e *externalBackend) isDirtyAll() (bool, error) {
	out, err := e.run("status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("get status: %w", err)
	}
	return out != "", nil
}

// fileHasChanges returns true if the given file has uncommitted changes.
func (e *externalBackend) fileHasChanges(path string) (bool, error) {
	rel, err := e.toRelative(path)
	if err != nil {
		return false, err
	}

	// use -uall to list individual files, not collapsed directories
	out, err := e.run("status", "--porcelain", "-uall", "--", rel)
	if err != nil {
		return false, fmt.Errorf("check file status: %w", err)
	}
	return out != "", nil
}

// fileTracked reports whether path is present in the Git index.
func (e *externalBackend) fileTracked(path string) (bool, error) {
	rel, err := e.toRelative(path)
	if err != nil {
		return false, err
	}
	out, err := e.run("ls-files", "-z", "--cached", "--", rel)
	if err != nil {
		return false, fmt.Errorf("check tracked file: %w", err)
	}
	return out != "", nil
}

// hasChangesOtherThan returns the list of dirty file paths (excluding the given file, case-insensitive).
// this includes modified/deleted tracked files, staged changes, and untracked files (excluding gitignored).
// an empty slice means no other changes.
func (e *externalBackend) hasChangesOtherThan(path string) ([]string, error) {
	rel, err := e.toRelative(path)
	if err != nil {
		return nil, err
	}

	// use -uall to list individual files, not collapsed directories
	out, err := e.run("status", "--porcelain", "-uall")
	if err != nil {
		return nil, fmt.Errorf("get status: %w", err)
	}

	if out == "" {
		return nil, nil
	}

	// one entry per porcelain line at most, some are skipped below
	dirty := make([]string, 0, strings.Count(out, "\n")+1)
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		// extract file path from porcelain output: "XY path" or "XY path -> newpath"
		filePath := e.extractPathFromPorcelain(line)
		if strings.EqualFold(filePath, rel) || isLoopaiRuntimePath(filePath) {
			continue
		}
		dirty = append(dirty, filePath)
	}
	return dirty, nil
}

func isLoopaiRuntimePath(path string) bool {
	path = filepath.ToSlash(path)
	return path == ".loopai/progress" || strings.HasPrefix(path, ".loopai/progress/") ||
		path == ".loopai/worktrees" || strings.HasPrefix(path, ".loopai/worktrees/")
}

// add stages a file for commit.
func (e *externalBackend) add(path string) error {
	rel, err := e.toRelative(path)
	if err != nil {
		return err
	}
	_, err = e.run("add", "--", rel)
	if err != nil {
		return fmt.Errorf("add file: %w", err)
	}
	return nil
}

// moveFile moves a file using git mv.
func (e *externalBackend) moveFile(src, dst string) error {
	srcRel, err := e.toRelative(src)
	if err != nil {
		return fmt.Errorf("invalid source path: %w", err)
	}
	dstRel, err := e.toRelative(dst)
	if err != nil {
		return fmt.Errorf("invalid destination path: %w", err)
	}
	_, err = e.run("mv", "--", srcRel, dstRel)
	if err != nil {
		return fmt.Errorf("move file: %w", err)
	}
	return nil
}

// commit creates a commit with the given message.
func (e *externalBackend) commit(msg string) error {
	_, err := e.run("commit", "-m", msg)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// commitFiles creates a commit restricted to the given paths only.
// other staged files remain staged but are excluded from this commit.
func (e *externalBackend) commitFiles(msg string, paths ...string) error {
	if len(paths) == 0 {
		return errors.New("commit files: no paths provided")
	}
	args := []string{"commit", "-m", msg, "--"}
	for _, p := range paths {
		rel, err := e.toRelative(p)
		if err != nil {
			return err
		}
		args = append(args, rel)
	}
	if _, err := e.run(args...); err != nil {
		return fmt.Errorf("commit files: %w", err)
	}
	return nil
}

// autoCommitAll stages all non-ignored files and commits them when anything changed.
func (e *externalBackend) autoCommitAll(msg string) (bool, error) {
	if err := e.validateAutoCommitState(); err != nil {
		return false, err
	}
	index, err := e.snapshotIndex()
	if err != nil {
		return false, err
	}
	restoreOnError := func(cause error) error {
		if restoreErr := index.restore(); restoreErr != nil {
			return errors.Join(cause, fmt.Errorf("restore Git index: %w", restoreErr))
		}
		return cause
	}

	// No :(exclude) pathspecs here: git add dies with "paths are ignored by one of
	// your .gitignore files" when any command-line pathspec — exclude magic included —
	// names an existing ignored path, which .loopai/progress and .loopai/worktrees are
	// on every run with prior history. Plain add -A skips ignored files natively; the
	// reset below strips runtime paths staged through a negated custom .loopai/.gitignore.
	_, err = e.run("add", "-A", "--", ".")
	if err != nil {
		return false, restoreOnError(fmt.Errorf("stage files: %w", err))
	}
	// Pathspec exclusions do not remove entries that were already staged before
	// this operation. Restore runtime paths to HEAD so the commit cannot capture
	// those entries either.
	if _, err = e.run("reset", "--quiet", "HEAD", "--", ".loopai/progress", ".loopai/worktrees"); err != nil {
		return false, restoreOnError(fmt.Errorf("unstage runtime artifacts: %w", err))
	}

	// Check for staged non-runtime changes. Runtime files may remain visible when
	// this low-level method is used before EnsureLocalGitignore. Any remaining
	// unstaged entry means git add -A could not capture the full working-tree state
	// (most commonly dirty content inside a submodule), so refuse a partial commit.
	out, err := e.run("status", "--porcelain", "--", ".",
		":(exclude).loopai/progress", ":(exclude).loopai/progress/**",
		":(exclude).loopai/worktrees", ":(exclude).loopai/worktrees/**")
	if err != nil {
		return false, restoreOnError(fmt.Errorf("check status: %w", err))
	}
	if out == "" {
		if restoreErr := index.restore(); restoreErr != nil {
			return false, fmt.Errorf("restore Git index after no-op auto-commit: %w", restoreErr)
		}
		return false, nil
	}
	for line := range strings.SplitSeq(out, "\n") {
		if len(line) < 2 {
			return false, restoreOnError(fmt.Errorf("parse status after staging: unexpected porcelain entry %q", line))
		}
		if line[0] == '?' || line[1] != ' ' {
			return false, restoreOnError(errors.New("working tree still has unstaged changes after git add -A; dirty submodules or concurrently modified files cannot be auto-committed safely"))
		}
	}

	_, err = e.run("commit", "-m", msg)
	if err != nil {
		return false, restoreOnError(fmt.Errorf("commit: %w", err))
	}
	return true, nil
}

type indexSnapshot struct {
	path    string
	data    []byte
	mode    os.FileMode
	existed bool
}

func (e *externalBackend) snapshotIndex() (indexSnapshot, error) {
	path, err := e.run("rev-parse", "--git-path", "index")
	if err != nil {
		return indexSnapshot{}, fmt.Errorf("locate Git index before auto-commit: %w", err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.path, path)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return indexSnapshot{path: path}, nil
	}
	if err != nil {
		return indexSnapshot{}, fmt.Errorf("inspect Git index before auto-commit: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return indexSnapshot{}, fmt.Errorf("read Git index before auto-commit: %w", err)
	}
	return indexSnapshot{path: path, data: data, mode: info.Mode(), existed: true}, nil
}

func (s indexSnapshot) restore() error {
	if !s.existed {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove newly created index: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(s.path, s.data, s.mode.Perm()); err != nil {
		return fmt.Errorf("write saved index: %w", err)
	}
	return nil
}

func (e *externalBackend) validateAutoCommitState() error {
	unmerged, err := e.run("ls-files", "-u")
	if err != nil {
		return fmt.Errorf("check unmerged paths before auto-commit: %w", err)
	}
	if unmerged != "" {
		return errors.New("refuse auto-commit with unmerged paths; finish or abort the current Git operation first")
	}

	gitDir, err := e.gitDir()
	if err != nil {
		return fmt.Errorf("locate Git operation state before auto-commit: %w", err)
	}
	states := []struct {
		path string
		name string
	}{
		{path: "MERGE_HEAD", name: "merge"},
		{path: "CHERRY_PICK_HEAD", name: "cherry-pick"},
		{path: "REVERT_HEAD", name: "revert"},
		{path: "rebase-merge", name: "rebase"},
		{path: "rebase-apply", name: "rebase"},
		{path: "sequencer", name: "sequenced Git operation"},
	}
	for _, state := range states {
		_, statErr := os.Stat(filepath.Join(gitDir, state.path))
		switch {
		case statErr == nil:
			return fmt.Errorf("refuse auto-commit while a %s is in progress; finish or abort it first", state.name)
		case os.IsNotExist(statErr):
			continue
		default:
			return fmt.Errorf("inspect %s state before auto-commit: %w", state.name, statErr)
		}
	}
	return nil
}

func (e *externalBackend) gitDir() (string, error) {
	out, err := e.run("rev-parse", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("get Git directory: %w", err)
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(e.path, out)
	}
	return filepath.Clean(out), nil
}

// createInitialCommit stages all non-ignored files and creates an initial commit.
func (e *externalBackend) createInitialCommit(msg string) error {
	committed, err := e.autoCommitAll(msg)
	if err != nil {
		return err
	}
	if !committed {
		return errors.New("no files to commit")
	}
	return nil
}

// diffStats returns change statistics between baseBranch and headRef, which must be a
// resolvable revision such as HEAD or a branch name.
// returns zero stats if either side doesn't resolve or both point at the same commit.
func (e *externalBackend) diffStats(baseBranch, headRef string) (DiffStats, error) {
	// check if base branch exists (try local, remote, origin/ prefix)
	baseRef := e.resolveRef(baseBranch)
	if baseRef == "" {
		return DiffStats{}, nil
	}

	// check if the head side equals base
	headHash, err := e.revParse(headRef)
	if err != nil {
		return DiffStats{}, nil //nolint:nilerr // unresolvable head means no stats
	}
	baseHash, err := e.revParse(baseRef)
	if err != nil {
		return DiffStats{}, nil //nolint:nilerr // can't resolve base, return zero
	}
	if baseHash == headHash {
		return DiffStats{}, nil
	}

	// get numstat
	out, err := e.run("diff", "--numstat", baseRef+"..."+headRef)
	if err != nil {
		return DiffStats{}, fmt.Errorf("diff numstat: %w", err)
	}

	if out == "" {
		return DiffStats{}, nil
	}

	var result DiffStats
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		// binary files show "-" for additions/deletions
		if parts[0] == "-" || parts[1] == "-" {
			result.Files++
			continue
		}
		additions, _ := strconv.Atoi(parts[0])
		deletions, _ := strconv.Atoi(parts[1])
		result.Files++
		result.Additions += additions
		result.Deletions += deletions
	}
	return result, nil
}

// resolveRef tries to resolve a branch name to a valid git ref.
// checks local branch, remote tracking (origin/<name>), "origin/" prefixed names,
// and finally arbitrary refs like commit hashes or tags via rev-parse.
func (e *externalBackend) resolveRef(branchName string) string {
	// try local branch
	localRef := "refs/heads/" + branchName
	if e.refExists(localRef) {
		return localRef
	}

	// try remote tracking branch
	remoteRef := "refs/remotes/origin/" + branchName
	if e.refExists(remoteRef) {
		return remoteRef
	}

	// try as-is for "origin/" prefixed names
	if strings.HasPrefix(branchName, "origin/") {
		remoteName := branchName[7:]
		remoteRef = "refs/remotes/origin/" + remoteName
		if e.refExists(remoteRef) {
			return remoteRef
		}
	}

	// try as arbitrary ref (commit hash, tag, etc.) via rev-parse
	cmd := exec.CommandContext(context.Background(), e.command, "rev-parse", "--verify", "--quiet", branchName)
	cmd.Dir = e.path
	if cmd.Run() == nil {
		return branchName
	}

	return ""
}

// refExists checks if a git reference exists.
func (e *externalBackend) refExists(ref string) bool {
	cmd := exec.CommandContext(context.Background(), e.command, "show-ref", "--verify", "--quiet", ref)
	cmd.Dir = e.path
	return cmd.Run() == nil
}

// toRelative converts a path to be relative to the repository root.
func (e *externalBackend) toRelative(path string) (string, error) {
	if !filepath.IsAbs(path) {
		cleaned := filepath.Clean(path)
		if strings.HasPrefix(cleaned, "..") {
			return "", fmt.Errorf("path %q escapes repository root", path)
		}
		return cleaned, nil
	}

	// resolve symlinks for consistent comparison (macOS /var -> /private/var)
	resolved, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		// if can't resolve, use original path
		resolved = filepath.Dir(path)
	}
	path = filepath.Join(resolved, filepath.Base(path))

	rel, err := filepath.Rel(e.path, path)
	if err != nil {
		return "", fmt.Errorf("path outside repository: %w", err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q is outside repository root %q", path, e.path)
	}
	return rel, nil
}

// addWorktree creates a git worktree at the given path.
// when createBranch is true, creates a new branch with `git worktree add <path> -b <branch>`.
// when createBranch is false, uses existing branch with `git worktree add <path> <branch>`.
func (e *externalBackend) addWorktree(ctx context.Context, path, branch string, createBranch bool) error {
	var args []string
	if createBranch {
		args = []string{"worktree", "add", path, "-b", branch}
	} else {
		args = []string{"worktree", "add", path, branch}
	}
	_, err := e.runContext(ctx, args...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("add worktree: %w", ctxErr)
		}
		return fmt.Errorf("add worktree: %w", err)
	}
	return nil
}

// removeWorktree removes a git worktree at the given path.
func (e *externalBackend) removeWorktree(path string) error {
	_, err := e.run("worktree", "remove", "--force", path)
	if err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	return nil
}

// removeWorktreeSafe refuses to discard modifications or untracked files. Ignored files are
// deliberately allowed and are deleted by the non-forced `git worktree remove` operation.
func (e *externalBackend) removeWorktreeSafe(path string) error {
	target, err := newExternalBackend(path, e.command)
	if err != nil {
		return fmt.Errorf("inspect worktree before removal: %w", err)
	}
	out, err := target.run("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect worktree content before removal: %w", err)
	}
	if out != "" {
		return errors.New("remove worktree: worktree contains modified or untracked files")
	}
	_, err = e.run("worktree", "remove", path)
	if err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	return nil
}

// pruneWorktrees prunes stale worktree entries.
func (e *externalBackend) pruneWorktrees() error {
	_, err := e.run("worktree", "prune")
	if err != nil {
		return fmt.Errorf("prune worktrees: %w", err)
	}
	return nil
}

// extractPathFromPorcelain extracts file path from git status --porcelain output.
// format: "XY path" or "XY original -> renamed"
func (e *externalBackend) extractPathFromPorcelain(line string) string {
	if len(line) < 4 {
		return ""
	}
	// skip the 2-char status code and space
	path := line[3:]
	// handle renames: "XY old -> new"
	if idx := strings.Index(path, " -> "); idx >= 0 {
		path = path[idx+4:]
	}
	return path
}
