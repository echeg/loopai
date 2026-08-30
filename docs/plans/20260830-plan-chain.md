# Plan Chain: Sequential Execution of Multiple Plans

## Overview
- Allow passing several plan files in the positional argument, comma-separated: `loopai plan1.md,plan2.md,plan3.md`.
- Plans execute sequentially, each as a full normal run (tasks, reviews, finalize). Plan N+1 builds on plan N's result: its branch is cut from plan N's branch tip (stacked branches), so later plans see earlier plans' code.
- The chain stops on the first failure or user abort; completed plans keep their branches and archived plan files.
- After a successful chain the last branch contains all work; `--merge` of the last branch (or sequential merges) closes out.

## Decisions
- **Context**: user wants "do plan 1, then plan 2, ..." where later plans depend on earlier results.
- **Chosen approach**: stacked branches — plan N+1's worktree/branch is cut from plan N's branch tip instead of the source checkout's HEAD. The source checkout is never mutated between plans.
- **Rejected alternatives**:
  - Independent runs (each plan cut from source HEAD): rejected — plan 2 would not see plan 1's code.
  - Auto-merge between plans (merge plan N into source, cut plan N+1 from new HEAD): rejected — mutates the user's checkout mid-chain, merge conflicts mid-chain, much more machinery.
- **Failure semantics** (decided, not asked): stop the chain on the first failed plan AND on user abort. Note `executePlan` returns nil for `processor.ErrUserAborted` (cmd/loopai/main.go:1018-1022), so the chain must observe an explicit outcome signal (as `worktreeFinishState.succeeded` does), not the error value alone.
- **Verified facts**:
  - `PlanFile` is a single positional (cmd/loopai/main.go:90); surplus positionals land in `extraArgs` and are ignored outside close-out validation.
  - One plan's end-to-end unit is `executePlan` (main.go:882); worktree wrapper is `runWithWorktree` (main.go:1111); natural chain insertion point is a loop around `selectAndExecutePlan` inside `run` (main.go:463), reusing config/git/notify, recreating reporters and cleanup holders per plan.
  - `runPlanMode` (main.go:2571) already runs two stages in one process and hands cmux/orca reporters over between them (main.go:2739-2743) — the model for inter-plan transitions.
  - Worktree branches are currently cut from the source checkout's HEAD; branch reuse merges source HEAD into the branch. Stacking needs an explicit start-ref override.
  - No automatic merge exists; `--merge`/`--pr` are standalone close-out commands and already reject execution flags.
  - `--branch` override would collapse all chained plans into one branch and one progress file — must be rejected for chains.
  - `--serve` blocks in `keepDashboardAlive` after a run (main.go:809) and binds one port per `executePlan` — incompatible with a chain in v1.

## Context (from discovery)
- Files/components involved: `cmd/loopai/main.go` (arg parsing, `run`, `selectAndExecutePlan`, `executePlan`, `runWithWorktree`, `prepareWorktreeRunContext`, validation, cmux hand-off), `pkg/git/service.go` (worktree/branch creation, `EffectiveBranchName`), `pkg/plan` (selector, `ExtractBranchName`).
- Related patterns found: `runPlanMode` two-stage reporter hand-off; `worktreeFinishState` success signal; per-plan progress files keyed by plan basename.
- Dependencies identified: cmux workspace hand-off pre-validates `o.PlanFile` readability (`planFileHandOffRefusal`, main.go:3237) and derives the workspace name from it (main.go:3270) — both must understand the comma list.

## Development Approach
- **Testing approach**: Regular (code first, then tests in the same task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods and modified functions/methods
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change (`make test`, `make lint` at task boundaries)
- Maintain backward compatibility: a single plan argument without commas must behave byte-identically to today

## Testing Strategy
- **Unit tests**: required for every task; table-driven with testify; filesystem via `t.TempDir()`; git behavior via the existing local-repo test helpers in `pkg/git` and `cmd/loopai` tests
- **E2E tests**: the Playwright suite covers the dashboard only and is unaffected (`--serve` is rejected with chains); no e2e changes required

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code changes, tests, documentation updates in this repo
- **Post-Completion** (no checkboxes): manual smoke runs, external verifications

## Implementation Steps

### Task 1: Parse the comma-separated plan chain and validate flag combinations
- [x] split the positional plan argument on commas into `o.PlanFiles []string` (trimmed, empty entries rejected); keep `o.PlanFile` as the first entry so all existing single-plan code paths stay valid
- [x] treat a single entry exactly as today (no behavior change without commas)
- [x] validation for chains (len > 1): every listed file must exist, be a regular file, and be readable at startup (fail fast before any branch is created); duplicate entries rejected
- [x] reject incompatible flags with a chain: `--branch` (collapses branch names and progress files), `--serve` (dashboard blocks between plans and rebinds its port), `--plan` (already conflicts with a positional plan)
- [x] write tests for chain parsing (single, multiple, whitespace, empty entry, duplicates)
- [x] write tests for flag-combination rejections and for unchanged single-plan behavior
- [x] run tests - must pass before next task

### Task 2: Support cutting a worktree branch from an explicit start ref
- [x] extend the worktree/branch creation path (`pkg/git/service.go` and `prepareWorktreeRunContext` in cmd/loopai/main.go) with an optional start ref; default (empty) keeps today's "source checkout HEAD" behavior
- [x] chain callers pass `refs/heads/<previous-plan-branch>` as the start ref for plan N+1
- [x] branch-reuse synchronization for a chained plan merges the start ref (previous branch tip), not source HEAD
- [x] verify the plan file for plan N+1 reaches its worktree the same way a single run guarantees it today (worktrees carry committed files only); document the mechanism in code comments
- [x] write tests in pkg/git for start-ref worktree creation (default HEAD, explicit ref, missing ref error)
- [x] write tests for branch reuse against a start ref
- [x] run tests - must pass before next task

### Task 3: Chain loop in `run` with per-plan lifecycle
- [x] loop over `o.PlanFiles` around `selectAndExecutePlan` (cmd/loopai/main.go:463): reuse loaded config, git service, notifier, and selector; construct fresh orca `setupTitles` and cmux reporter per plan following the `runPlanMode` hand-off pattern (main.go:2739-2743)
- [x] thread the previous plan's effective branch name into the next iteration as the worktree start ref
- [x] stop the chain on the first failed plan; also stop on user abort by observing an explicit success signal (mirror `worktreeFinishState.succeeded`), since `executePlan` returns nil for `ErrUserAborted`
- [x] guarantee cwd restoration and progress-logger closure between plans (logger for plan N+1 must open only after plan N's is closed)
- [x] log a clear chain header per plan (`plan 2/3: <file>`) and a final chain summary line
- [x] `-c`/`--commit` applies to the first plan only (source auto-commit); later plans cut from branch tips and must not re-trigger it
- [x] write tests for chain iteration order, stop-on-failure, and stop-on-abort
- [x] write tests for reporter/logger lifecycle between plans (no double-open, cwd restored)
- [x] run tests - must pass before next task

### Task 4: Non-worktree chain stacking
- [x] with `use_worktree` disabled, after plan N the checkout sits on plan N's branch; for a chain, explicitly create plan N+1's branch from the current HEAD instead of relying on `prepareBranchPlan`'s early return ("already on feature branch, caller should skip")
- [x] keep the existing single-plan early-return behavior untouched for non-chain runs
- [x] verify clean-tree expectations between chained plans (each plan's phases commit their work; a dirty tree between plans is a chain error with a clear message)
- [x] write tests for non-worktree chaining (branch N+1 created from branch N tip, single-plan path unchanged)
- [x] write tests for the dirty-tree-between-plans error
- [x] run tests - must pass before next task

### Task 5: cmux hand-off and status integration for chains
- [x] `planFileHandOffRefusal` (main.go:3237) validates every file in the comma list before spawning a workspace
- [x] `cmuxWorkspaceName` (main.go:3270) derives the workspace name from the first plan (documented behavior)
- [x] per-plan cmux reporters overwrite the previous plan's pill as runs progress; the final pill reflects the last executed plan (done or failed) — verify `Finish`/`Stop` ordering across the chain
- [x] write tests for multi-plan hand-off refusal and workspace naming
- [x] run tests - must pass before next task

### Task 6: Verify acceptance criteria
- [x] verify all requirements from Overview are implemented (comma syntax, sequential stacked execution, stop on failure/abort, single-plan behavior unchanged)
- [x] verify edge cases: nonexistent second plan fails before any run; abort during plan 1 leaves no plan 2 artifacts; plan archival and progress files correct per plan
- [x] run full test suite (`make test`)
- [x] run linter (`make lint`) - all issues must be fixed
- [x] cross-compile check for platform-sensitive code (`GOOS=windows GOARCH=amd64 go build ./...`)
- [x] verify test coverage of new code meets project standard

### Task 7: [Final] Update documentation
- [x] update README.md: chain syntax, stacked-branch semantics, close-out guidance (merge the last branch), rejected flag combinations
- [x] update llms.txt with the chain usage line
- [x] update CLAUDE.md architecture notes (worktree start-ref override, chain loop location)
- [x] update embedded config/help text if the positional arg description mentions a single plan

## Technical Details
- CLI: positional arg split on `,`; `o.PlanFiles []string` with `o.PlanFile == o.PlanFiles[0]`; no new flags in v1.
- Chain state threaded between iterations: previous plan's effective branch name (after `--branch` rejection, always derived from the plan filename via `EffectiveBranchName`).
- Worktree start ref: plan 1 uses today's default (source HEAD); plan N+1 uses `refs/heads/<branch N>`. Branch reuse for a chained plan synchronizes against the start ref.
- Outcome signal: chain continues only on an explicit per-plan success (pattern of `worktreeFinishState.succeeded`), treating `ErrUserAborted`-with-nil-error as a stop.
- Exit code: unchanged binary contract — first failure prints `error: %v` and exits 1; earlier completed plans remain archived with their branches intact.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- Smoke-run a 2-plan chain in the `make e2e-prep` toy repository (worktree mode and non-worktree mode): verify branch 2 is cut from branch 1's tip, both plans archived, `--merge` of the last branch lands everything
- Abort a chain with Ctrl+C during plan 1 and verify plan 2 never starts and no stray worktree/pill remains
