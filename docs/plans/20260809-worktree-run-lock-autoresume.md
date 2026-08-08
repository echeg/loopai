# Worktree Run Lock and Auto-Resume

## Overview
- Give every worktree run a liveness lock: the loopai process holds an OS advisory file lock (flock/LockFileEx) on a per-worktree lock file for its entire lifetime, with pid and start time written inside for diagnostics. The OS releases the lock on any process death (crash, kill -9, closed terminal), so lock state precisely distinguishes "run alive in this worktree" from "orphaned worktree".
- Change `--worktree` routing when the target worktree already exists: probe the run lock. Held → fail with a precise message naming the pid and path. Free → automatically resume in the existing worktree (today's `--resume-worktree` semantics: no auto-commit, no source sync). The user expectation becomes true: re-running the same `--worktree` command after an interruption just continues.
- Remove the `--resume-worktree` flag entirely: auto-resume makes it redundant, and the project prefers clean removals over deprecated aliases (precedent: the removed `-c` alias for `--codex-only`). Passing it after this change is an unknown-flag error.
- Problem it solves: today an existing worktree makes `--worktree` fail with a *guess* ("another instance may be running"), and the user must know a second flag to continue after a crash. The tool has no way to tell busy from orphaned; the lock provides exactly that signal.

## Context (from discovery)
- Files/components involved:
  - `pkg/git/repository_lock_unix.go` / `pkg/git/repository_lock_windows.go` — existing blocking lock primitives (`lockRepositoryFile`/`unlockRepositoryFile`); need non-blocking `tryLockRepositoryFile` variants (LOCK_NB / LOCKFILE_FAIL_IMMEDIATELY)
  - `pkg/git/service.go` — `AcquireWorktreeCreationLockContext` (~156) as the pattern to follow; existing-worktree rejection at ~754 (`worktree already exists at %s, another instance may be running`)
  - `cmd/loopai/main.go` — `ResumeWorktree` option (~68), resume wiring (`resumed:` ~1210, `requireResumeWorktree` ~1334, `validateResumeWorktree` ~1246), worktree setup around `AcquireWorktreeCreationLockContext` call (~1272)
- Related patterns found:
  - Lock file placement pattern: creation lock lives in the shared Git metadata dir (`gitCommonDir()`); the run lock belongs in the worktree's *private* git dir (`git rev-parse --git-dir` inside the worktree, i.e. `.git/worktrees/<name>/`), so it never dirties the working tree and vanishes with worktree removal
  - Resume validation already exists (`validateResumeWorktree` checks worktree/branch/plan consistency) — auto-resume routes into the same path
- Dependencies identified: none new (platform lock code already split per OS)

## Development Approach
- **Testing approach**: Regular (code first, then tests in the same task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility: `--resume-worktree` keeps working; fresh-creation flow (including stale-branch automerge) unchanged
- Tests must use `t.TempDir()` repositories and never touch real user configuration

## Testing Strategy
- **Unit tests**: table-driven tests in `pkg/git` and `cmd/loopai` with real temp repos; lock contention simulated by acquiring the lock in-test before invoking the routing under test
- **E2E tests**: none (no dashboard changes)

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## Implementation Steps

### Task 1: Add non-blocking try-lock primitives and the run lock
- [ ] add `tryLockRepositoryFile(f *os.File) (acquired bool, err error)` to both `repository_lock_unix.go` (flock LOCK_EX|LOCK_NB) and `repository_lock_windows.go` (LockFileEx with LOCKFILE_FAIL_IMMEDIATELY)
- [ ] add `AcquireWorktreeRunLock(wtPath string) (release func() error, err error)` to `pkg/git`: resolve the worktree's private git dir via `rev-parse --git-dir` executed in `wtPath`, open/create `loopai-run.lock` there, try-lock non-blocking; on success truncate and write `pid=<pid> started=<RFC3339>` (diagnostic only — the flock is the authority, pids get reused); on contention return a typed `ErrWorktreeBusy` carrying the lock file's recorded pid/started values for the error message
- [ ] add `ProbeWorktreeRunLock(wtPath string) (busy bool, info string, err error)`: same try-lock, immediately released when acquired; used by routing to classify an existing worktree
- [ ] write tests: acquire/release cycle; second acquire while held → `ErrWorktreeBusy` with recorded info; probe on free lock → not busy and leaves the lock acquirable; missing/corrupt lock file content → still works (info empty); lock file lives under the worktree's git dir, not the working tree
- [ ] run tests - must pass before next task

### Task 2: Hold the run lock for the lifetime of every worktree run
- [ ] in `cmd/loopai/main.go` worktree setup: after a worktree is created or resumed, acquire the run lock and register release in the existing cleanup chain (same discipline as `WtCleanup`); a failed acquire on a *freshly created* worktree is an internal error (nothing else can know about it yet)
- [ ] the resume code path acquires the lock the same way; contention → `ErrWorktreeBusy` message
- [ ] confirm the release also runs on the interrupt path (Ctrl+C cleanup), and that a kill -9 leaves only the OS to release the flock (no stale-state cleanup needed by design)
- [ ] write tests: run-lock acquired during a worktree run (probe from test → busy) and released after cleanup (probe → free); resume path contention error includes pid info
- [ ] run tests - must pass before next task

### Task 3: Auto-resume routing for --worktree with an existing worktree
- [ ] replace the unconditional rejection (`worktree already exists at %s, another instance may be running`, `pkg/git/service.go:~754` and its callers) with lock-aware routing: probe the run lock; busy → error `plan worktree %s is busy: loopai is already running in it (%s)` with the recorded pid/started info; free → route into the existing resume path (`validateResumeWorktree` consistency checks included) and log `resuming interrupted worktree %s`
- [ ] when `-c/--commit` was passed and routing lands on auto-resume, print a warning that the flag is ignored (resume never auto-commits) and do not touch the source checkout
- [ ] remove the `--resume-worktree` option from the options struct and all validation/routing that references it; the resume machinery it drove (`requireResumeWorktree`, `validateResumeWorktree`, the `resumed:` wiring) stays and is now reached only via auto-resume; delete or repurpose its flag-level tests
- [ ] worktree exists but fails resume validation (branch mismatch, foreign directory) → keep failing with the validation error; never silently delete or recreate
- [ ] update existing tests asserting the old "already exists" error; add routing tests: existing worktree + free lock + plain `--worktree` → resume; + held lock → busy error; + `-c` → resume with ignored-flag warning; missing worktree → creation flow unchanged (automerge behavior intact)
- [ ] run tests - must pass before next task

### Task 4: Verify acceptance criteria
- [ ] verify all requirements from Overview are implemented (crash → same command continues; live run → precise busy error; fresh creation untouched; `--resume-worktree` rejected as unknown flag)
- [ ] verify edge cases: two loopai processes racing to resume the same orphan (one wins the lock, the other gets busy), worktree removed between probe and resume (creation-lock serialization covers it), lock file deleted manually while held (flock on the open fd still held — document that the file is advisory)
- [ ] run full test suite (`make test`)
- [ ] run linter (`make lint`) - all issues must be fixed
- [ ] verify test coverage meets project standard (80%+) for the new code paths
- [ ] cross-compile `GOOS=windows GOARCH=amd64 go build ./...` (platform lock code changed)

### Task 5: [Final] Update documentation
- [ ] update CLAUDE.md worktree section: run lock, auto-resume semantics, `-c` ignored on resume, `--resume-worktree` removed
- [ ] update llms.txt `--worktree`/resume paragraphs (note the removal the same way the removed `--codex-only` alias is noted)
- [ ] update README.md flag docs and any "interrupted run" guidance; remove every `--resume-worktree` mention

## Technical Details
- The flock is the single source of truth for liveness; the pid inside the file is decoration for error messages (pid reuse makes pid-based checks unreliable — never gate behavior on it).
- Lock file path: `$(git -C <wtPath> rev-parse --git-dir)/loopai-run.lock` — inside `.git/worktrees/<name>/` of the main repository; `git worktree remove` deletes it with the metadata, and it can never appear in `git status` of the worktree.
- Auto-resume inherits resume's contract wholesale: no auto-commit, no source sync, no ancestor requirement — the earlier stale-branch automerge applies only to the create-from-existing-branch flow (worktree absent), which is unchanged.
- Probe-then-acquire has a benign race (two resumers): the final non-blocking acquire decides; the loser gets the busy error. The existing creation lock already serializes create-vs-create.
- Non-worktree (single-checkout branch) runs are out of scope: no shared directory to contend over.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- Start a worktree run, kill the terminal hard, reopen, re-run the exact same `--worktree` command → run resumes in the same worktree, log says `resuming interrupted worktree ...`
- While a worktree run is executing, run the same command from another terminal → immediate busy error naming the pid
- `loopai --resume-worktree ...` → unknown-flag error; `--help` no longer lists it

**External system updates**: none
