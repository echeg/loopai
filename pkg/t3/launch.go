package t3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// worktreeTimeout bounds `git worktree add` on the server; checkouts of large repositories are slow.
const worktreeTimeout = 5 * time.Minute

// launchTerminalID names the thread terminal that runs loopai.
const launchTerminalID = "loopai"

// ShellKind selects how the launch command is quoted for the thread terminal's shell.
type ShellKind int

const (
	// ShellPOSIX quotes for sh-compatible shells, the T3 Code default outside Windows.
	ShellPOSIX ShellKind = iota
	// ShellPowerShell quotes for pwsh and Windows PowerShell, the T3 Code default on Windows.
	ShellPowerShell
)

// RPC is the WebSocket surface the launcher needs.
type RPC interface {
	CreateWorktree(ctx context.Context, in CreateWorktreeInput) (Worktree, error)
	OpenTerminal(ctx context.Context, in TerminalOpenInput) error
	WriteTerminal(ctx context.Context, threadID, terminalID, data string) error
}

// LaunchRequest describes one --t3-launch invocation.
type LaunchRequest struct {
	RepoRoot   string // source checkout root; must be a T3 Code project root
	PlanFile   string // plan path inside RepoRoot
	Branch     string // new branch name for the worktree
	BaseRef    string // branch or commit the worktree starts from
	SourceHead string // commit the new worktree must check out
	LoopaiPath string // absolute path of the loopai executable
	Args       []string
	Executor   string
	Model      string
	Shell      ShellKind
	Endpoint   Endpoint
	// HeadOf reports the commit checked out in a directory.
	HeadOf func(dir string) (string, error)
}

// LaunchResult reports what the launcher created.
type LaunchResult struct {
	ThreadID     string
	WorktreePath string
	Branch       string
}

// PartialLaunchError reports a launch that failed after the worktree was created. The worktree is
// left in place so nothing the user might need is deleted.
type PartialLaunchError struct {
	Result LaunchResult
	Err    error
}

func (e *PartialLaunchError) Error() string {
	var created []string
	if e.Result.WorktreePath != "" {
		created = append(created, "worktree "+e.Result.WorktreePath+" (branch "+e.Result.Branch+")")
	}
	if e.Result.ThreadID != "" {
		created = append(created, "thread "+e.Result.ThreadID)
	}
	return fmt.Sprintf("%v; already created: %s", e.Err, strings.Join(created, ", "))
}

func (e *PartialLaunchError) Unwrap() error { return e.Err }

// overrideDirs are the untracked .loopai inputs carried into the new worktree, which receives
// committed files only.
var overrideDirs = []string{"config", "prompts", "agents"}

// Launch creates a T3-managed worktree and a thread bound to it, carries the plan and local
// .loopai overrides over, and starts `loopai --t3` in the thread's terminal.
func Launch(ctx context.Context, api Dispatcher, rpc RPC, req LaunchRequest) (LaunchResult, error) {
	rel, err := planRelPath(req.RepoRoot, req.PlanFile)
	if err != nil {
		return LaunchResult{}, err
	}
	shell, err := api.Shell(ctx)
	if err != nil {
		return LaunchResult{}, err //nolint:wrapcheck // Shell already names the failing call
	}
	project, ok := shell.FindProject(req.RepoRoot)
	if !ok {
		return LaunchResult{}, fmt.Errorf("t3: no T3 Code project for %s; add the repository in T3 Code first", req.RepoRoot)
	}

	wtCtx, cancel := context.WithTimeout(ctx, worktreeTimeout)
	wt, err := rpc.CreateWorktree(wtCtx, CreateWorktreeInput{Cwd: req.RepoRoot, RefName: req.BaseRef, NewRefName: req.Branch})
	cancel()
	if err != nil {
		return LaunchResult{}, err //nolint:wrapcheck // RPC errors name the failing method
	}
	result := LaunchResult{WorktreePath: wt.Path, Branch: req.Branch}
	fail := func(err error) (LaunchResult, error) {
		return result, &PartialLaunchError{Result: result, Err: err}
	}

	if req.HeadOf != nil {
		head, headErr := req.HeadOf(wt.Path)
		if headErr != nil {
			return fail(fmt.Errorf("t3: read worktree HEAD: %w", headErr))
		}
		if head != req.SourceHead {
			return fail(fmt.Errorf("t3: worktree HEAD %s does not match source HEAD %s", head, req.SourceHead))
		}
	}
	if err := carryInputs(req.RepoRoot, wt.Path, rel); err != nil {
		return fail(err)
	}

	threadID := NewID()
	create := NewThreadCreate(threadID, project.ID, runName(req.PlanFile)+" · starting",
		modelSelection(req.Executor, req.Model), req.Branch, wt.Path)
	if _, err := api.Dispatch(ctx, create); err != nil {
		return fail(err)
	}
	result.ThreadID = threadID

	env := map[string]string{
		"LOOPAI_T3": "true",
		EnvToken:    req.Endpoint.Token,
		EnvURL:      req.Endpoint.BaseURL,
		EnvThreadID: threadID,
	}
	open := TerminalOpenInput{ThreadID: threadID, TerminalID: launchTerminalID, Cwd: wt.Path, WorktreePath: wt.Path, Env: env}
	if err := rpc.OpenTerminal(ctx, open); err != nil {
		return fail(err)
	}
	command := LaunchCommand(req.Shell, req.LoopaiPath, append(append([]string{"--t3"}, req.Args...), rel))
	if err := rpc.WriteTerminal(ctx, threadID, launchTerminalID, command+"\r"); err != nil {
		return fail(err)
	}
	return result, nil
}

func planRelPath(root, planFile string) (string, error) {
	absPlan, err := filepath.Abs(planFile)
	if err != nil {
		return "", fmt.Errorf("t3: resolve plan path: %w", err)
	}
	rel, err := filepath.Rel(root, absPlan)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("t3: plan %s is outside the repository %s", planFile, root)
	}
	if _, err := os.Stat(absPlan); err != nil {
		return "", fmt.Errorf("t3: plan: %w", err)
	}
	return rel, nil
}

// carryInputs copies the plan (so uncommitted edits reach the worktree) and every .loopai
// override file the worktree does not already have. Existing worktree files other than the plan
// are never overwritten.
func carryInputs(srcRoot, dstRoot, planRel string) error {
	if err := copyFile(filepath.Join(srcRoot, planRel), filepath.Join(dstRoot, planRel)); err != nil {
		return fmt.Errorf("t3: copy plan: %w", err)
	}
	for _, dir := range overrideDirs {
		src := filepath.Join(srcRoot, ".loopai", dir)
		info, err := os.Lstat(src)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("t3: inspect %s: %w", src, err)
		}
		if info.Mode().IsRegular() {
			if err := copyIfAbsent(src, filepath.Join(dstRoot, ".loopai", dir)); err != nil {
				return err
			}
			continue
		}
		if !info.IsDir() {
			continue // symlinks and special files are not carried over
		}
		walkErr := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, relErr := filepath.Rel(srcRoot, path)
			if relErr != nil {
				return relErr //nolint:wrapcheck // wrapped below
			}
			return copyIfAbsent(path, filepath.Join(dstRoot, rel))
		})
		if walkErr != nil {
			return fmt.Errorf("t3: copy %s: %w", src, walkErr)
		}
	}
	return nil
}

func copyIfAbsent(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("t3: inspect %s: %w", dst, err)
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // paths come from the source checkout
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	if mkErr := os.MkdirAll(filepath.Dir(dst), 0o750); mkErr != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dst), mkErr)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // destination inside the new worktree
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}

// LaunchCommand renders the command line typed into the thread terminal.
func LaunchCommand(shell ShellKind, program string, args []string) string {
	quote := posixQuote
	parts := make([]string, 0, len(args)+2)
	if shell == ShellPowerShell {
		quote = powerShellQuote
		parts = append(parts, "&")
	}
	parts = append(parts, quote(program))
	for _, arg := range args {
		parts = append(parts, quote(arg))
	}
	return strings.Join(parts, " ")
}

// posixQuote single-quotes a word unless it consists only of characters no POSIX shell treats
// specially.
func posixQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._/:,+-=@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// powerShellQuote always single-quotes: inside single quotes PowerShell expands nothing, and a
// literal quote is doubled. Curly single quotes are quote characters to PowerShell too.
func powerShellQuote(s string) string {
	r := strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛")
	return "'" + r.Replace(s) + "'"
}
