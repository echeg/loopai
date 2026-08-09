# Worktree Run Lock and Auto-Resume

## Overview
- Give every worktree run a liveness lock: the loopai process holds an OS advisory file lock (flock/LockFileEx) on a per-worktree lock file for its entire lifetime, with pid and start time written inside for diagnostics. The OS releases the lock on any process death (crash, kill -9, closed terminal), so lock state precisely distinguishes "run alive in this worktree" from "orphaned worktree".
- Change `--worktree` routing when the target worktree already exists: acquire the run lock. Held → fail with a precise message naming the pid and path. Acquired → validate and automatically resume in the existing worktree (today's `--resume-worktree` semantics: no auto-commit, no source sync). The user expectation becomes true: re-running the same `--worktree` command after an interruption just continues.
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
- Breaking change: remove `--resume-worktree`; users create or continue by rerunning the same `--worktree` command. Keep fresh-creation behavior (including stale-branch automerge) unchanged.
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
- [x] add `tryLockRepositoryFile(f *os.File) (acquired bool, err error)` to both `repository_lock_unix.go` (flock LOCK_EX|LOCK_NB) and `repository_lock_windows.go` (LockFileEx with LOCKFILE_FAIL_IMMEDIATELY)
- [x] add `Service.AcquireWorktreeRunLockContext(ctx)` to `pkg/git`: resolve the worktree's private git dir through the service's configured VCS command, open/create `loopai-run.lock` there, try-lock non-blocking; on success truncate and write `pid=<pid> started=<RFC3339>` (diagnostic only — the flock is the authority, pids get reused); on contention return a typed `ErrWorktreeBusy` carrying the worktree path and recorded pid/started values for the final error message
- [x] use the authoritative acquisition itself for routing; do not maintain a separate probe path that cannot transfer ownership atomically
- [x] write tests: acquire/release cycle; second acquire while held → `ErrWorktreeBusy` with recorded info; corrupt lock-file content leaves diagnostics empty without affecting contention; lock file lives under the worktree's git dir, not the working tree; configured VCS command and cancellation are honored; process death releases the lock
- [x] run tests - must pass before next task

### Task 2: Hold the run lock for the lifetime of every worktree run
- [x] in `cmd/loopai/main.go` worktree setup: hold the shared repository preparation lock across target classification or creation and authoritative run-lock acquisition, then register release in the existing cleanup chain (same discipline as `WtCleanup`); a fresh worktree is never visible without its creator owning the run lock
- [x] the resume code path acquires the lock the same way; contention → `ErrWorktreeBusy` message
- [x] confirm the release also runs on the interrupt path (Ctrl+C cleanup), and that a kill -9 leaves only the OS to release the flock (no stale-state cleanup needed by design)
- [x] write tests: a competing acquire reports busy during a worktree run and succeeds after cleanup; force-exit cleanup removes fresh worktrees but preserves and unlocks resumed worktrees; resume contention includes pid info
- [x] run tests - must pass before next task

### Task 3: Auto-resume routing for --worktree with an existing worktree
- [x] replace the unconditional rejection (`worktree already exists at %s, another instance may be running`, `pkg/git/service.go:~754` and its callers) with lock-aware routing: acquire the run lock before mutable resume validation; busy → typed error `plan worktree %s is busy: loopai is already running in it (%s)` with recorded pid/started info; acquired → validate the existing target and log `resuming interrupted worktree %s`
- [x] when `-c/--commit` was passed and routing lands on auto-resume, print a warning that the flag is ignored (resume never auto-commits) and do not touch the source checkout
- [x] remove the `--resume-worktree` option from the options struct and all validation/routing that references it; the resume machinery it drove (`requireResumeWorktree`, `validateResumeWorktree`, the `resumed:` wiring) stays and is now reached only via auto-resume; delete or repurpose its flag-level tests
- [x] worktree exists but fails resume validation (branch mismatch, foreign directory) → keep failing with the validation error; never silently delete or recreate
- [x] update existing tests asserting the old "already exists" error; add routing tests: existing worktree + free lock + plain `--worktree` → resume; + held lock → busy error; + `-c` → resume with ignored-flag warning; missing worktree → creation flow unchanged (automerge behavior intact)
- [x] run tests - must pass before next task

### Task 4: Verify acceptance criteria
- [x] verify all requirements from Overview are implemented (crash → same command continues; live run → precise busy error; fresh creation untouched; `--resume-worktree` rejected as unknown flag)
- [x] verify edge cases: two loopai processes racing for the same orphan (one wins the lock, the other gets busy); shared repository locking makes classify/create/acquire atomic and guards release-before-remove teardown; if the worktree disappears before ownership is obtained, fail without deleting or recreating it; lock file deleted manually while held (flock on the open fd still held — document that the file is advisory)
- [x] run full test suite (`make test`)
- [x] run linter (`make lint`) - all issues must be fixed
- [x] verify test coverage meets project standard (80%+) for the new code paths
- [x] cross-compile `GOOS=windows GOARCH=amd64 go build ./...` (platform lock code changed)

### Task 5: [Final] Update documentation
- [x] update CLAUDE.md worktree section: run lock, auto-resume semantics, `-c` ignored on resume, `--resume-worktree` removed
- [x] update llms.txt `--worktree`/resume paragraphs (note the removal the same way the removed `--codex-only` alias is noted)
- [x] update README.md flag docs and any "interrupted run" guidance; remove every `--resume-worktree` mention

## Technical Details
- The flock is the single source of truth for liveness; the pid inside the file is decoration for error messages (pid reuse makes pid-based checks unreliable — never gate behavior on it).
- Lock file path: `$(git -C <wtPath> rev-parse --git-dir)/loopai-run.lock` — inside `.git/worktrees/<name>/` of the main repository; `git worktree remove` deletes it with the metadata, and it can never appear in `git status` of the worktree.
- Auto-resume inherits resume's contract wholesale: no auto-commit, no source sync, no ancestor requirement — the earlier stale-branch automerge applies only to the create-from-existing-branch flow (worktree absent), which is unchanged.
- The shared repository preparation lock serializes classification, fresh creation, and final non-blocking run-lock acquisition. Teardown takes the same shared lock before releasing the run lock and removing the worktree, which keeps the required Windows release-before-delete ordering without exposing an unlocked checkout to a resumer.
- Non-worktree (single-checkout branch) runs are out of scope: no shared directory to contend over.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- Start a worktree run, kill the terminal hard, reopen, re-run the exact same `--worktree` command → run resumes in the same worktree, log says `resuming interrupted worktree ...`
- While a worktree run is executing, run the same command from another terminal → immediate busy error naming the pid
- `loopai --resume-worktree ...` → unknown-flag error; `--help` no longer lists it

**External system updates**: none
