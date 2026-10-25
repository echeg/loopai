# Guard dirty checkouts and keep dirty foreground worktrees

## Overview
- Fix the two major backlog entries and remove them once fixed:
  - `docs/backlog/review-only-modes-commit-user-edits.md`: `--review` and `--external-only` run in the
    user's own checkout, and so does a non-worktree full or tasks-only run whose plan branch is already
    checked out (`prepareBranchPlan` returns "already on feature branch" before its dirty-tree gate,
    `pkg/git/service.go:940`). Every commit site stages by file path, so a user's unrelated hunks in a
    file the model also touched land in loopai's `fix: address code review findings` or `feat: ...`
    commit.
  - `docs/backlog/preserve-review-worktree-on-failure.md`: a fresh foreground `--worktree` run removes
    its worktree with `git worktree remove --force` on every exit, so uncommitted reviewer fixes are lost
    when the run fails or is interrupted before a commit boundary.
- Result: loopai lists the dirty files and asks before it starts committing in a dirty checkout it does
  not own, and a foreground `--worktree` run never deletes a worktree that holds uncommitted work.

## Decisions
- **Context**: an uncommitted file in the user's checkout is sometimes the user's unrelated work in
  progress and sometimes loopai's own unfinished work (for example `--external-only` launched over fixes
  an interrupted run left uncommitted, which the user wants committed). A hard gate would break the
  second case.
- **Chosen approach (dirty checkout)**: a startup guard for the three exposed paths (`ModeReview`,
  `ModeCodexOnly`, and a non-worktree single-plan full/tasks-only run whose current branch is not the
  default branch). When the checkout has dirty paths other than the selected plan file, loopai prints the
  list with one line saying review and task commits stage whole files, so edits in a file the model also
  changes can land in loopai's commit. With an interactive terminal it asks `Continue anyway?` through
  `input.AskYesNo` (default no, declining exits without a run); without one (or with
  `req.NonInteractive`) it prints the warning and continues. No new flag or config key, so the skill
  contract surface is unchanged.
- **Chosen approach (foreground worktree)**: a fresh foreground worktree is removed only when it is
  settled: no uncommitted or untracked changes and no Git operation in progress, the existing
  `worktreeUnsettled` check from `cmd/loopai/detach_worktree.go`. Otherwise it is kept on any outcome
  (failure, interruption, or success) and loopai prints its path, that rerunning the same command
  resumes in it, and that `git worktree remove <path>` discards it. Clean worktrees are removed exactly
  as today, so nothing accumulates when there was nothing to lose.
- **Rejected alternatives**:
  - refusing to start on a dirty tree unless `--allow-dirty` is given: safest, but a new visible flag
    changes the `TestSkillContract` surface and makes the legitimate "commit loopai's leftovers" case
    always need a flag.
  - a warning without a question: cheapest, but scrolls past and prevents nothing.
  - moving `prepareBranchPlan`'s dirty-tree gate above its early return: refuses the legitimate resume
    of an interrupted non-worktree run whose leftovers are loopai's own.
  - baseline-aware hunk staging (`git stash push --keep-index` around fixes): the only way to keep
    today's silent tolerance, and by far the most fragile; all seven commit instruction sites would move.
  - keeping a foreground worktree on every non-success once review began (the entry's suggestion), or
    on every non-success: needs phase tracking or keeps clean worktrees that hold nothing.
- **Verified facts**:
  - `modeRequiresBranch` excludes `ModeReview`/`ModeCodexOnly` (`cmd/loopai/main.go:3414`);
    review-only startup runs `checkReviewDiffRange` at `cmd/loopai/main.go:991` and `:1068`.
  - `prepareSelectedPlanBranch` (`cmd/loopai/main.go:1076` area) calls `CreateBranchForPlan`, which
    returns `nil` silently when `prepareBranchPlan` reports "already on feature branch"
    (`pkg/git/service.go:934-940`, `:1001-1003`).
  - `pkg/input.AskYesNo` prompts `[y/N]` (`pkg/input/input.go:223`); `golang.org/x/term` is already a
    dependency (`cmd/loopai/terminal_unix.go`).
  - `runWithWorktree` (`cmd/loopai/main.go:2154`) removes a fresh worktree through `setupCleanup(true)`
    on setup failures and through the cleanup that `keepWorktreeDecision`/`worktreeCmuxFinishCleanup`
    drive at the end; `req.WtCleanup` holds the force-exit cleanup for a fresh worktree.
  - `keepDetachedWorktree` returns `false` for a success unless `KeepWorktreeOnFailure` is set, and
    `fireAttemptStarted` returns `keep = wt.resumed` without an `AttemptStarted` hook, so a fresh
    foreground worktree is always removed today.
  - `IsDirtyAll` counts untracked files; `inspectPlanChanges` already separates the plan file from other
    dirty paths (`pkg/git/service.go:870`).

## Context (from discovery)
- Files/components involved:
  - `cmd/loopai/main.go`: review-only startup (`checkReviewDiffRange` call sites), `prepareSelectedPlanBranch`,
    `runWithWorktree` and its cleanups
  - `cmd/loopai/detach_worktree.go`: `keepWorktreeDecision`, `keepDetachedWorktree`, `worktreeUnsettled`
  - `pkg/git/service.go`: `prepareBranchPlan`, `inspectPlanChanges`, `formatDirtyFiles`, `IsDirtyAll`
  - `pkg/input/input.go`: `AskYesNo`
  - tests: `cmd/loopai/main_test.go` and the per-file tests beside new code, `pkg/git/service_test.go`
  - docs: `README.md`, `llms.txt`, `CLAUDE.md`, and any `docs/` page describing `--review`,
    `--external-only`, or `--worktree` cleanup
- Related patterns found: startup guards live in `cmd/loopai` (`worktreeIgnoredWarning`,
  `checkReviewDiffRange`); interactive prompts take `stdin`/`stdout` parameters so tests drive them; the
  detached keep decision is memoized and checked while the worktree still exists.
- Dependencies identified: none new.

## Development Approach
- **Testing approach**: Regular (code first, then tests)
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
- Maintain backward compatibility

## Testing Strategy
- **Unit tests**: required for every task (see Development Approach above); Git behavior is tested
  against real temporary repositories created under `t.TempDir()`, and prompts with injected
  `stdin`/`stdout`
- **E2E tests**: the dashboard Playwright suite is unaffected

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): tasks achievable within this codebase - code changes, tests, documentation updates
- **Post-Completion** (no checkboxes): items requiring external action - manual testing, changes in consuming projects, deployment configs, third-party verifications
- **Checkbox placement**: Checkboxes belong only in Task sections (`### Task N:` or `### Iteration N:`). Do not put checkboxes in Success criteria, Overview, Context, Decisions, or Decision Log — they cause extra loop iterations.

## Implementation Steps

### Task 1: List dirty paths other than the plan
- [ ] add an exported `git.Service` method (for example `DirtyFilesExcept(planFile string) ([]string, error)`)
      built on `inspectPlanChanges`, returning dirty and untracked paths other than the selected plan
      file (and nothing for an empty plan path), with a godoc saying it backs the dirty-checkout guard
- [ ] write tests in `pkg/git/service_test.go`: a clean tree returns none; a modified tracked file, a
      staged file, and an untracked file are listed; a dirty plan file alone returns none; ignored files
      are not listed
- [ ] run `go test ./pkg/git/...` - must pass before task 2

### Task 2: Guard review-only modes and feature-branch resumes in a dirty checkout
- [ ] add a `cmd/loopai` helper (for example `confirmDirtyCheckout(ctx, gitSvc, planFile, reason, interactive, stdin, stdout, warn)`)
      that does nothing on a clean list, otherwise prints the dirty paths (bounded, through the existing
      `formatDirtyFiles` style) with the whole-file staging warning, then asks `Continue anyway?` with
      `input.AskYesNo` when interactive and returns an error such as `aborted: dirty checkout` when
      declined; non-interactive prints the warning and continues
- [ ] call it for `ModeReview` and `ModeCodexOnly` right after both `checkReviewDiffRange` call sites,
      before executor dependency checks, with the selected plan file (empty under plain `--review`)
- [ ] call it in the single-plan non-chain path of `prepareSelectedPlanBranch` when the current branch is
      not the default branch, so `CreateBranchForPlan` will short-circuit; chains and worktree mode are
      untouched (they already create their own branch or checkout)
- [ ] decide interactivity as stdin being a terminal and `req.NonInteractive` false, and keep the helper
      testable by passing that decision in
- [ ] write tests: clean checkout asks nothing; dirty checkout under `--review` and `--external-only`
      prompts and proceeds on `y`, aborts on `n` and on empty input; non-interactive prints the list and
      proceeds; a dirty plan file alone does not trigger it; a non-worktree run on its feature branch is
      guarded while a run from the default branch keeps the existing `cannot create branch` error
- [ ] run `go test ./cmd/loopai -run 'Dirty|Review|PrepareSelectedPlanBranch'` - must pass before task 3

### Task 3: Keep a dirty fresh worktree of a foreground --worktree run
- [ ] in `cmd/loopai/detach_worktree.go`, make the end-of-run decision keep a fresh worktree whenever
      `worktreeUnsettled` reports it unsettled, whatever the outcome and without `KeepWorktreeOnFailure`;
      the detached and resumed behavior stays as it is, and the decision stays memoized and taken while
      the worktree exists
- [ ] in `runWithWorktree`, apply the same rule to the setup-failure path after the worktree exists and
      to the force-exit cleanup in `req.WtCleanup`, so an interrupted run never force-removes uncommitted
      work; record the kept path through `req.Outcome.recordWorktreeKept`
- [ ] when a foreground worktree is kept for this reason, print one message naming the path, that
      rerunning the same command resumes in it, and that `git worktree remove <path>` discards it
- [ ] write tests: a clean fresh worktree is still removed after success and after failure; a dirty one
      (modified file, untracked file, merge in progress) is kept after failure, after success, and on the
      force-exit cleanup, with the message printed; a rerun resumes in the kept worktree; detached-run keep
      tests stay green unchanged
- [ ] run `go test ./cmd/loopai -run 'Worktree|Keep|Detach'` - must pass before task 4

### Task 4: Verify acceptance criteria
- [ ] verify both backlog problems are fixed as described in the Overview
- [ ] run `make test </dev/null` (full suite)
- [ ] run `make lint` - all issues must be fixed
- [ ] run `GOOS=windows GOARCH=amd64 go build ./...`
- [ ] confirm `go test ./cmd/loopai -run TestSkillContract` passes without `-update` (no flag or key added)
- [ ] verify test coverage of the new helpers meets the project standard (80%+)

### Task 5: [Final] Update documentation and close the backlog entries
- [ ] `README.md` and `llms.txt`: describe the dirty-checkout question for `--review`, `--external-only`,
      and a run resumed on its own branch, and that a foreground `--worktree` run keeps a worktree holding
      uncommitted work and says where it is
- [ ] `CLAUDE.md`: add the guard to the review-only startup guards paragraph and the foreground keep rule
      to the worktree paragraph, and adjust the backlog-capture paragraph that calls a dirty tree "allowed
      and never gated" in review-only modes
- [ ] delete `docs/backlog/review-only-modes-commit-user-edits.md` and
      `docs/backlog/preserve-review-worktree-on-failure.md`, staged with `git rm` in the task commit

*Note: loopai automatically moves completed plans to `docs/plans/completed/`*

## Technical Details
- Guarded paths: `ModeReview`, `ModeCodexOnly`, and a non-worktree, non-chain `ModeFull`/`ModeTasksOnly`
  run whose current branch does not match the default branch. Not guarded: worktree mode, chains,
  detached runs (always worktree), and a run from the default branch, which keeps today's hard
  `cannot create branch ... uncommitted changes` error.
- Dirty set: `git status --porcelain` paths, untracked included and ignored excluded, minus the selected
  plan file.
- Prompt: `input.AskYesNo(ctx, "Continue anyway?", stdin, stdout)`, default no; interactive means stdin
  is a terminal and the request is not `NonInteractive`.
- Foreground keep rule: remove a fresh worktree only when `IsDirtyAll` is false and
  `OperationInProgress` is empty; any check error keeps it, as `worktreeUnsettled` already does.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- In a scratch repository, edit a tracked file, run `loopai --external-only`, and confirm the file list
  and the `Continue anyway?` question; answer `n` and confirm nothing ran.
- Start `loopai --worktree <plan>`, interrupt it during review after a fix is written but not committed,
  and confirm the worktree remains with the printed path and that rerunning the command resumes there.
